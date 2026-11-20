package memvfs

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig/internal/memextract"
	"github.com/zeokeel/zeokeel-rig/memoryvfs"
)

// overviewMessages 是会话 overview（Working Memory）里保留的消息条数。
const overviewMessages = 8

// Capture 幂等追加消息到 pending 队列：同一条消息（role|content|at）只存一次。
// At 为零时使用当前时间；去重键用原始 At，因此"零时间戳"的消息按 role|content 去重。
func (s *Store) Capture(ctx context.Context, sessionID string, messages ...memoryvfs.Message) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if err := memoryvfs.ValidateSessionID(sessionID); err != nil {
		return err
	}
	if err := s.lock(); err != nil {
		return err
	}
	defer s.mu.Unlock()
	pending, err := s.readPendingLocked(sessionID)
	if err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(pending)+len(messages))
	for _, message := range pending {
		seen[messageKey(message)] = struct{}{}
	}
	for _, message := range messages {
		key := messageKey(message)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		stored := message
		stored.Key = key
		if stored.At.IsZero() {
			stored.At = s.now()
		}
		pending = append(pending, stored)
	}
	return s.writePendingLocked(sessionID, pending)
}

// messageKey 返回幂等键：调用方显式给的 Key 优先；否则用 role|content|原始 At
// 的 sha256（零值 At 归一成空串，所以同一句零时间戳消息重复捕获仍然幂等）。
func messageKey(message memoryvfs.Message) string {
	if key := strings.TrimSpace(message.Key); key != "" {
		return key
	}
	return dedupeKey(message)
}

func dedupeKey(message memoryvfs.Message) string {
	at := ""
	if !message.At.IsZero() {
		at = message.At.UTC().Format(time.RFC3339Nano)
	}
	sum := sha256.Sum256([]byte(message.Role + "|" + message.Content + "|" + at))
	return hex.EncodeToString(sum[:])
}

// Pending 返回待提交状态。
func (s *Store) Pending(ctx context.Context, sessionID string) (memoryvfs.SessionState, error) {
	if err := ctxErr(ctx); err != nil {
		return memoryvfs.SessionState{}, err
	}
	if err := memoryvfs.ValidateSessionID(sessionID); err != nil {
		return memoryvfs.SessionState{}, err
	}
	if err := s.rlock(); err != nil {
		return memoryvfs.SessionState{}, err
	}
	defer s.mu.RUnlock()
	messages, err := s.readPendingLocked(sessionID)
	if err != nil {
		return memoryvfs.SessionState{}, err
	}
	state := memoryvfs.SessionState{ID: sessionID, Pending: len(messages), Updated: s.sessionUpdatedLocked(sessionID)}
	for _, message := range messages {
		state.Tokens += memoryvfs.EstimateTokens(message.Content)
	}
	if at, ok := s.meta.Checkpoints[sessionID]; ok {
		state.LastCommit = at
	}
	return state, nil
}

