// Package sessionstore is ZeoKeel's SQLite-backed session store kernel module.
//
// It owns the durable half of a conversation: message history (Rig
// rig.History), interruption checkpoints (rig.CheckpointStore), the session
// event log (rig.SessionLog) and the product's session list metadata. The UI
// and the agent backend consume it through contracts.KeyStore; the database
// lives in the product data directory (contracts.KeyDataDir).
//
// The schema is created idempotently on open:
//
//	sessions(id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL DEFAULT '',
//	         title TEXT, created_at INTEGER, updated_at INTEGER)
//	messages(id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL
//	         REFERENCES sessions(id) ON DELETE CASCADE, ordinal INTEGER,
//	         payload BLOB NOT NULL, created_at INTEGER)
//	events(seq INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT, type TEXT,
//	       payload BLOB, created_at INTEGER)
//	checkpoints(session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
//	            payload BLOB, updated_at INTEGER)
//
// The database uses WAL, foreign keys and a busy timeout, all applied on every
// connection through the driver DSN. Timestamps are stored as UTC unix
// nanoseconds; message payloads are JSON-encoded rig.Message values, checkpoint
// payloads are JSON-encoded rig.Checkpoint values, event payloads are stored
// verbatim as JSON.
//
// # Semantics worth knowing
//
//   - Append (history) and AppendEvent create the session row when it is
//     missing and bump the session's updated_at timestamp; they are atomic
//     transactions. A second Store opened on the same file serializes through
//     SQLite's busy timeout instead of returning SQLITE_BUSY.
//   - History.Load on an unknown session returns (nil, nil); CheckpointStore
//     Load returns (nil, nil) when no checkpoint was saved; Last with a
//     non-positive limit and Events for an unknown session return an empty
//     slice.
//   - Search matches the event type and payload text case-insensitively with
//     LIKE wildcards in the query treated literally. An empty query returns all
//     events; a non-positive limit means no limit (callers pass a positive
//     limit for bounded UI queries).
//   - DeleteSession removes the session's messages, events and checkpoint
//     explicitly (events has no foreign key in the fixed schema); deleting an
//     unknown session is a no-op. RenameSession on an unknown session returns
//     ErrUnknownSession.
package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	rig "github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/session/sqlsearch"

	)

// ErrUnknownSession is returned by operations that require an existing session
// (for example RenameSession).
var ErrUnknownSession = errors.New("sqlstore: unknown session")

