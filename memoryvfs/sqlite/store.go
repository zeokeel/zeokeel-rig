package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zeokeel/zeokeel-rig/memoryvfs"
	_ "modernc.org/sqlite"
)

// Option 配置 SQLite 后端。
type Option func(*Store)

// WithWorkspace 选择工作区（默认 "default"）。所有读写都带 workspace_id，
// 同一个库里的不同工作区互不可见。
func WithWorkspace(id string) Option {
	return func(s *Store) {
		if trimmed := strings.TrimSpace(id); trimmed != "" {
			s.workspace = trimmed
		}
	}
}

// Store 是 memoryvfs.Store 的 SQLite 实现。
type Store struct {
	db        *sql.DB
	workspace string

	mu     sync.RWMutex
	closed bool
}

var _ memoryvfs.Store = (*Store)(nil)

// Open 打开/创建 SQLite 库。dsn 例如 "file:/tmp/memory.db?_pragma=busy_timeout(5000)"。
func Open(dsn string, opts ...Option) (*Store, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("sqlite: empty dsn")
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open: %w", err)
	}
	// modernc 的 sqlite 驱动在单连接下最稳；本包内部只做短事务。
	db.SetMaxOpenConns(1)
	store := &Store{db: db, workspace: "default"}
	for _, opt := range opts {
		if opt != nil {
			opt(store)
		}
	}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite: pragma: %w", err)
	}
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS memory_entries (
			workspace TEXT NOT NULL, uri TEXT NOT NULL, entry BLOB NOT NULL, updated INTEGER NOT NULL,
			PRIMARY KEY (workspace, uri))`,
		`CREATE INDEX IF NOT EXISTS memory_entries_updated ON memory_entries(workspace, updated DESC)`,
		`CREATE TABLE IF NOT EXISTS memory_pending (
			workspace TEXT NOT NULL, session_id TEXT NOT NULL, seq INTEGER NOT NULL, message BLOB NOT NULL,
			PRIMARY KEY (workspace, session_id, seq))`,
		`CREATE TABLE IF NOT EXISTS memory_checkpoints (
			workspace TEXT NOT NULL, session_id TEXT NOT NULL, at INTEGER NOT NULL,
			PRIMARY KEY (workspace, session_id))`,
		`CREATE TABLE IF NOT EXISTS memory_snapshots (
			workspace TEXT NOT NULL, id TEXT NOT NULL, label TEXT NOT NULL, created INTEGER NOT NULL,
			entries BLOB NOT NULL, PRIMARY KEY (workspace, id))`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("sqlite: schema: %w", err)
		}
	}
	return store, nil
}

func (s *Store) check(ctx context.Context) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	s.mu.RLock()
	closed := s.closed
	s.mu.RUnlock()
	if closed {
		return memoryvfs.ErrClosed
	}
	return nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	return s.db.Close()
}

func (s *Store) workspaceID() string { return s.workspace }

func (s *Store) load(ctx context.Context, uri memoryvfs.URI) (memoryvfs.Entry, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT entry FROM memory_entries WHERE workspace=? AND uri=?`, s.workspaceID(), string(uri)).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return memoryvfs.Entry{}, fmt.Errorf("%w: %s", memoryvfs.ErrNotFound, uri)
	}
	if err != nil {
		return memoryvfs.Entry{}, fmt.Errorf("sqlite: read %s: %w", uri, err)
	}
	var entry memoryvfs.Entry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return memoryvfs.Entry{}, fmt.Errorf("%w: %s: %v", memoryvfs.ErrCorrupt, uri, err)
	}
	return entry, nil
}

func (s *Store) all(ctx context.Context) ([]memoryvfs.Entry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT entry FROM memory_entries WHERE workspace=? ORDER BY updated DESC, uri ASC`, s.workspaceID())
	if err != nil {
		return nil, fmt.Errorf("sqlite: list: %w", err)
	}
	defer rows.Close()
	var out []memoryvfs.Entry
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var entry memoryvfs.Entry
		if err := json.Unmarshal(raw, &entry); err != nil {
			continue
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}

func (s *Store) put(ctx context.Context, entry memoryvfs.Entry) error {
	raw, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("sqlite: marshal %s: %w", entry.URI, err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO memory_entries(workspace,uri,entry,updated) VALUES(?,?,?,?)
		 ON CONFLICT(workspace,uri) DO UPDATE SET entry=excluded.entry, updated=excluded.updated`,
		s.workspaceID(), string(entry.URI), raw, entry.Updated.UnixNano())
	if err != nil {
		return fmt.Errorf("sqlite: write %s: %w", entry.URI, err)
	}
	return nil
}