// Commit 归档 pending 消息并按 opts 抽取长期记忆。
//
// 失败时保留 pending（任何一步出错都不会静默成功）；成功后才清空。
func (s *Store) Commit(ctx context.Context, sessionID string, opts memoryvfs.CommitOptions) (memoryvfs.CommitResult, error) {
	if err := ctxErr(ctx); err != nil {
		return memoryvfs.CommitResult{}, err
	}
	if err := memoryvfs.ValidateSessionID(sessionID); err != nil {
		return memoryvfs.CommitResult{}, err
	}

	// 阶段一：取一份 pending 快照（不持锁调用抽取器，避免宿主回调死锁）。
	if err := s.rlock(); err != nil {
		return memoryvfs.CommitResult{}, err
	}
	messages, err := s.readPendingLocked(sessionID)
	s.mu.RUnlock()
	if err != nil {
		return memoryvfs.CommitResult{}, err
	}
	if len(messages) == 0 {
		return memoryvfs.CommitResult{}, nil
	}

	// 阶段二：抽取候选（失败立即返回，pending 原样保留）。
	var candidates []memoryvfs.Candidate
	if opts.Extract {
		extractor := s.opts.Extractor
		if extractor == nil {
			extractor = memextract.New()
		}
		budget := s.opts.CommitTokens
		if opts.TokenBudget > 0 {
			budget = opts.TokenBudget
		}
		extractInput := trimByTokenBudget(messages, budget)
		session := memoryvfs.Session{
			ID:       sessionID,
			Messages: extractInput,
			Started:  extractInput[0].At,
			Updated:  extractInput[len(extractInput)-1].At,
		}
		candidates, err = extractor.Extract(ctx, session)
		if err != nil {
			return memoryvfs.CommitResult{}, fmt.Errorf("memvfs: extract session %s: %w", sessionID, err)
		}
	}

	// 阶段三：归档/Remember/Working Memory 都走公开 Write/Remember（回调在锁外），
	// 最后再拿写锁清 pending。崩溃时会重复写归档（P2），但不丢消息。
	if err := s.lock(); err != nil {
		return memoryvfs.CommitResult{}, err
	}
	archiveURI, err := s.archiveURILocked(sessionID, messages[len(messages)-1].At)
	s.mu.Unlock()
	if err != nil {
		return memoryvfs.CommitResult{}, err
	}
	archive, err := s.Write(ctx, buildArchiveEntry(sessionID, messages, archiveURI))
	if err != nil {
		return memoryvfs.CommitResult{}, err
	}
	result := memoryvfs.CommitResult{Archive: archive.URI, Tokens: archive.Tokens}
	for _, candidate := range candidates {
		if opts.MinConfidence > 0 && candidate.Confidence < opts.MinConfidence {
			result.Skipped++
			continue
		}
		entry, ok := candidateEntry(sessionID, candidate)
		if !ok {
			result.Skipped++
			continue
		}
		_, preStatErr := s.Stat(ctx, entry.URI)
		existedBefore := preStatErr == nil
		written, rememberErr := s.Remember(ctx, entry)
		if rememberErr != nil {
			if errors.Is(rememberErr, memoryvfs.ErrQuotaExceeded) {
				result.Skipped++
				continue
			}
			return memoryvfs.CommitResult{}, rememberErr
		}
		if existedBefore {
			result.Merged = append(result.Merged, written.URI)
		} else {
			result.Written = append(result.Written, written.URI)
		}
	}
	if opts.WorkingMemory {
		working, buildErr := s.Write(ctx, buildWorkingMemoryEntry(sessionID, messages))
		if buildErr != nil {
			return memoryvfs.CommitResult{}, buildErr
		}
		result.Written = append(result.Written, working.URI)
	}

	if err := s.lock(); err != nil {
		return memoryvfs.CommitResult{}, err
	}
	defer s.mu.Unlock()
	current, err := s.readPendingLocked(sessionID)
	if err != nil {
		return memoryvfs.CommitResult{}, err
	}
	remainder := pendingRemainder(current, messages)
	s.meta.Checkpoints[sessionID] = s.now()
	if err := s.persistMetaLocked(); err != nil {
		return memoryvfs.CommitResult{}, err
	}
	if err := s.writePendingLocked(sessionID, remainder); err != nil {
		return memoryvfs.CommitResult{}, err
	}
	return result, nil
}

// buildWorkingMemoryEntry 构造会话 overview 条目（不落盘）。
func buildWorkingMemoryEntry(sessionID string, messages []memoryvfs.Message) memoryvfs.Entry {
	segment := sessionSegment(sessionID)
	uri, _ := memoryvfs.ParseURI(sessionsPrefix + segment + "/overview")
	var builder strings.Builder
	for index, message := range messages {
		if index >= overviewMessages {
			break
		}
		line, _ := memoryvfs.TruncateBytes(strings.TrimSpace(message.Content), 300)
		fmt.Fprintf(&builder, "- %s: %s\n", message.Role, line)
	}
	overview, _ := memoryvfs.TruncateBytes(builder.String(), memoryvfs.MaxContentBytes)
	return memoryvfs.Entry{
		URI:      uri,
		Kind:     memoryvfs.KindSession,
		Title:    "session " + segment,
		Summary:  fmt.Sprintf("session %s: %d messages", sessionID, len(messages)),
		Overview: overview,
		Content:  overview,
		Tags:     map[string]string{"session": segment},
		Source:   "session:" + sessionID,
	}
}