// Meta is one session's list metadata. WorkspaceID is an opaque host scope (the
// database is shared by every host that points at it).
type Meta struct {
	ID           string
	WorkspaceID  string
	Title        string
	MessageCount int
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Hit is one cross-session search result.
type Hit struct {
	// SessionID is the session the message belongs to.
	SessionID string
	// SessionTitle is that session's display title (for the result row).
	SessionTitle string
	// Ordinal is the message's stable 1-based position within the session.
	Ordinal int
	// Role is the message role (user/assistant/tool).
	Role string
	// Snippet is a short excerpt around the match.
	Snippet string
	// CreatedAt is when the message was stored.
	CreatedAt time.Time
}

// Searcher finds messages across sessions.
type Searcher interface {
	// SearchMessages matches the decoded search text and session titles,
	// newest first, bounded by limit (non-positive means the default).
	SearchMessages(ctx context.Context, query string, limit int) ([]Hit, error)
}

// ScopeStore records which opaque host scope a session belongs to.
type ScopeStore interface {
	// SetSessionWorkspace records the scope of an existing session.
	SetSessionWorkspace(ctx context.Context, sessionID, workspaceID string) error
}

// Store is the durable session database. It satisfies the rig interfaces the
// agent loop needs and adds the session-list metadata the host UI renders.
//
// The host owns the *sql.DB (driver, DSN and PRAGMAs); New only applies the
// idempotent schema and returns the typed views.
type Store struct {
	db     *sql.DB
	logger rig.Logger
}

var (
	_ Searcher   = (*Store)(nil)
	_ ScopeStore = (*Store)(nil)
	_ rig.History = historyView{}
	_ rig.CheckpointStore = checkpointView{}
)

// historyView is the rig.History view of a store.
type historyView struct{ store *Store }

// checkpointView is the rig.CheckpointStore view of a store.
type checkpointView struct{ store *Store }

// schemaStatements is the idempotent schema applied on every open.
var schemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS sessions (
		id TEXT PRIMARY KEY,
		workspace_id TEXT NOT NULL DEFAULT '',
		title TEXT,
		created_at INTEGER,
		updated_at INTEGER
	)`,
	`CREATE TABLE IF NOT EXISTS messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
		ordinal INTEGER,
		payload BLOB NOT NULL,
		text TEXT NOT NULL DEFAULT '',
		search_text TEXT NOT NULL DEFAULT '',
		created_at INTEGER
	)`,
	`CREATE INDEX IF NOT EXISTS idx_messages_session_ordinal ON messages(session_id, ordinal)`,
	`CREATE TABLE IF NOT EXISTS events (
		seq INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT,
		type TEXT,
		payload BLOB,
		text TEXT NOT NULL DEFAULT '',
		created_at INTEGER
	)`,
	`CREATE INDEX IF NOT EXISTS idx_events_session_seq ON events(session_id, seq)`,
	`CREATE TABLE IF NOT EXISTS checkpoints (
		session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
		payload BLOB,
		updated_at INTEGER
	)`,
}

// New applies the schema to db and returns the store views. The caller owns db
// (including its driver, DSN/PRAGMAs and Close).
func New(ctx context.Context, db *sql.DB, logger rig.Logger) (*Store, error) {
	if db == nil {
		return nil, errors.New("sqlstore: nil database handle")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	store := &Store{db: db, logger: logger}
	if err := store.initSchema(ctx); err != nil {
		return nil, err
	}
	return store, nil
}

// initSchema creates every table/index (idempotent) and adds the columns older
// databases are missing.
func (s *Store) initSchema(ctx context.Context) error {
	for _, statement := range schemaStatements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("sqlstore: initialize schema: %w", err)
		}
	}
	if err := s.migrateWorkspaceColumn(ctx); err != nil {
		return err
	}
	return s.migrateSearchColumns(ctx)
}

// migrateWorkspaceColumn adds sessions.workspace_id to databases created before
// scope association existed (idempotent).
func (s *Store) migrateWorkspaceColumn(ctx context.Context) error {
	exists, err := s.hasColumn(ctx, "sessions", "workspace_id")
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	if _, err := s.db.ExecContext(ctx,
		`ALTER TABLE sessions ADD COLUMN workspace_id TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("sqlstore: add sessions.workspace_id: %w", err)
	}
	return nil
}

// hasColumn reports whether table has the named column.
func (s *Store) hasColumn(ctx context.Context, table, column string) (bool, error) {
	rows, err := s.db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return false, fmt.Errorf("sqlstore: inspect schema of %s: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid          int
			name         string
			columnType   string
			notNull      int
			defaultValue any
			primaryKey   int
		)
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return false, fmt.Errorf("sqlstore: inspect schema of %s: %w", table, err)
		}
		if name == column {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("sqlstore: inspect schema of %s: %w", table, err)
	}
	return false, nil
}

// --- Store: views ----------------------------------------------------------

// History implements Store.
func (s *Store) History() rig.History { return historyView{store: s} }

// CheckpointStore implements Store.
func (s *Store) CheckpointStore() rig.CheckpointStore { return checkpointView{store: s} }

// Replace implements rig.HistoryReplacer: compaction replaces the stored window
// atomically (the raw history is archived by the caller first, fail-closed).
func (v historyView) Replace(ctx context.Context, sessionID string, messages []rig.Message) error {
	return v.store.ReplaceHistory(ctx, sessionID, messages)
}

// Load implements rig.History.
func (v historyView) Load(ctx context.Context, sessionID string) ([]rig.Message, error) {
	return v.store.loadMessages(ctx, sessionID)
}

// Append implements rig.History.
func (v historyView) Append(ctx context.Context, sessionID string, messages ...rig.Message) error {
	return v.store.appendMessages(ctx, sessionID, messages)
}

// Load implements rig.CheckpointStore.
func (v checkpointView) Load(ctx context.Context, sessionID string) (*rig.Checkpoint, error) {
	return v.store.loadCheckpoint(ctx, sessionID)
}

// Save implements rig.CheckpointStore.
func (v checkpointView) Save(ctx context.Context, sessionID string, checkpoint *rig.Checkpoint) error {
	return v.store.saveCheckpoint(ctx, sessionID, checkpoint)
}