func (s *Store) Write(ctx context.Context, entry memoryvfs.Entry) (memoryvfs.Entry, error) {
	if err := s.check(ctx); err != nil {
		return memoryvfs.Entry{}, err
	}
	uri, err := memoryvfs.ParseURI(string(entry.URI))
	if err != nil {
		return memoryvfs.Entry{}, err
	}
	entry.URI = uri
	now := time.Now().UTC()
	entry = memoryvfs.NormalizeEntry(entry, func() time.Time { return now })
	if entry.Kind != memoryvfs.KindDir && strings.TrimSpace(entry.Summary) == "" {
		entry.Summary, _ = memoryvfs.TruncateBytes(memoryvfs.CollapseWhitespace(entry.Content), 200)
	}
	if err := memoryvfs.ValidateEntry(entry); err != nil {
		return memoryvfs.Entry{}, err
	}
	if existing, loadErr := s.load(ctx, uri); loadErr == nil {
		entry.Created = existing.Created
		entry.Version = existing.Version + 1
		if entry.Updated.IsZero() || !entry.Updated.After(existing.Updated) {
			entry.Updated = now
		}
	} else {
		if entry.Version == 0 {
			entry.Version = 1
		}
	}
	entry.Tokens = entry.TokenEstimate(memoryvfs.Level2)
	if err := s.put(ctx, entry); err != nil {
		return memoryvfs.Entry{}, err
	}
	return entry.Clone(), nil
}

func (s *Store) Stat(ctx context.Context, uri memoryvfs.URI) (memoryvfs.Entry, error) {
	if err := s.check(ctx); err != nil {
		return memoryvfs.Entry{}, err
	}
	return s.load(ctx, uri)
}

func (s *Store) Read(ctx context.Context, uri memoryvfs.URI, level memoryvfs.Level) (memoryvfs.Entry, error) {
	if err := s.check(ctx); err != nil {
		return memoryvfs.Entry{}, err
	}
	entry, err := s.load(ctx, uri)
	if err != nil {
		return memoryvfs.Entry{}, err
	}
	switch level {
	case memoryvfs.Level0:
		entry.Overview, entry.Content = "", ""
	case memoryvfs.Level1:
		entry.Content = ""
	case memoryvfs.Level2, memoryvfs.LevelAny:
	default:
		return memoryvfs.Entry{}, fmt.Errorf("%w: invalid level %d", memoryvfs.ErrInvalidEntry, level)
	}
	return entry, nil
}

func (s *Store) Append(ctx context.Context, uri memoryvfs.URI, text string) (memoryvfs.Entry, error) {
	entry, err := s.Read(ctx, uri, memoryvfs.Level2)
	if err != nil {
		return memoryvfs.Entry{}, err
	}
	if strings.TrimSpace(text) == "" {
		return entry, nil
	}
	if strings.TrimSpace(entry.Content) != "" {
		entry.Content += "\n"
	}
	entry.Content += text
	return s.Write(ctx, entry)
}

func (s *Store) Mkdir(ctx context.Context, uri memoryvfs.URI, description string) (memoryvfs.Entry, error) {
	if entry, err := s.load(ctx, uri); err == nil {
		if entry.Kind == memoryvfs.KindDir {
			return entry, nil
		}
		return memoryvfs.Entry{}, fmt.Errorf("%w: %s is an entry", memoryvfs.ErrExists, uri)
	}
	return s.Write(ctx, memoryvfs.Entry{
		URI: uri, Kind: memoryvfs.KindDir, Title: uri.Base(),
		Summary: memoryvfs.CollapseWhitespace(description), Content: description,
	})
}

