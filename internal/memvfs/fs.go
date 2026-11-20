package memvfs

import (
	"context"
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

// ---------------------------------------------------------------------------
// 读
// ---------------------------------------------------------------------------

// Stat 返回条目或目录的元数据。目录的 Freshness 按当前子项实时计算。
func (s *Store) Stat(ctx context.Context, uri memoryvfs.URI) (memoryvfs.Entry, error) {
	if err := ctxErr(ctx); err != nil {
		return memoryvfs.Entry{}, err
	}
	if err := s.rlock(); err != nil {
		return memoryvfs.Entry{}, err
	}
	defer s.mu.RUnlock()
	parsed, err := memoryvfs.ParseURI(string(uri))
	if err != nil {
		return memoryvfs.Entry{}, err
	}
	return s.statLocked(parsed)
}

func (s *Store) statLocked(uri memoryvfs.URI) (memoryvfs.Entry, error) {
	if uri == memoryvfs.RootURI {
		entry := memoryvfs.Entry{
			URI:     memoryvfs.RootURI,
			Kind:    memoryvfs.KindDir,
			Title:   "root",
			Summary: "memory root",
			Updated: s.now(),
		}
		entry.Freshness = s.freshnessLocked(uri)
		return entry, nil
	}
	if entry, ok := s.entries[uri]; ok {
		return entry.Clone(), nil
	}
	if dir, ok := s.dirs[uri]; ok {
		out := dir.Clone()
		out.Freshness = s.freshnessViewLocked(uri, dir.Freshness)
		if strings.TrimSpace(out.Summary) == "" {
			out.Summary, out.Overview = s.autoDirDescriptionLocked(uri)
		}
		return out, nil
	}
	return memoryvfs.Entry{}, fmt.Errorf("%w: %s", memoryvfs.ErrNotFound, uri)
}

// List 只返回直接子项。目录项带 sidecar 的 Summary/Overview/Freshness。
func (s *Store) List(ctx context.Context, uri memoryvfs.URI) ([]memoryvfs.Entry, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	if err := s.rlock(); err != nil {
		return nil, err
	}
	defer s.mu.RUnlock()
	parsed, err := memoryvfs.ParseURI(string(uri))
	if err != nil {
		return nil, err
	}
	if parsed != memoryvfs.RootURI {
		if _, ok := s.dirs[parsed]; !ok {
			if _, ok := s.entries[parsed]; ok {
				return nil, fmt.Errorf("%w: %s is an entry, not a directory", memoryvfs.ErrInvalidEntry, parsed)
			}
			return nil, fmt.Errorf("%w: %s", memoryvfs.ErrNotFound, parsed)
		}
	}
	return s.childrenLocked(parsed), nil
}

// Tree 递归返回子项；depth<=0 表示不限深度。
func (s *Store) Tree(ctx context.Context, uri memoryvfs.URI, depth int) ([]memoryvfs.Entry, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	if err := s.rlock(); err != nil {
		return nil, err
	}
	defer s.mu.RUnlock()
	parsed, err := memoryvfs.ParseURI(string(uri))
	if err != nil {
		return nil, err
	}
	if parsed != memoryvfs.RootURI {
		if _, ok := s.dirs[parsed]; !ok {
			if _, ok := s.entries[parsed]; ok {
				return nil, fmt.Errorf("%w: %s is an entry, not a directory", memoryvfs.ErrInvalidEntry, parsed)
			}
			return nil, fmt.Errorf("%w: %s", memoryvfs.ErrNotFound, parsed)
		}
	}
	var out []memoryvfs.Entry
	var walk func(dir memoryvfs.URI, level int)
	walk = func(dir memoryvfs.URI, level int) {
		for _, child := range s.childrenLocked(dir) {
			out = append(out, child)
			if child.Kind == memoryvfs.KindDir {
				if depth <= 0 || level < depth {
					walk(child.URI, level+1)
				}
			}
		}
	}
	walk(parsed, 1)
	return out, nil
}

// Read 按层级返回条目：L0 只有 Summary，L1 是 Summary+Overview，L2 是全量。
func (s *Store) Read(ctx context.Context, uri memoryvfs.URI, level memoryvfs.Level) (memoryvfs.Entry, error) {
	if err := ctxErr(ctx); err != nil {
		return memoryvfs.Entry{}, err
	}
	if err := s.rlock(); err != nil {
		return memoryvfs.Entry{}, err
	}
	defer s.mu.RUnlock()
	parsed, err := memoryvfs.ParseURI(string(uri))
	if err != nil {
		return memoryvfs.Entry{}, err
	}
	entry, err := s.statLocked(parsed)
	if err != nil {
		return memoryvfs.Entry{}, err
	}
	return trimToLevel(entry, level), nil
}

// trimToLevel 按层级裁剪字段。
func trimToLevel(entry memoryvfs.Entry, level memoryvfs.Level) memoryvfs.Entry {
	out := entry.Clone()
	switch level {
	case memoryvfs.Level0:
		out.Overview = ""
		out.Content = ""
	case memoryvfs.Level1:
		out.Content = ""
	case memoryvfs.Level2, memoryvfs.LevelAny:
	default:
	}
	return out
}

// childrenLocked 返回直接子项（目录在前，各自按 URI 升序）。
func (s *Store) childrenLocked(dir memoryvfs.URI) []memoryvfs.Entry {
	children := make([]memoryvfs.Entry, 0, 8)
	for uri, entry := range s.entries {
		if uri.Parent() == dir {
			children = append(children, entry.Clone())
		}
	}
	for uri, entry := range s.dirs {
		if uri == memoryvfs.RootURI || uri.Parent() != dir {
			continue
		}
		child := entry.Clone()
		child.Freshness = s.freshnessViewLocked(uri, entry.Freshness)
		if strings.TrimSpace(child.Summary) == "" {
			child.Summary, child.Overview = s.autoDirDescriptionLocked(uri)
		}
		children = append(children, child)
	}
	sort.SliceStable(children, func(a, b int) bool {
		if children[a].Kind == children[b].Kind {
			return children[a].URI < children[b].URI
		}
		return children[a].Kind == memoryvfs.KindDir
	})
	return children
}

// freshnessLocked 计算目录的直接子项覆盖情况：Total=直接子项数，
// Sampled=min(32,total)。PendingChanges 由调用方决定（实时视图里保留存值）。
func (s *Store) freshnessLocked(dir memoryvfs.URI) *memoryvfs.Freshness {
	total := 0
	for uri := range s.entries {
		if uri.Parent() == dir {
			total++
		}
	}
	for uri := range s.dirs {
		if uri != memoryvfs.RootURI && uri.Parent() == dir {
			total++
		}
	}
	sampled := total
	if sampled > sampleLimit {
		sampled = sampleLimit
	}
	return &memoryvfs.Freshness{TotalEntries: total, SampledEntries: sampled, Updated: s.now()}
}

// freshnessViewLocked 是 freshnessLocked 的"读视图"：子项数实时计算，
// PendingChanges 保留 sidecar 里累计的值（Stat/List 不能把它清零）。
func (s *Store) freshnessViewLocked(dir memoryvfs.URI, stored *memoryvfs.Freshness) *memoryvfs.Freshness {
	view := s.freshnessLocked(dir)
	if stored != nil {
		view.PendingChanges = stored.PendingChanges
	}
	return view
}

// autoDirDescriptionLocked 生成目录的派生 L0/L1（不递归，避免描述互相依赖）。
func (s *Store) autoDirDescriptionLocked(dir memoryvfs.URI) (string, string) {
	total := 0
	lines := make([]string, 0, sampleLimit)
	add := func(uri memoryvfs.URI, summary string) {
		total++
		if len(lines) >= sampleLimit {
			return
		}
		label := strings.TrimSpace(summary)
		if label == "" {
			label = string(uri.Base())
		}
		lines = append(lines, "- "+string(uri)+": "+label)
	}
	for uri, entry := range s.entries {
		if uri.Parent() == dir {
			add(uri, entry.Summary)
		}
	}
	for uri, entry := range s.dirs {
		if uri != memoryvfs.RootURI && uri.Parent() == dir {
			add(uri, entry.Summary)
		}
	}
	summary := fmt.Sprintf("%d entries", total)
	if total == 0 {
		summary = "empty directory"
	}
	overview, _ := memoryvfs.TruncateBytes(strings.Join(lines, "\n"), memoryvfs.MaxOverviewBytes)
	return summary, overview
}

// refreshDirLocked 重建目录 sidecar 的 freshness（PendingChanges 清零）与派生描述。
func (s *Store) refreshDirLocked(dir memoryvfs.URI) error {
	if dir == memoryvfs.RootURI {
		return nil
	}
	entry, ok := s.dirs[dir]
	if !ok {
		entry = memoryvfs.Entry{URI: dir, Kind: memoryvfs.KindDir, Title: dirTitle(dir)}
	}
	if entry.Kind != memoryvfs.KindDir {
		entry.Kind = memoryvfs.KindDir
	}
	entry.Updated = s.now()
	if entry.Created.IsZero() {
		entry.Created = entry.Updated
	}
	entry.Freshness = s.freshnessLocked(dir)
	if isAutoDir(entry) {
		entry.Summary, entry.Overview = s.autoDirDescriptionLocked(dir)
	}
	s.dirs[dir] = entry
	return s.persistDirLocked(entry)
}

// touchParentsLocked 在子项变更后刷新所有祖先目录：重算 freshness、PendingChanges+1。
func (s *Store) touchParentsLocked(uri memoryvfs.URI) error {
	for parent := uri.Parent(); parent != memoryvfs.RootURI; parent = parent.Parent() {
		entry, ok := s.dirs[parent]
		if !ok {
			entry = memoryvfs.Entry{
				URI:   parent,
				Kind:  memoryvfs.KindDir,
				Title: dirTitle(parent),
				Tags:  map[string]string{autoDirTag: "1"},
			}
		}
		if entry.Tags == nil {
			entry.Tags = map[string]string{}
		}
		entry.Updated = s.now()
		if entry.Created.IsZero() {
			entry.Created = entry.Updated
		}
		freshness := s.freshnessLocked(parent)
		pending := 0
		if entry.Freshness != nil {
			pending = entry.Freshness.PendingChanges
		}
		freshness.PendingChanges = pending + 1
		entry.Freshness = freshness
		if isAutoDir(entry) {
			entry.Summary, entry.Overview = s.autoDirDescriptionLocked(parent)
		}
		s.dirs[parent] = entry
		if err := s.persistDirLocked(entry); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 写
// ---------------------------------------------------------------------------

// Write 覆盖或新建条目：规整字段、补 L0/L1、检查配额、递增版本、刷新父目录。
func (s *Store) Write(ctx context.Context, entry memoryvfs.Entry) (memoryvfs.Entry, error) {
	if err := ctxErr(ctx); err != nil {
		return memoryvfs.Entry{}, err
	}
	if entry.Kind != memoryvfs.KindDir && strings.TrimSpace(entry.Summary) == "" && strings.TrimSpace(entry.Content) == "" {
		return memoryvfs.Entry{}, fmt.Errorf("%w: entry %s", memoryvfs.ErrSummaryRequired, entry.URI)
	}
	// 宿主回调（Summarizer/Embedder）可能在内部回读同一个 Store；必须在拿锁之前
	// 调用，否则 RWMutex 不可重入会硬死锁。
	entry = s.prepareEntry(ctx, entry)
	vector := s.embedEntry(ctx, entry)
	if err := s.lock(); err != nil {
		return memoryvfs.Entry{}, err
	}
	defer s.mu.Unlock()
	return s.writeLockedPrepared(ctx, entry, vector)
}

// embedEntry 在锁外算向量；没有 Embedder/失败时返回 nil（写入后不更新向量）。
func (s *Store) embedEntry(ctx context.Context, entry memoryvfs.Entry) []float32 {
	if s.opts.Embedder == nil || s.dimension <= 0 {
		return nil
	}
	text := strings.TrimSpace(s.embedText(entry))
	if text == "" {
		return nil
	}
	vectors, err := s.opts.Embedder.Embed(ctx, []string{text})
	if err != nil || len(vectors) != 1 || len(vectors[0]) != s.dimension {
		return nil
	}
	return append([]float32(nil), vectors[0]...)
}

func (s *Store) writeLocked(ctx context.Context, entry memoryvfs.Entry) (memoryvfs.Entry, error) {
	entry = s.prepareEntry(ctx, entry)
	return s.writeLockedPrepared(ctx, entry, nil)
}

func (s *Store) writeLockedPrepared(ctx context.Context, entry memoryvfs.Entry, vector []float32) (memoryvfs.Entry, error) {
	uri, err := memoryvfs.ParseURI(string(entry.URI))
	if err != nil {
		return memoryvfs.Entry{}, err
	}
	if _, err := relativeURIPath(uri); err != nil {
		return memoryvfs.Entry{}, fmt.Errorf("%w: cannot write %s", memoryvfs.ErrInvalidEntry, uri)
	}
	if _, isDir := s.dirs[uri]; isDir {
		return memoryvfs.Entry{}, fmt.Errorf("%w: %s is a directory", memoryvfs.ErrInvalidEntry, uri)
	}
	if entry.Kind == memoryvfs.KindDir {
		return memoryvfs.Entry{}, fmt.Errorf("%w: use Mkdir to create the directory %s", memoryvfs.ErrInvalidEntry, uri)
	}
	if err := s.checkDirChainLocked(uri); err != nil {
		return memoryvfs.Entry{}, err
	}
	existing, exists := s.entries[uri]
	entry.URI = uri
	if entry.Kind != memoryvfs.KindDir && strings.TrimSpace(entry.Summary) == "" && strings.TrimSpace(entry.Content) == "" {
		return memoryvfs.Entry{}, fmt.Errorf("%w: entry %s", memoryvfs.ErrSummaryRequired, uri)
	}
	if entry.Kind == "" {
		switch {
		case exists && existing.Kind != "":
			entry.Kind = existing.Kind
		case entry.Type != "":
			entry.Kind = memoryvfs.KindMemory
		default:
			entry.Kind = memoryvfs.KindResource
		}
	}
	if exists {
		entry.Created = existing.Created
		entry.Version = existing.Version + 1
		if existing.Pinned {
			entry.Pinned = true
		}
		if entry.LastAccess == nil {
			entry.LastAccess = existing.LastAccess
		}
		if entry.Updated.IsZero() || !entry.Updated.After(existing.Updated) {
			// 覆盖写没有显式给 Updated 时，NormalizeEntry 会退化成 Created（可能
			// 早于旧版本），所以这里显式推进到当前时间。
			entry.Updated = s.now()
		}
	} else {
		entry.Version = 1
	}
	if err := memoryvfs.ValidateEntry(entry); err != nil {
		return entry, err
	}
	if err := s.checkQuotaLocked(entry, exists); err != nil {
		return entry, err
	}
	if err := s.persistEntryLocked(entry); err != nil {
		return entry, err
	}
	stored := entry.Clone()
	s.entries[uri] = stored
	s.index.Put(stored)
	if vector != nil {
		s.vectors[uri] = vector
		s.index.SetVector(uri, vector)
	} else {
		s.embedLocked(ctx, stored)
	}
	if err := s.touchParentsLocked(uri); err != nil {
		return stored.Clone(), err
	}
	return stored.Clone(), nil
}

// prepareEntry 规整条目并补齐缺失的 L0/L1（Summarizer 优先，确定性摘要兜底）。
func (s *Store) prepareEntry(ctx context.Context, entry memoryvfs.Entry) memoryvfs.Entry {
	entry = memoryvfs.NormalizeEntry(entry, s.opts.Now)
	if entry.Kind == memoryvfs.KindDir {
		return entry
	}
	needSummary := strings.TrimSpace(entry.Summary) == ""
	needOverview := strings.TrimSpace(entry.Overview) == ""
	if !needSummary && !needOverview {
		return entry
	}
	summary, overview := "", ""
	if s.opts.Summarizer != nil {
		generated, generatedOverview, err := s.opts.Summarizer.Summarize(ctx, entry)
		if err == nil {
			summary, overview = generated, generatedOverview
		}
	}
	fallbackSummary, fallbackOverview := memextract.Summarize(entry)
	if strings.TrimSpace(summary) == "" {
		summary = fallbackSummary
	}
	if strings.TrimSpace(overview) == "" {
		overview = fallbackOverview
	}
	if needSummary && strings.TrimSpace(summary) != "" {
		if trimmed, _ := memoryvfs.TruncateBytes(memoryvfs.CollapseWhitespace(summary), memoryvfs.MaxSummaryBytes); trimmed != "" {
			entry.Summary = trimmed
		}
	}
	if needOverview && strings.TrimSpace(overview) != "" {
		if trimmed, _ := memoryvfs.TruncateBytes(overview, memoryvfs.MaxOverviewBytes); trimmed != "" {
			entry.Overview = trimmed
		}
	}
	entry.Tokens = entry.TokenEstimate(memoryvfs.Level2)
	return entry
}

// checkDirChainLocked 拒绝"在条目下面挂子项"（例如 mem://a 是条目时写 mem://a/b）：
// 那会让同一 URI 同时是条目和目录，语义无法表达。
func (s *Store) checkDirChainLocked(uri memoryvfs.URI) error {
	for parent := uri.Parent(); parent != memoryvfs.RootURI; parent = parent.Parent() {
		if _, ok := s.entries[parent]; ok {
			return fmt.Errorf("%w: %s is an entry, so %s cannot be nested under it", memoryvfs.ErrInvalidEntry, parent, uri)
		}
	}
	return nil
}

func (s *Store) checkQuotaLocked(entry memoryvfs.Entry, exists bool) error {
	// 会话归档/工作记忆是系统条目：它们必须永远能落盘，否则配额打满时 Commit
	// 会永久失败、pending 永远排不空（记忆采集事实性瘫痪）。它们由 GC 回收。
	if entry.Kind == memoryvfs.KindSession {
		return nil
	}
	entries := 0
	bytes := int64(entrySize(entry))
	for _, existing := range s.entries {
		if existing.Kind == memoryvfs.KindSession {
			continue
		}
		entries++
		if existing.URI == entry.URI {
			bytes -= int64(entrySize(existing))
			continue
		}
		bytes += int64(entrySize(existing))
	}
	if limit := s.opts.MaxEntries; limit > 0 && !exists && entries >= limit {
		return fmt.Errorf("%w: %d entries would exceed the %d entry limit", memoryvfs.ErrQuotaExceeded, entries+1, limit)
	}
	if limit := s.opts.MaxBytes; limit > 0 && bytes > limit {
		return fmt.Errorf("%w: %d bytes would exceed the %d byte limit", memoryvfs.ErrQuotaExceeded, bytes, limit)
	}
	return nil
}

// Append 追加正文到已有条目（不存在则 ErrNotFound），并刷新 Tokens 与版本。
//
// 没有 Embedder 时直接在写锁内 RMW（无宿主回调，最快且不丢并发更新）；注入
// Embedder 时把 prepare/embed 放到锁外，再用写锁内的版本 CAS 防丢更新。
func (s *Store) Append(ctx context.Context, uri memoryvfs.URI, text string) (memoryvfs.Entry, error) {
	if err := ctxErr(ctx); err != nil {
		return memoryvfs.Entry{}, err
	}
	parsed, err := memoryvfs.ParseURI(string(uri))
	if err != nil {
		return memoryvfs.Entry{}, err
	}
	if strings.TrimSpace(text) == "" {
		return s.Read(ctx, parsed, memoryvfs.Level2)
	}
	if s.opts.Embedder == nil {
		if err := s.lock(); err != nil {
			return memoryvfs.Entry{}, err
		}
		defer s.mu.Unlock()
		existing, ok := s.entries[parsed]
		if !ok {
			if _, isDir := s.dirs[parsed]; isDir {
				return memoryvfs.Entry{}, fmt.Errorf("%w: %s is a directory", memoryvfs.ErrInvalidEntry, parsed)
			}
			return memoryvfs.Entry{}, fmt.Errorf("%w: %s", memoryvfs.ErrNotFound, parsed)
		}
		next := existing.Clone()
		if strings.TrimSpace(next.Content) != "" {
			next.Content += "\n"
		}
		next.Content += text
		next.Updated = s.now()
		return s.writeLockedPrepared(ctx, s.prepareEntry(ctx, next), nil)
	}
	for attempt := 0; attempt < 8; attempt++ {
		existing, readErr := s.Read(ctx, parsed, memoryvfs.Level2)
		if readErr != nil {
			return memoryvfs.Entry{}, readErr
		}
		next := existing.Clone()
		if strings.TrimSpace(next.Content) != "" {
			next.Content += "\n"
		}
		next.Content += text
		next.Updated = s.now()
		next = s.prepareEntry(ctx, next)
		vector := s.embedEntry(ctx, next)
		if err := s.lock(); err != nil {
			return memoryvfs.Entry{}, err
		}
		current, ok := s.entries[parsed]
		if !ok {
			_, isDir := s.dirs[parsed]
			s.mu.Unlock()
			if isDir {
				return memoryvfs.Entry{}, fmt.Errorf("%w: %s is a directory", memoryvfs.ErrInvalidEntry, parsed)
			}
			return memoryvfs.Entry{}, fmt.Errorf("%w: %s", memoryvfs.ErrNotFound, parsed)
		}
		if current.Version != existing.Version {
			s.mu.Unlock()
			continue
		}
		written, writeErr := s.writeLockedPrepared(ctx, next, vector)
		s.mu.Unlock()
		if writeErr != nil {
			return memoryvfs.Entry{}, writeErr
		}
		return written, nil
	}
	return memoryvfs.Entry{}, fmt.Errorf("%w: append %s: concurrent updates", memoryvfs.ErrConflict, parsed)
}

// Move 移动条目或整棵目录。条目保留 Created 并递增 Version；目标存在则 ErrExists。
func (s *Store) Move(ctx context.Context, from, to memoryvfs.URI) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if err := s.lock(); err != nil {
		return err
	}
	defer s.mu.Unlock()
	fromURI, err := memoryvfs.ParseURI(string(from))
	if err != nil {
		return err
	}
	toURI, err := memoryvfs.ParseURI(string(to))
	if err != nil {
		return err
	}
	if fromURI == toURI {
		return nil
	}
	if fromURI == memoryvfs.RootURI || toURI == memoryvfs.RootURI {
		return fmt.Errorf("%w: cannot move the root", memoryvfs.ErrInvalidEntry)
	}
	if toURI.IsUnder(fromURI) {
		return fmt.Errorf("%w: cannot move %s under itself", memoryvfs.ErrInvalidEntry, fromURI)
	}
	if entry, ok := s.entries[fromURI]; ok {
		return s.moveEntryLocked(entry, toURI)
	}
	if _, ok := s.dirs[fromURI]; ok {
		return s.moveDirLocked(fromURI, toURI)
	}
	return fmt.Errorf("%w: %s", memoryvfs.ErrNotFound, fromURI)
}

func (s *Store) moveEntryLocked(entry memoryvfs.Entry, to memoryvfs.URI) error {
	if _, exists := s.entries[to]; exists {
		return fmt.Errorf("%w: %s", memoryvfs.ErrExists, to)
	}
	if _, exists := s.dirs[to]; exists {
		return fmt.Errorf("%w: %s", memoryvfs.ErrExists, to)
	}
	if err := s.checkDirChainLocked(to); err != nil {
		return err
	}
	next := entry.Clone()
	next.URI = to
	if next.Title == "" || next.Title == entry.URI.Base() {
		next.Title = to.Base()
	}
	next.Version = entry.Version + 1
	next.Updated = s.now()
	if err := memoryvfs.ValidateEntry(next); err != nil {
		return err
	}
	if err := s.persistEntryLocked(next); err != nil {
		return err
	}
	if err := s.removeEntryFileLocked(entry.URI); err != nil {
		return err
	}
	s.dropEntryLocked(entry.URI)
	stored := next.Clone()
	s.entries[to] = stored
	s.index.Put(stored)
	if vector, ok := s.vectors[entry.URI]; ok {
		delete(s.vectors, entry.URI)
		s.vectors[to] = vector
		s.index.SetVector(to, vector)
	}
	if err := s.touchParentsLocked(to); err != nil {
		return err
	}
	return s.touchParentsLocked(entry.URI)
}

func (s *Store) moveDirLocked(from, to memoryvfs.URI) error {
	if _, exists := s.entries[to]; exists {
		return fmt.Errorf("%w: %s", memoryvfs.ErrExists, to)
	}
	if _, exists := s.dirs[to]; exists {
		return fmt.Errorf("%w: %s", memoryvfs.ErrExists, to)
	}
	if err := s.checkDirChainLocked(to); err != nil {
		return err
	}
	entries, dirs := s.collectSubtreeLocked(from)
	for _, entry := range entries {
		next := entry.Clone()
		next.URI = replaceURIPrefix(entry.URI, from, to)
		if next.Title == "" || next.Title == entry.URI.Base() {
			next.Title = next.URI.Base()
		}
		next.Version = entry.Version + 1
		next.Updated = s.now()
		if err := memoryvfs.ValidateEntry(next); err != nil {
			return err
		}
		if err := s.persistEntryLocked(next); err != nil {
			return err
		}
		if err := s.removeEntryFileLocked(entry.URI); err != nil {
			return err
		}
		s.dropEntryLocked(entry.URI)
		stored := next.Clone()
		s.entries[next.URI] = stored
		s.index.Put(stored)
		if vector, ok := s.vectors[entry.URI]; ok {
			delete(s.vectors, entry.URI)
			s.vectors[next.URI] = vector
			s.index.SetVector(next.URI, vector)
		}
	}
	for _, dir := range dirs {
		next := dir.Clone()
		next.URI = replaceURIPrefix(dir.URI, from, to)
		next.Version = dir.Version + 1
		next.Updated = s.now()
		if err := s.persistDirLocked(next); err != nil {
			return err
		}
		if err := s.removeDirFileLocked(dir.URI); err != nil {
			return err
		}
		delete(s.dirs, dir.URI)
		s.dirs[next.URI] = next
	}
	if err := s.touchParentsLocked(to); err != nil {
		return err
	}
	return s.touchParentsLocked(from)
}

// Delete 删除条目或整棵子树，删除前把内容写进 trash 快照。
func (s *Store) Delete(ctx context.Context, uri memoryvfs.URI) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if err := s.lock(); err != nil {
		return err
	}
	defer s.mu.Unlock()
	parsed, err := memoryvfs.ParseURI(string(uri))
	if err != nil {
		return err
	}
	return s.deleteLocked(parsed)
}

func (s *Store) deleteLocked(uri memoryvfs.URI) error {
	if uri == memoryvfs.RootURI {
		return fmt.Errorf("%w: cannot delete the root", memoryvfs.ErrInvalidEntry)
	}
	if entry, ok := s.entries[uri]; ok {
		if err := s.recordTrashLocked([]memoryvfs.Entry{entry.Clone()}, nil); err != nil {
			return err
		}
		if err := s.removeEntryFileLocked(uri); err != nil {
			return err
		}
		s.dropEntryLocked(uri)
		return s.touchParentsLocked(uri)
	}
	if _, ok := s.dirs[uri]; ok {
		entries, dirs := s.collectSubtreeLocked(uri)
		if err := s.recordTrashLocked(entries, dirs); err != nil {
			return err
		}
		for _, entry := range entries {
			if err := s.removeEntryFileLocked(entry.URI); err != nil {
				return err
			}
			s.dropEntryLocked(entry.URI)
		}
		for _, dir := range dirs {
			if err := s.removeDirFileLocked(dir.URI); err != nil {
				return err
			}
			delete(s.dirs, dir.URI)
		}
		return s.touchParentsLocked(uri)
	}
	return fmt.Errorf("%w: %s", memoryvfs.ErrNotFound, uri)
}

// Mkdir 创建（或更新描述）一个目录 sidecar。已存在时返回既有目录。
func (s *Store) Mkdir(ctx context.Context, uri memoryvfs.URI, description string) (memoryvfs.Entry, error) {
	if err := ctxErr(ctx); err != nil {
		return memoryvfs.Entry{}, err
	}
	if err := s.lock(); err != nil {
		return memoryvfs.Entry{}, err
	}
	defer s.mu.Unlock()
	parsed, err := memoryvfs.ParseURI(string(uri))
	if err != nil {
		return memoryvfs.Entry{}, err
	}
	if parsed == memoryvfs.RootURI {
		return s.statLocked(memoryvfs.RootURI)
	}
	return s.mkdirLocked(parsed, description)
}

// ---------------------------------------------------------------------------
// 内部写入辅助
// ---------------------------------------------------------------------------

func (s *Store) persistEntryLocked(entry memoryvfs.Entry) error {
	if s.dir == "" {
		return nil
	}
	path, err := s.resolvePath(entriesDirName, entry.URI, ".json")
	if err != nil {
		return err
	}
	return writeJSONAtomic(path, entry)
}

func (s *Store) persistDirLocked(entry memoryvfs.Entry) error {
	if s.dir == "" || entry.URI == memoryvfs.RootURI {
		return nil
	}
	path, err := s.resolvePath(dirsDirName, entry.URI, ".json")
	if err != nil {
		return err
	}
	return writeJSONAtomic(path, entry)
}

func (s *Store) removeEntryFileLocked(uri memoryvfs.URI) error {
	if s.dir == "" {
		return nil
	}
	path, err := s.resolvePath(entriesDirName, uri, ".json")
	if err != nil {
		return err
	}
	return removeFile(path)
}

func (s *Store) removeDirFileLocked(uri memoryvfs.URI) error {
	if s.dir == "" || uri == memoryvfs.RootURI {
		return nil
	}
	path, err := s.resolvePath(dirsDirName, uri, ".json")
	if err != nil {
		return err
	}
	return removeFile(path)
}

func removeFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("memvfs: remove %s: %w", path, err)
	}
	return syncDir(filepath.Dir(path))
}