// Clear implements rig.CheckpointStore.
func (v checkpointView) Clear(ctx context.Context, sessionID string) error {
	return v.store.clearCheckpoint(ctx, sessionID)
}

// --- Store: session metadata ----------------------------------------------

// CreateSession implements Store. Creating an existing session is a
// no-op: the title and timestamps are left untouched.
func (s *Store) CreateSession(ctx context.Context, id, title string) error {
	id, err := requireSessionID(id)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	now := nowNanos()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions(id, title, created_at, updated_at) VALUES(?, ?, ?, ?)
		 ON CONFLICT(id) DO NOTHING`,
		id, title, now, now); err != nil {
		return fmt.Errorf("sqlstore: create session %q: %w", id, err)
	}
	return nil
}

// DeleteSession implements Store. It removes the session, its
// messages, events and checkpoint in one transaction; an unknown session is a
// no-op.
func (s *Store) DeleteSession(ctx context.Context, id string) error {
	id, err := requireSessionID(id)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlstore: delete session %q: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	// events has no foreign key in the fixed schema, so delete every dependent
	// row explicitly; the others are also explicit so deletion does not depend
	// on the foreign_keys pragma being enabled.
	for _, statement := range []string{
		`DELETE FROM events WHERE session_id = ?`,
		`DELETE FROM messages WHERE session_id = ?`,
		`DELETE FROM checkpoints WHERE session_id = ?`,
		`DELETE FROM sessions WHERE id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, statement, id); err != nil {
			return fmt.Errorf("sqlstore: delete session %q: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlstore: delete session %q: %w", id, err)
	}
	return nil
}

// RenameSession implements Store. Renaming an unknown session returns
// ErrUnknownSession.
func (s *Store) RenameSession(ctx context.Context, id, title string) error {
	id, err := requireSessionID(id)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET title = ?, updated_at = ? WHERE id = ?`,
		title, nowNanos(), id)
	if err != nil {
		return fmt.Errorf("sqlstore: rename session %q: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlstore: rename session %q: %w", id, err)
	}
	if affected == 0 {
		return fmt.Errorf("%w: %q", ErrUnknownSession, id)
	}
	return nil
}

// SetSessionWorkspace implements ScopeStore: it records
// which workspace a durable session belongs to. Unknown sessions return
// ErrUnknownSession; an empty workspace ID clears the association.
func (s *Store) SetSessionWorkspace(ctx context.Context, sessionID, workspaceID string) error {
	id, err := requireSessionID(sessionID)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET workspace_id = ? WHERE id = ?`,
		strings.TrimSpace(workspaceID), id)
	if err != nil {
		return fmt.Errorf("sqlstore: set workspace of session %q: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlstore: set workspace of session %q: %w", id, err)
	}
	if affected == 0 {
		return fmt.Errorf("%w: %q", ErrUnknownSession, id)
	}
	return nil
}

// Sessions implements Store: newest updated first. One LEFT JOIN +
// GROUP BY computes every message count in a single pass (2026-11 audit
// AR-20: the old COUNT correlated subquery ran once per session row); empty
// sessions keep their row with a zero count.
func (s *Store) Sessions(ctx context.Context) ([]Meta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.id, s.workspace_id, s.title, s.created_at, s.updated_at,
		       COUNT(m.id)
		FROM sessions s
		LEFT JOIN messages m ON m.session_id = s.id
		GROUP BY s.id, s.workspace_id, s.title, s.created_at, s.updated_at
		ORDER BY s.updated_at DESC, s.id ASC`)
	if err != nil {
		return nil, fmt.Errorf("sqlstore: list sessions: %w", err)
	}
	defer rows.Close()
	var metas []Meta
	for rows.Next() {
		meta, err := scanSessionMeta(rows)
		if err != nil {
			return nil, err
		}
		metas = append(metas, meta)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlstore: list sessions: %w", err)
	}
	return metas, nil
}

// GetSessionMeta implements Store.
func (s *Store) GetSessionMeta(ctx context.Context, id string) (Meta, bool, error) {
	id, err := requireSessionID(id)
	if err != nil {
		return Meta{}, false, err
	}
	if err := ctx.Err(); err != nil {
		return Meta{}, false, err
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT s.id, s.workspace_id, s.title, s.created_at, s.updated_at,
		       (SELECT COUNT(*) FROM messages m WHERE m.session_id = s.id)
		FROM sessions s WHERE s.id = ?`, id)
	meta, err := scanSessionMetaRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Meta{}, false, nil
	}
	if err != nil {
		return Meta{}, false, err
	}
	return meta, true, nil
}