func (s *Store) List(ctx context.Context, uri memoryvfs.URI) ([]memoryvfs.Entry, error) {
	if err := s.check(ctx); err != nil {
		return nil, err
	}
	all, err := s.all(ctx)
	if err != nil {
		return nil, err
	}
	prefix := strings.TrimSuffix(string(uri), "/")
	if prefix != string(memoryvfs.RootURI) {
		prefix += "/"
	}
	seen := make(map[memoryvfs.URI]memoryvfs.Entry)
	for _, entry := range all {
		raw := string(entry.URI)
		if !strings.HasPrefix(raw, prefix) {
			continue
		}
		relative := strings.TrimPrefix(raw, prefix)
		if relative == "" {
			continue
		}
		segment := strings.Split(relative, "/")[0]
		child, childErr := memoryvfs.Join(strings.TrimPrefix(prefix, memoryvfs.Scheme), segment)
		if childErr != nil {
			continue
		}
		if _, ok := seen[child]; ok {
			continue
		}
		if child == entry.URI {
			seen[child] = entry
		} else {
			seen[child] = memoryvfs.Entry{URI: child, Kind: memoryvfs.KindDir, Title: segment, Summary: segment}
		}
	}
	out := make([]memoryvfs.Entry, 0, len(seen))
	for _, entry := range seen {
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].URI < out[j].URI })
	return out, nil
}

func (s *Store) Tree(ctx context.Context, uri memoryvfs.URI, depth int) ([]memoryvfs.Entry, error) {
	if err := s.check(ctx); err != nil {
		return nil, err
	}
	if depth <= 0 {
		depth = 3
	}
	all, err := s.all(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]memoryvfs.Entry, 0, len(all))
	for _, entry := range all {
		if !entry.URI.IsUnder(uri) || entry.URI == uri {
			continue
		}
		if strings.Count(strings.TrimPrefix(string(entry.URI), string(uri)), "/") > depth {
			continue
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].URI < out[j].URI })
	return out, nil
}

func (s *Store) Move(ctx context.Context, from, to memoryvfs.URI) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	if from == to {
		return nil
	}
	all, err := s.all(ctx)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	prefix := string(from) + "/"
	for _, entry := range all {
		raw := string(entry.URI)
		var next string
		if raw == string(from) {
			next = string(to)
		} else if strings.HasPrefix(raw, prefix) {
			next = string(to) + strings.TrimPrefix(raw, string(from))
		} else {
			continue
		}
		moved := entry.Clone()
		moved.URI = memoryvfs.URI(next)
		body, _ := json.Marshal(moved)
		if _, err := tx.ExecContext(ctx, `UPDATE memory_entries SET uri=?, entry=?, updated=? WHERE workspace=? AND uri=?`, next, body, moved.Updated.UnixNano(), s.workspaceID(), raw); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Delete(ctx context.Context, uri memoryvfs.URI) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	prefix := string(uri) + "/"
	res, err := s.db.ExecContext(ctx, `DELETE FROM memory_entries WHERE workspace=? AND (uri=? OR substr(uri,1,?)=?)`, s.workspaceID(), string(uri), len(prefix), prefix)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: %s", memoryvfs.ErrNotFound, uri)
	}
	return nil
}

func (s *Store) Remember(ctx context.Context, entry memoryvfs.Entry) (memoryvfs.Entry, error) {
	if existing, err := s.Read(ctx, entry.URI, memoryvfs.Level2); err == nil {
		if strings.TrimSpace(entry.Content) != "" && !strings.Contains(existing.Content, entry.Content) {
			existing.Content = strings.TrimSpace(existing.Content) + "\n" + entry.Content
		}
		if entry.Summary != "" {
			existing.Summary = entry.Summary
		}
		return s.Write(ctx, existing)
	}
	return s.Write(ctx, entry)
}

func (s *Store) Forget(ctx context.Context, uri memoryvfs.URI) error { return s.Delete(ctx, uri) }