func (s *Store) dropEntryLocked(uri memoryvfs.URI) {
	delete(s.entries, uri)
	s.index.Delete(uri)
	delete(s.vectors, uri)
}

func (s *Store) collectSubtreeLocked(dir memoryvfs.URI) ([]memoryvfs.Entry, []memoryvfs.Entry) {
	var entries []memoryvfs.Entry
	for uri, entry := range s.entries {
		if uri.IsUnder(dir) {
			entries = append(entries, entry.Clone())
		}
	}
	var dirs []memoryvfs.Entry
	for uri, entry := range s.dirs {
		if uri == memoryvfs.RootURI {
			continue
		}
		if uri.IsUnder(dir) {
			dirs = append(dirs, entry.Clone())
		}
	}
	sort.Slice(entries, func(a, b int) bool { return entries[a].URI < entries[b].URI })
	sort.Slice(dirs, func(a, b int) bool { return dirs[a].URI < dirs[b].URI })
	return entries, dirs
}

func (s *Store) recordTrashLocked(entries, dirs []memoryvfs.Entry) error {
	if s.dir == "" {
		return nil
	}
	trash := struct {
		At      time.Time         `json:"at"`
		Entries []memoryvfs.Entry `json:"entries,omitempty"`
		Dirs    []memoryvfs.Entry `json:"dirs,omitempty"`
	}{At: s.now(), Entries: entries, Dirs: dirs}
	id := fmt.Sprintf("%d-%s", s.now().UnixNano(), randomHex(4))
	path := filepath.Join(s.dir, trashDirName, id+".json")
	return writeJSONAtomic(path, trash)
}