// trimByTokenBudget 从最新消息往前取，直到累计 token 达到预算；预算<=0 时原样
// 返回。归档条目始终保留完整转录，这个裁剪只作用于抽取器看到的输入。
func trimByTokenBudget(messages []memoryvfs.Message, budget int) []memoryvfs.Message {
	if budget <= 0 || len(messages) <= 1 {
		return messages
	}
	used := 0
	start := len(messages)
	for index := len(messages) - 1; index >= 0; index-- {
		used += memoryvfs.EstimateTokens(messages[index].Content)
		if used > budget && index < len(messages)-1 {
			break
		}
		start = index
	}
	return messages[start:]
}

// pendingRemainder 返回"已提交前缀之外"的消息，避免并发 Capture 的消息被误删。
func pendingRemainder(current, committed []memoryvfs.Message) []memoryvfs.Message {
	if len(current) < len(committed) {
		return nil
	}
	for index := range committed {
		if current[index].Role != committed[index].Role ||
			current[index].Content != committed[index].Content ||
			current[index].Key != committed[index].Key ||
			!current[index].At.Equal(committed[index].At) {
			return current
		}
	}
	return current[len(committed):]
}

// writeWorkingMemoryLocked 写会话 overview 条目（Working Memory 归档）。
func (s *Store) writeWorkingMemoryLocked(ctx context.Context, sessionID string, messages []memoryvfs.Message) (memoryvfs.Entry, error) {
	segment := sessionSegment(sessionID)
	uri, err := memoryvfs.ParseURI(sessionsPrefix + segment + "/overview")
	if err != nil {
		return memoryvfs.Entry{}, err
	}
	var builder strings.Builder
	for index, message := range messages {
		if index >= overviewMessages {
			break
		}
		line, _ := memoryvfs.TruncateBytes(strings.TrimSpace(message.Content), 300)
		fmt.Fprintf(&builder, "- %s: %s\n", message.Role, line)
	}
	overview, _ := memoryvfs.TruncateBytes(builder.String(), memoryvfs.MaxContentBytes)
	summary := fmt.Sprintf("session %s: %d messages", sessionID, len(messages))
	return s.writeLocked(ctx, memoryvfs.Entry{
		URI:      uri,
		Kind:     memoryvfs.KindSession,
		Title:    "session " + segment,
		Summary:  summary,
		Overview: overview,
		Content:  overview,
		Tags:     map[string]string{"session": segment},
		Source:   "session:" + sessionID,
	})
}

// buildArchiveEntry 把一批消息变成归档条目：L0=首条用户消息（截断），
// L1=前 N 条，L2=完整转录。
func buildArchiveEntry(sessionID string, messages []memoryvfs.Message, uri memoryvfs.URI) memoryvfs.Entry {
	var transcript strings.Builder
	for _, message := range messages {
		fmt.Fprintf(&transcript, "%s: %s\n", message.Role, strings.TrimSpace(message.Content))
	}
	content, _ := memoryvfs.TruncateBytes(transcript.String(), memoryvfs.MaxContentBytes)

	summary := ""
	for _, message := range messages {
		if strings.EqualFold(message.Role, "user") && strings.TrimSpace(message.Content) != "" {
			summary = strings.TrimSpace(message.Content)
			break
		}
	}
	if summary == "" {
		for _, message := range messages {
			if trimmed := strings.TrimSpace(message.Content); trimmed != "" {
				summary = trimmed
				break
			}
		}
	}
	if summary == "" {
		summary = fmt.Sprintf("session %s (%d messages)", sessionID, len(messages))
	}
	summary, _ = memoryvfs.TruncateBytes(memoryvfs.CollapseWhitespace(summary), memoryvfs.MaxSummaryBytes)

	var overview strings.Builder
	for index, message := range messages {
		if index >= overviewMessages {
			break
		}
		line, _ := memoryvfs.TruncateBytes(strings.TrimSpace(message.Content), 200)
		fmt.Fprintf(&overview, "- %s: %s\n", message.Role, line)
	}
	overviewText, _ := memoryvfs.TruncateBytes(overview.String(), memoryvfs.MaxOverviewBytes)

	segment := sessionSegment(sessionID)
	created := messages[0].At
	if created.IsZero() {
		created = messages[len(messages)-1].At
	}
	return memoryvfs.Entry{
		URI:      uri,
		Kind:     memoryvfs.KindSession,
		Title:    "session " + segment,
		Summary:  summary,
		Overview: overviewText,
		Content:  content,
		Tags:     map[string]string{"session": segment},
		Source:   "session:" + sessionID,
		Created:  created,
	}
}