func (s *Store) query(ctx context.Context, query memoryvfs.Query) (memoryvfs.Result, error) {
	if err := s.check(ctx); err != nil {
		return memoryvfs.Result{}, err
	}
	query = memoryvfs.NormalizeQuery(query)
	all, err := s.all(ctx)
	if err != nil {
		return memoryvfs.Result{}, err
	}
	tokens := strings.Fields(strings.ToLower(query.Text))
	scope := query.Scope
	if scope == "" {
		scope = memoryvfs.RootURI
	}
	hits := make([]memoryvfs.Entry, 0, len(all))
	totalTokens := 0
	for _, entry := range all {
		if !entry.URI.IsUnder(scope) {
			continue
		}
		if len(query.Kinds) > 0 && !containsKind(query.Kinds, entry.Kind) {
			continue
		}
		if len(query.Types) > 0 && !containsString(query.Types, entry.Type) {
			continue
		}
		if len(query.Tags) > 0 {
			match := true
			for key, value := range query.Tags {
				if entry.Tags[key] != value {
					match = false
					break
				}
			}
			if !match {
				continue
			}
		}
		if len(query.Levels) > 0 && !hasLevel(entry, query.Levels) {
			continue
		}
		score := 0.0
		if len(tokens) == 0 {
			score = float64(entry.Updated.UnixNano()) / 1e18
		} else {
			text := strings.ToLower(entry.Title + " " + entry.Summary + " " + entry.Overview + " " + entry.Content)
			for _, token := range tokens {
				score += float64(strings.Count(text, token))
			}
			if score == 0 {
				continue
			}
		}
		entry.Score = score
		entry.Tokens = entry.TokenEstimate(memoryvfs.Level2)
		if !query.IncludeBody {
			entry.Content = ""
		}
		hits = append(hits, entry)
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].URI < hits[j].URI
	})
	total := len(hits)
	if query.Offset > 0 && query.Offset < len(hits) {
		hits = hits[query.Offset:]
	} else if query.Offset >= len(hits) {
		hits = nil
	}
	limit := query.Limit
	if limit <= 0 {
		limit = 20
	}
	if len(hits) > limit {
		hits = hits[:limit]
	}
	kept := hits[:0]
	for _, entry := range hits {
		cost := entry.TokenEstimate(memoryvfs.Level2)
		if query.TokenBudget > 0 && totalTokens+cost > query.TokenBudget {
			break
		}
		totalTokens += cost
		kept = append(kept, entry)
	}
	return memoryvfs.Result{Entries: kept, Total: total, Tokens: totalTokens}, nil
}

func (s *Store) Find(ctx context.Context, query memoryvfs.Query) (memoryvfs.Result, error) {
	return s.query(ctx, query)
}
func (s *Store) Search(ctx context.Context, query memoryvfs.Query) (memoryvfs.Result, error) {
	return s.query(ctx, query)
}

func containsKind(kinds []memoryvfs.Kind, kind memoryvfs.Kind) bool {
	for _, k := range kinds {
		if k == kind {
			return true
		}
	}
	return false
}
func containsString(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}
func hasLevel(entry memoryvfs.Entry, levels []memoryvfs.Level) bool {
	for _, level := range levels {
		switch level {
		case memoryvfs.Level0:
			if entry.Summary != "" {
				return true
			}
		case memoryvfs.Level1:
			if entry.Overview != "" {
				return true
			}
		case memoryvfs.Level2:
			if entry.Content != "" {
				return true
			}
		case memoryvfs.LevelAny:
			return true
		}
	}
	return false
}

func (s *Store) Capture(ctx context.Context, sessionID string, messages ...memoryvfs.Message) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return errors.New("sqlite: empty session id")
	}
	existing, err := s.pending(ctx, sessionID)
	if err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(existing))
	for _, message := range existing {
		seen[messageKey(message)] = struct{}{}
	}
	seq := len(existing)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, message := range messages {
		key := messageKey(message)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		raw, _ := json.Marshal(message)
		if _, err := tx.ExecContext(ctx, `INSERT INTO memory_pending(workspace,session_id,seq,message) VALUES(?,?,?,?)`, s.workspaceID(), sessionID, seq, raw); err != nil {
			return err
		}
		seq++
	}
	return tx.Commit()
}

func messageKey(message memoryvfs.Message) string {
	if message.Key != "" {
		return message.Key
	}
	return message.Role + "\x00" + message.Content
}