func replaceURIPrefix(uri, from, to memoryvfs.URI) memoryvfs.URI {
	rest := strings.TrimPrefix(string(uri), string(from))
	return memoryvfs.URI(string(to) + rest)
}

// embedText 是写入/重建时送给 Embedder 的文本：标题 + L0 + L1 + 截断的正文。
func (s *Store) embedText(entry memoryvfs.Entry) string {
	text := strings.Join([]string{entry.Title, entry.Summary, entry.Overview}, "\n")
	if entry.Content != "" {
		if trimmed, _ := memoryvfs.TruncateBytes(entry.Content, 4000); trimmed != "" {
			text += "\n" + trimmed
		}
	}
	return text
}

// embedLocked 尽力计算向量；失败时静默降级到 BM25（写入不因此失败）。
func (s *Store) embedLocked(ctx context.Context, entry memoryvfs.Entry) {
	if s.opts.Embedder == nil || s.dimension <= 0 {
		return
	}
	vectors, err := s.opts.Embedder.Embed(ctx, []string{s.embedText(entry)})
	if err != nil || len(vectors) != 1 || len(vectors[0]) != s.dimension {
		return
	}
	stored := append([]float32(nil), vectors[0]...)
	s.vectors[entry.URI] = stored
	s.index.SetVector(entry.URI, stored)
}