// --- Store: history --------------------------------------------------------

// loadMessages returns a session's messages ordered by ordinal.
func (s *Store) loadMessages(ctx context.Context, sessionID string) ([]rig.Message, error) {
	id, err := requireSessionID(sessionID)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT payload FROM messages WHERE session_id = ? ORDER BY ordinal ASC, id ASC`, id)
	if err != nil {
		return nil, fmt.Errorf("sqlstore: load history %q: %w", id, err)
	}
	defer rows.Close()
	var messages []rig.Message
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("sqlstore: load history %q: %w", id, err)
		}
		var message rig.Message
		if err := json.Unmarshal(payload, &message); err != nil {
			return nil, fmt.Errorf("sqlstore: decode message of session %q: %w", id, err)
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlstore: load history %q: %w", id, err)
	}
	return messages, nil
}

// appendMessages appends messages atomically, creating the session row when
// needed and updating its updated_at timestamp.
func (s *Store) appendMessages(ctx context.Context, sessionID string, messages []rig.Message) error {
	id, err := requireSessionID(sessionID)
	if err != nil {
		return err
	}
	if len(messages) == 0 {
		return ctx.Err()
	}
	encoded := make([]encodedMessage, 0, len(messages))
	for index, message := range messages {
		payload, err := json.Marshal(message)
		if err != nil {
			return fmt.Errorf("sqlstore: encode message %d of session %q: %w", index, id, err)
		}
		text, searchText := messageSearchTextColumns(message)
		encoded = append(encoded, encodedMessage{payload: payload, text: text, searchText: searchText})
	}
	now := nowNanos()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlstore: append history %q: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := ensureSessionTx(ctx, tx, id, now); err != nil {
		return err
	}
	var baseOrdinal int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(ordinal), 0) FROM messages WHERE session_id = ?`, id).
		Scan(&baseOrdinal); err != nil {
		return fmt.Errorf("sqlstore: append history %q: %w", id, err)
	}
	for index, message := range encoded {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO messages(session_id, ordinal, payload, text, search_text, created_at) VALUES(?, ?, ?, ?, ?, ?)`,
			id, baseOrdinal+int64(index)+1, message.payload, message.text, message.searchText, now); err != nil {
			return fmt.Errorf("sqlstore: append history %q: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlstore: append history %q: %w", id, err)
	}
	return nil
}

// ReplaceHistory implements Store (Rig HistoryReplacer semantics).
func (s *Store) ReplaceHistory(ctx context.Context, sessionID string, messages []rig.Message) error {
	id, err := requireSessionID(sessionID)
	if err != nil {
		return err
	}
	encoded := make([]encodedMessage, 0, len(messages))
	for index, message := range messages {
		payload, err := json.Marshal(message)
		if err != nil {
			return fmt.Errorf("sqlstore: encode message %d of session %q: %w", index, id, err)
		}
		text, searchText := messageSearchTextColumns(message)
		encoded = append(encoded, encodedMessage{payload: payload, text: text, searchText: searchText})
	}
	now := nowNanos()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlstore: replace history %q: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := ensureSessionTx(ctx, tx, id, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE session_id = ?`, id); err != nil {
		return fmt.Errorf("sqlstore: replace history %q: %w", id, err)
	}
	for index, message := range encoded {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO messages(session_id, ordinal, payload, text, search_text, created_at) VALUES(?, ?, ?, ?, ?, ?)`,
			id, int64(index)+1, message.payload, message.text, message.searchText, now); err != nil {
			return fmt.Errorf("sqlstore: replace history %q: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlstore: replace history %q: %w", id, err)
	}
	return nil
}

// --- Store: checkpoints ----------------------------------------------------

// loadCheckpoint returns (nil, nil) when the session has no checkpoint.
func (s *Store) loadCheckpoint(ctx context.Context, sessionID string) (*rig.Checkpoint, error) {
	id, err := requireSessionID(sessionID)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var payload []byte
	err = s.db.QueryRowContext(ctx,
		`SELECT payload FROM checkpoints WHERE session_id = ?`, id).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sqlstore: load checkpoint %q: %w", id, err)
	}
	var checkpoint rig.Checkpoint
	if err := json.Unmarshal(payload, &checkpoint); err != nil {
		return nil, fmt.Errorf("sqlstore: decode checkpoint %q: %w", id, err)
	}
	return &checkpoint, nil
}

// saveCheckpoint upserts the session checkpoint.
func (s *Store) saveCheckpoint(ctx context.Context, sessionID string, checkpoint *rig.Checkpoint) error {
	id, err := requireSessionID(sessionID)
	if err != nil {
		return err
	}
	if checkpoint == nil {
		return errors.New("sqlstore: checkpoint is required")
	}
	payload, err := json.Marshal(checkpoint)
	if err != nil {
		return fmt.Errorf("sqlstore: encode checkpoint %q: %w", id, err)
	}
	now := nowNanos()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlstore: save checkpoint %q: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := ensureSessionTx(ctx, tx, id, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO checkpoints(session_id, payload, updated_at) VALUES(?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET payload = excluded.payload, updated_at = excluded.updated_at`,
		id, payload, now); err != nil {
		return fmt.Errorf("sqlstore: save checkpoint %q: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlstore: save checkpoint %q: %w", id, err)
	}
	return nil
}