func (s *Store) pending(ctx context.Context, sessionID string) ([]memoryvfs.Message, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT message FROM memory_pending WHERE workspace=? AND session_id=? ORDER BY seq`, s.workspaceID(), sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []memoryvfs.Message
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var message memoryvfs.Message
		if json.Unmarshal(raw, &message) == nil {
			out = append(out, message)
		}
	}
	return out, rows.Err()
}

func (s *Store) Pending(ctx context.Context, sessionID string) (memoryvfs.SessionState, error) {
	messages, err := s.pending(ctx, strings.TrimSpace(sessionID))
	if err != nil {
		return memoryvfs.SessionState{}, err
	}
	state := memoryvfs.SessionState{ID: sessionID, Pending: len(messages)}
	for _, message := range messages {
		state.Tokens += memoryvfs.EstimateTokens(message.Content)
	}
	var at int64
	_ = s.db.QueryRowContext(ctx, `SELECT at FROM memory_checkpoints WHERE workspace=? AND session_id=?`, s.workspaceID(), sessionID).Scan(&at)
	if at > 0 {
		state.LastCommit = time.Unix(0, at).UTC()
	}
	state.Updated = time.Now().UTC()
	return state, nil
}

func (s *Store) Commit(ctx context.Context, sessionID string, opts memoryvfs.CommitOptions) (memoryvfs.CommitResult, error) {
	messages, err := s.pending(ctx, sessionID)
	if err != nil {
		return memoryvfs.CommitResult{}, err
	}
	if len(messages) == 0 {
		return memoryvfs.CommitResult{}, nil
	}
	segment := sessionSegment(sessionID)
	sum := sha256.New()
	for _, message := range messages {
		fmt.Fprintf(sum, "%s\x00%s\x00%s\n", message.Role, message.Content, message.Key)
	}
	hash := hex.EncodeToString(sum.Sum(nil))[:12]
	archiveURI, _ := memoryvfs.ParseURI(fmt.Sprintf("%s%s/%d-%s", memoryvfs.Scheme+"sessions/", segment, time.Now().UTC().UnixNano(), hash))
	var transcript strings.Builder
	for _, message := range messages {
		fmt.Fprintf(&transcript, "%s: %s\n", message.Role, message.Content)
	}
	content, _ := memoryvfs.TruncateBytes(transcript.String(), memoryvfs.MaxContentBytes)
	archive, err := s.Write(ctx, memoryvfs.Entry{
		URI: archiveURI, Kind: memoryvfs.KindSession, Title: "session " + segment,
		Summary: fmt.Sprintf("session %s (%d messages)", sessionID, len(messages)), Content: content,
	})
	if err != nil {
		return memoryvfs.CommitResult{}, err
	}
	if opts.WorkingMemory {
		overviewURI, _ := memoryvfs.ParseURI(fmt.Sprintf("%s%s/overview", memoryvfs.Scheme+"sessions/", segment))
		overview, _ := memoryvfs.TruncateBytes(content, memoryvfs.MaxOverviewBytes)
		_, _ = s.Write(ctx, memoryvfs.Entry{URI: overviewURI, Kind: memoryvfs.KindSession, Title: "session " + segment, Summary: archive.Summary, Content: overview})
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM memory_pending WHERE workspace=? AND session_id=?`, s.workspaceID(), sessionID); err != nil {
		return memoryvfs.CommitResult{}, err
	}
	_, _ = s.db.ExecContext(ctx, `INSERT INTO memory_checkpoints(workspace,session_id,at) VALUES(?,?,?) ON CONFLICT(workspace,session_id) DO UPDATE SET at=excluded.at`, s.workspaceID(), sessionID, time.Now().UTC().UnixNano())
	return memoryvfs.CommitResult{Archive: archive.URI, Tokens: archive.Tokens}, nil
}

func sessionSegment(id string) string {
	lowered := strings.ToLower(strings.TrimSpace(id))
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
	if segment != strings.TrimSpace(id) {
		sum := sha256.Sum256([]byte(id))
		segment += "-" + hex.EncodeToString(sum[:])[:8]
	}
	return segment
}