// archiveURILocked 分配 mem://sessions/<id>/<unixnano>；固定时钟下若撞号就 +1ns。
func (s *Store) archiveURILocked(sessionID string, at time.Time) (memoryvfs.URI, error) {
	segment := sessionSegment(sessionID)
	nanos := at.UnixNano()
	if at.IsZero() {
		nanos = s.now().UnixNano()
	}
	for attempt := 0; attempt < 1024; attempt++ {
		uri, err := memoryvfs.ParseURI(fmt.Sprintf("%s%s/%d", sessionsPrefix, segment, nanos))
		if err != nil {
			return "", err
		}
		if _, exists := s.entries[uri]; !exists {
			return uri, nil
		}
		nanos++
	}
	return "", fmt.Errorf("%w: cannot allocate an archive uri for session %s", memoryvfs.ErrConflict, sessionID)
}

// sessionSegment 把会话 ID 收敛成合法的 URI 段（契约只允许 [a-z0-9._-]）。
func sessionSegment(sessionID string) string {
	lowered := strings.ToLower(strings.TrimSpace(sessionID))
	var builder strings.Builder
	lastDash := false
	for _, r := range lowered {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_':
			builder.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				builder.WriteByte('-')
				lastDash = true
			}
		}
	}
	segment := strings.Trim(builder.String(), "-.")
	if segment == "" {
		segment = "session"
	}
	if len(segment) > 64 {
		segment = segment[:64]
	}
	// 只有"清洗后与原文不同"时才追加哈希：`ws 1`/`ws-1`、`Foo`/`foo` 这类会
	// 收敛到同一段的 ID 必须仍然互相隔离；常见的安全 ID 保持可读路径不变。
	trimmed := strings.TrimSpace(sessionID)
	if segment != trimmed {
		sum := sha256.Sum256([]byte(sessionID))
		segment = segment + "-" + hex.EncodeToString(sum[:])[:8]
	}
	return segment
}

// candidateEntry 把抽取候选映射成确定性的记忆 URI（同内容重复提交会命中同一条）。
func candidateEntry(sessionID string, candidate memoryvfs.Candidate) (memoryvfs.Entry, bool) {
	kind := candidate.Kind
	if kind == "" {
		kind = memoryvfs.KindMemory
	}
	summary := strings.TrimSpace(candidate.Summary)
	content := strings.TrimSpace(candidate.Content)
	if summary == "" {
		summary = content
	}
	if summary == "" {
		return memoryvfs.Entry{}, false
	}
	if trimmed, truncated := memoryvfs.TruncateBytes(memoryvfs.CollapseWhitespace(summary), memoryvfs.MaxSummaryBytes); truncated || len(trimmed) > 0 {
		summary = trimmed
	}
	memType := strings.TrimSpace(candidate.Type)
	if kind == memoryvfs.KindMemory {
		memType = sanitizeSlug(memType)
		if memType == "" {
			memType = memoryvfs.TypeEvents
		}
	}
	slug := sanitizeSlug(candidate.Title)
	if slug == "" {
		slug = sanitizeSlug(summary)
	}
	if slug == "" {
		slug = "note"
	}
	if len(slug) > 40 {
		slug = slug[:40]
	}
	sum := sha256.Sum256([]byte(memType + "\x00" + summary + "\x00" + content))
	hash := hex.EncodeToString(sum[:])[:8]
	uri, err := memoryvfs.ParseURI(fmt.Sprintf("%suser/self/memories/%s/%s-%s", memoryvfs.Scheme, memType, slug, hash))
	if err != nil {
		return memoryvfs.Entry{}, false
	}
	links := make([]memoryvfs.URI, 0, len(candidate.Links)+1)
	links = append(links, candidate.Links...)
	entry := memoryvfs.Entry{
		URI:     uri,
		Kind:    kind,
		Type:    memType,
		Title:   strings.TrimSpace(candidate.Title),
		Summary: summary,
		Content: content,
		Tags:    cloneTags(candidate.Tags),
		Links:   links,
		Source:  candidate.Source,
	}
	if entry.Source == "" {
		entry.Source = "session:" + sessionID
	}
	if entry.Tags == nil {
		entry.Tags = map[string]string{}
	}
	entry.Tags["session"] = sessionSegment(sessionID)
	return entry, true
}