// clearCheckpoint removes a checkpoint (idempotent).
func (s *Store) clearCheckpoint(ctx context.Context, sessionID string) error {
	id, err := requireSessionID(sessionID)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM checkpoints WHERE session_id = ?`, id); err != nil {
		return fmt.Errorf("sqlstore: clear checkpoint %q: %w", id, err)
	}
	return nil
}

// --- Store: session log ----------------------------------------------------

// AppendEvent implements Store: it records one event, creating the
// session row when needed and bumping its updated_at timestamp.
func (s *Store) AppendEvent(ctx context.Context, event rig.SessionEvent) error {
	id, err := requireSessionID(event.SessionID)
	if err != nil {
		return err
	}
	now := nowNanos()
	createdAt := now
	if !event.Time.IsZero() {
		createdAt = event.Time.UTC().UnixNano()
	}
	var payload []byte
	if len(event.Payload) > 0 {
		payload = append([]byte(nil), event.Payload...)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlstore: append event: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := ensureSessionTx(ctx, tx, id, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events(session_id, type, payload, text, created_at) VALUES(?, ?, ?, ?, ?)`,
		id, event.Type, payload, eventPayloadSearchText(payload), createdAt); err != nil {
		return fmt.Errorf("sqlstore: append event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlstore: append event: %w", err)
	}
	return nil
}

// Events implements rig.SessionLog: from <= seq <= to, with to <= 0 meaning
// "until the end".
func (s *Store) Events(ctx context.Context, sessionID string, from, to int64) ([]rig.SessionEvent, error) {
	id, err := requireSessionID(sessionID)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if from < 0 {
		from = 0
	}
	query := `SELECT seq, session_id, type, payload, created_at FROM events WHERE session_id = ? AND seq >= ?`
	args := []any{id, from}
	if to > 0 {
		query += ` AND seq <= ?`
		args = append(args, to)
	}
	query += ` ORDER BY seq ASC`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlstore: list events of %q: %w", id, err)
	}
	defer rows.Close()
	return scanEvents(rows)
}

// Search implements rig.SessionLog: a case-insensitive substring match over the
// event type and the JSON payload text. An empty query returns all events (up
// to limit); a non-positive limit means no limit.
func (s *Store) Search(ctx context.Context, query string, limit int) ([]rig.SessionEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(query)
	statement := `SELECT seq, session_id, type, payload, created_at FROM events`
	var args []any
	if trimmed != "" {
		pattern := "%" + sqlsearch.EscapeLike(trimmed) + "%"
		statement += ` WHERE (type LIKE ? ESCAPE '\' OR text LIKE ? ESCAPE '\')`
		args = append(args, pattern, pattern)
	}
	statement += ` ORDER BY seq ASC`
	if limit > 0 {
		statement += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlstore: search events: %w", err)
	}
	defer rows.Close()
	return scanEvents(rows)
}