func (s *Store) CompileRules(ctx context.Context, instruction string, sources []memoryvfs.URI) (memoryvfs.Entry, error) {
	text := strings.TrimSpace(instruction)
	for _, source := range sources {
		if entry, err := s.Read(ctx, source, memoryvfs.Level2); err == nil {
			text += "\n" + entry.Content
		}
	}
	if text == "" {
		return memoryvfs.Entry{}, fmt.Errorf("%w: empty instruction", memoryvfs.ErrInvalidEntry)
	}
	sum := sha256.Sum256([]byte(text))
	uri, _ := memoryvfs.ParseURI(fmt.Sprintf("%srules/general-%s", memoryvfs.Scheme, hex.EncodeToString(sum[:])[:8]))
	summary, _ := memoryvfs.TruncateBytes(memoryvfs.CollapseWhitespace(text), 200)
	return s.Write(ctx, memoryvfs.Entry{URI: uri, Kind: memoryvfs.KindRule, Type: memoryvfs.TypeRules, Summary: summary, Content: text})
}

func (s *Store) SearchRules(ctx context.Context, taskContext string, threshold float64) ([]memoryvfs.Rule, error) {
	all, err := s.all(ctx)
	if err != nil {
		return nil, err
	}
	tokens := strings.Fields(strings.ToLower(taskContext))
	var rules []memoryvfs.Rule
	for _, entry := range all {
		if entry.Kind != memoryvfs.KindRule {
			continue
		}
		score := 0.0
		text := strings.ToLower(entry.Content)
		for _, token := range tokens {
			if strings.Contains(text, token) {
				score++
			}
		}
		if len(tokens) > 0 {
			score /= float64(len(tokens))
		}
		if score < threshold {
			continue
		}
		rules = append(rules, memoryvfs.Rule{URI: entry.URI, Content: entry.Content, Probability: score})
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].Probability > rules[j].Probability })
	return rules, nil
}

func (s *Store) Snapshot(ctx context.Context, label string) (memoryvfs.Snapshot, error) {
	all, err := s.all(ctx)
	if err != nil {
		return memoryvfs.Snapshot{}, err
	}
	raw, err := json.Marshal(all)
	if err != nil {
		return memoryvfs.Snapshot{}, err
	}
	now := time.Now().UTC()
	id := now.Format("20060102T150405.000000000")
	if _, err := s.db.ExecContext(ctx, `INSERT INTO memory_snapshots(workspace,id,label,created,entries) VALUES(?,?,?,?,?)`, s.workspaceID(), id, label, now.UnixNano(), raw); err != nil {
		return memoryvfs.Snapshot{}, err
	}
	return memoryvfs.Snapshot{ID: id, Label: label, Created: now, Entries: len(all), Bytes: int64(len(raw))}, nil
}