func cloneTags(tags map[string]string) map[string]string {
	if tags == nil {
		return nil
	}
	out := make(map[string]string, len(tags))
	for key, value := range tags {
		out[key] = value
	}
	return out
}

// sanitizeSlug 把任意文本收敛成 [a-z0-9-] 的短 slug。
func sanitizeSlug(text string) string {
	lowered := strings.ToLower(strings.TrimSpace(text))
	var builder strings.Builder
	lastDash := false
	for _, r := range lowered {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			builder.WriteRune(r)
			lastDash = false
		default:
			if builder.Len() > 0 && !lastDash {
				builder.WriteByte('-')
				lastDash = true
			}
		}
		if builder.Len() >= 48 {
			break
		}
	}
	return strings.Trim(builder.String(), "-")
}

// ---------------------------------------------------------------------------
// pending.jsonl
// ---------------------------------------------------------------------------

func (s *Store) pendingPath(sessionID string) string {
	return filepath.Join(s.dir, sessionsDirName, sessionID, "pending.jsonl")
}

func (s *Store) readPendingLocked(sessionID string) ([]memoryvfs.Message, error) {
	if s.dir == "" {
		return append([]memoryvfs.Message(nil), s.pending[sessionID]...), nil
	}
	data, err := os.ReadFile(s.pendingPath(sessionID))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("memvfs: read pending for %s: %w", sessionID, err)
	}
	messages := make([]memoryvfs.Message, 0, 8)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), memoryvfs.MaxContentBytes+1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var message memoryvfs.Message
		if err := json.Unmarshal(line, &message); err != nil {
			return nil, fmt.Errorf("memvfs: corrupt pending line for %s: %w", sessionID, err)
		}
		messages = append(messages, message)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("memvfs: scan pending for %s: %w", sessionID, err)
	}
	return messages, nil
}

func (s *Store) writePendingLocked(sessionID string, messages []memoryvfs.Message) error {
	if s.dir == "" {
		if len(messages) == 0 {
			delete(s.pending, sessionID)
			return nil
		}
		s.pending[sessionID] = append([]memoryvfs.Message(nil), messages...)
		return nil
	}
	path := s.pendingPath(sessionID)
	if len(messages) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("memvfs: clear pending for %s: %w", sessionID, err)
		}
		_ = syncDir(filepath.Dir(path))
		return nil
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	for _, message := range messages {
		if err := encoder.Encode(message); err != nil {
			return fmt.Errorf("memvfs: encode pending for %s: %w", sessionID, err)
		}
	}
	return writeFileAtomic(path, buffer.Bytes())
}

func (s *Store) sessionIDsLocked() []string {
	if s.dir == "" {
		ids := make([]string, 0, len(s.pending))
		for id := range s.pending {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		return ids
	}
	entries, err := os.ReadDir(filepath.Join(s.dir, sessionsDirName))
	if err != nil {
		return nil
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			ids = append(ids, entry.Name())
		}
	}
	sort.Strings(ids)
	return ids
}

func (s *Store) sessionUpdatedLocked(sessionID string) time.Time {
	if s.dir == "" {
		messages := s.pending[sessionID]
		if len(messages) > 0 {
			return messages[len(messages)-1].At
		}
		if at, ok := s.meta.Checkpoints[sessionID]; ok {
			return at
		}
		return time.Time{}
	}
	if info, err := os.Stat(s.pendingPath(sessionID)); err == nil {
		return info.ModTime().UTC()
	}
	if at, ok := s.meta.Checkpoints[sessionID]; ok {
		return at
	}
	return time.Time{}
}

func (s *Store) persistMetaLocked() error {
	if s.dir == "" {
		return nil
	}
	s.meta.Version = metaVersion
	return writeJSONAtomic(filepath.Join(s.dir, metaFileName), s.meta)
}