// Last implements rig.SessionLog: the newest limit events of one session,
// returned in ascending sequence order.
func (s *Store) Last(ctx context.Context, sessionID string, limit int) ([]rig.SessionEvent, error) {
	id, err := requireSessionID(sessionID)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		return []rig.SessionEvent{}, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq, session_id, type, payload, created_at FROM events
		 WHERE session_id = ? ORDER BY seq DESC LIMIT ?`, id, limit)
	if err != nil {
		return nil, fmt.Errorf("sqlstore: last events of %q: %w", id, err)
	}
	defer rows.Close()
	events, err := scanEvents(rows)
	if err != nil {
		return nil, err
	}
	// The query returned the newest rows in descending order; reverse the slice
	// so Last is the newest-limit window in chronological order.
	for left, right := 0, len(events)-1; left < right; left, right = left+1, right-1 {
		events[left], events[right] = events[right], events[left]
	}
	return events, nil
}

// --- helpers ---------------------------------------------------------------

// execer is the subset of *sql.DB and *sql.Tx used by ensureSessionTx.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// ensureSessionTx creates the session row when missing and bumps updated_at.
// It is the first write of every transaction, so the SQLite write lock is taken
// before any read-max-ordinal query (concurrent appends stay serialized).
func ensureSessionTx(ctx context.Context, tx execer, id string, now int64) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO sessions(id, title, created_at, updated_at) VALUES(?, '', ?, ?)
		ON CONFLICT(id) DO UPDATE SET updated_at = excluded.updated_at`,
		id, now, now); err != nil {
		return fmt.Errorf("sqlstore: ensure session %q: %w", id, err)
	}
	return nil
}

// scanSessionMeta scans one session row from rows.
func scanSessionMeta(rows *sql.Rows) (Meta, error) {
	var (
		meta      Meta
		workspace sql.NullString
		title     sql.NullString
		created   sql.NullInt64
		updated   sql.NullInt64
		count     int64
	)
	if err := rows.Scan(&meta.ID, &workspace, &title, &created, &updated, &count); err != nil {
		return Meta{}, fmt.Errorf("sqlstore: scan session: %w", err)
	}
	meta.WorkspaceID = workspace.String
	meta.Title = title.String
	meta.CreatedAt = timeFromNanos(created.Int64)
	meta.UpdatedAt = timeFromNanos(updated.Int64)
	meta.MessageCount = int(count)
	return meta, nil
}

// scanSessionMetaRow is scanSessionMeta for a single *sql.Row.
func scanSessionMetaRow(row *sql.Row) (Meta, error) {
	var (
		meta      Meta
		workspace sql.NullString
		title     sql.NullString
		created   sql.NullInt64
		updated   sql.NullInt64
		count     int64
	)
	if err := row.Scan(&meta.ID, &workspace, &title, &created, &updated, &count); err != nil {
		return Meta{}, err
	}
	meta.WorkspaceID = workspace.String
	meta.Title = title.String
	meta.CreatedAt = timeFromNanos(created.Int64)
	meta.UpdatedAt = timeFromNanos(updated.Int64)
	meta.MessageCount = int(count)
	return meta, nil
}

// scanEvents scans an event result set.
func scanEvents(rows *sql.Rows) ([]rig.SessionEvent, error) {
	var events []rig.SessionEvent
	for rows.Next() {
		var (
			event   rig.SessionEvent
			session sql.NullString
			kind    sql.NullString
			payload []byte
			created sql.NullInt64
		)
		if err := rows.Scan(&event.Seq, &session, &kind, &payload, &created); err != nil {
			return nil, fmt.Errorf("sqlstore: scan event: %w", err)
		}
		event.SessionID = session.String
		event.Type = kind.String
		if len(payload) > 0 {
			event.Payload = append(json.RawMessage(nil), payload...)
		}
		if created.Valid {
			event.Time = timeFromNanos(created.Int64)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlstore: scan events: %w", err)
	}
	return events, nil
}

// escapeLike escapes LIKE wildcards in a user-supplied search string.
// requireSessionID trims and validates one session ID argument.
func requireSessionID(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", errors.New("sqlstore: session id is required")
	}
	return id, nil
}

// nowNanos is the current UTC time in unix nanoseconds.
func nowNanos() int64 { return time.Now().UTC().UnixNano() }

// timeFromNanos converts stored unix nanoseconds to a UTC time (zero when
// unset).
func timeFromNanos(nanos int64) time.Time {
	if nanos == 0 {
		return time.Time{}
	}
	return time.Unix(0, nanos).UTC()
}