func (s *Store) Snapshots(ctx context.Context) ([]memoryvfs.Snapshot, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,label,created,entries FROM memory_snapshots WHERE workspace=? ORDER BY created DESC`, s.workspaceID())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []memoryvfs.Snapshot
	for rows.Next() {
		var snap memoryvfs.Snapshot
		var created int64
		var raw []byte
		if err := rows.Scan(&snap.ID, &snap.Label, &created, &raw); err != nil {
			return nil, err
		}
		snap.Created = time.Unix(0, created).UTC()
		snap.Bytes = int64(len(raw))
		var entries []memoryvfs.Entry
		if json.Unmarshal(raw, &entries) == nil {
			snap.Entries = len(entries)
		}
		out = append(out, snap)
	}
	return out, rows.Err()
}

func (s *Store) Diff(ctx context.Context, from, to string) (string, error) {
	load := func(id string) ([]memoryvfs.Entry, error) {
		var raw []byte
		err := s.db.QueryRowContext(ctx, `SELECT entries FROM memory_snapshots WHERE workspace=? AND id=?`, s.workspaceID(), id).Scan(&raw)
		if err != nil {
			return nil, fmt.Errorf("%w: snapshot %s", memoryvfs.ErrNotFound, id)
		}
		var entries []memoryvfs.Entry
		return entries, json.Unmarshal(raw, &entries)
	}
	a, err := load(from)
	if err != nil {
		return "", err
	}
	b, err := load(to)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("snapshot %s: %d entries\nsnapshot %s: %d entries\n", from, len(a), to, len(b)), nil
}

func (s *Store) Restore(ctx context.Context, id string) error {
	var raw []byte
	if err := s.db.QueryRowContext(ctx, `SELECT entries FROM memory_snapshots WHERE workspace=? AND id=?`, s.workspaceID(), id).Scan(&raw); err != nil {
		return fmt.Errorf("%w: snapshot %s", memoryvfs.ErrNotFound, id)
	}
	var entries []memoryvfs.Entry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return fmt.Errorf("%w: snapshot %s", memoryvfs.ErrCorrupt, id)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_entries WHERE workspace=?`, s.workspaceID()); err != nil {
		return err
	}
	for _, entry := range entries {
		body, _ := json.Marshal(entry)
		if _, err := tx.ExecContext(ctx, `INSERT INTO memory_entries(workspace,uri,entry,updated) VALUES(?,?,?,?)`, s.workspaceID(), string(entry.URI), body, entry.Updated.UnixNano()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Stats(ctx context.Context) (memoryvfs.Stats, error) {
	all, err := s.all(ctx)
	if err != nil {
		return memoryvfs.Stats{}, err
	}
	stats := memoryvfs.Stats{ByKind: map[memoryvfs.Kind]int{}, ByType: map[string]int{}}
	for _, entry := range all {
		stats.Entries++
		if entry.Kind == memoryvfs.KindDir {
			stats.Dirs++
		}
		raw, _ := json.Marshal(entry)
		stats.Bytes += int64(len(raw))
		stats.Tokens += entry.TokenEstimate(memoryvfs.Level2)
		stats.ByKind[entry.Kind]++
		if entry.Type != "" {
			stats.ByType[entry.Type]++
		}
		if stats.Oldest.IsZero() || entry.Created.Before(stats.Oldest) {
			stats.Oldest = entry.Created
		}
		if entry.Updated.After(stats.Newest) {
			stats.Newest = entry.Updated
		}
	}
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT session_id) FROM memory_pending WHERE workspace=?`, s.workspaceID()).Scan(&stats.Sessions)
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_snapshots WHERE workspace=?`, s.workspaceID()).Scan(&stats.Snapshots)
	return stats, nil
}

func (s *Store) GC(ctx context.Context, opts memoryvfs.GCOptions) (memoryvfs.GCResult, error) {
	all, err := s.all(ctx)
	if err != nil {
		return memoryvfs.GCResult{}, err
	}
	now := time.Now().UTC()
	var expired []memoryvfs.URI
	for _, entry := range all {
		if entry.Kind == memoryvfs.KindSession {
			continue
		}
		if entry.ExpiresAt != nil && !entry.ExpiresAt.After(now) {
			expired = append(expired, entry.URI)
		}
	}
	if !opts.DryRun {
		for _, uri := range expired {
			_ = s.Delete(ctx, uri)
		}
	}
	return memoryvfs.GCResult{Expired: len(expired), Kept: len(all) - len(expired)}, nil
}

func (s *Store) Rebuild(ctx context.Context) error { return s.check(ctx) }

func (s *Store) Export(ctx context.Context, scope memoryvfs.URI) (memoryvfs.Pack, error) {
	all, err := s.all(ctx)
	if err != nil {
		return memoryvfs.Pack{}, err
	}
	if scope == "" {
		scope = memoryvfs.RootURI
	}
	var entries []memoryvfs.Entry
	for _, entry := range all {
		if entry.URI.IsUnder(scope) {
			entries = append(entries, entry)
		}
	}
	return memoryvfs.Pack{Version: 1, Generated: time.Now().UTC(), Scope: scope, Entries: entries}, nil
}

func (s *Store) Import(ctx context.Context, pack memoryvfs.Pack, opts memoryvfs.ImportOptions) (memoryvfs.ImportResult, error) {
	var result memoryvfs.ImportResult
	for _, entry := range pack.Entries {
		_, err := s.load(ctx, entry.URI)
		if err == nil && !opts.Overwrite {
			result.Skipped++
			continue
		}
		if err == nil {
			result.Updated++
		} else {
			result.Imported++
		}
		if opts.DryRun {
			continue
		}
		if _, err := s.Write(ctx, entry); err != nil {
			return result, err
		}
	}
	return result, nil
}
