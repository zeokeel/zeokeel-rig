package sqlite

// The session half of the store: the list a sidebar shows and the messages a run
// appends to. Every write is one transaction, so a crash leaves either the whole
// append or none of it — never a message without its session row, never a
// message count that disagrees with the rows.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	rig "github.com/zeokeel/zeokeel-rig"
)

// maxTitleRunes mirrors the directory store: a title is a summary of the first
// user message, never a paragraph, and it is cut on runes so a Chinese title is
// not split mid-character.
const maxTitleRunes = 120

// newSessionID mints a sortable id: milliseconds first, then randomness, so ids
// sort by creation while staying collision-free across processes.
func newSessionID(now time.Time) string {
	var suffix [6]byte
	_, _ = rand.Read(suffix[:])
	return fmt.Sprintf("%013d-%s", now.UnixMilli(), hex.EncodeToString(suffix[:]))
}

// titleFrom returns the trimmed, rune-bounded summary of a message's text.
func titleFrom(msg rig.Message) string {
	text := strings.TrimSpace(messageText(msg))
	if text == "" {
		return ""
	}
	runes := []rune(text)
	if len(runes) > maxTitleRunes {
		runes = runes[:maxTitleRunes]
	}
	return strings.TrimSpace(string(runes))
}

// messageText concatenates a message's text parts.
func messageText(msg rig.Message) string {
	var builder strings.Builder
	for _, part := range msg.Content {
		if part.Type == rig.PartText {
			builder.WriteString(part.Text)
		}
	}
	return builder.String()
}

// Create inserts a session and returns the row as stored. An empty ID is minted;
// an explicit ID keeps working (an importer relies on that).
func (s *Store) Create(ctx context.Context, meta Meta) (Meta, error) {
	if err := s.usable(); err != nil {
		return Meta{}, err
	}
	now := time.Now().UTC()
	if strings.TrimSpace(meta.ID) == "" {
		meta.ID = newSessionID(now)
	}
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = now
	}
	if meta.UpdatedAt.IsZero() {
		meta.UpdatedAt = meta.CreatedAt
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO rig_sessions(id, title, workspace_id, created_at, updated_at, message_count, bytes)
		 VALUES(?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO NOTHING`,
		meta.ID, meta.Title, meta.Workspace, meta.CreatedAt.UnixMilli(), meta.UpdatedAt.UnixMilli(), meta.Messages, meta.Bytes)
	if err != nil {
		return Meta{}, fmt.Errorf("sqlite: create session %q: %w", meta.ID, err)
	}
	return s.meta(ctx, meta.ID)
}

// Append writes one message and returns the updated row.
func (s *Store) Append(ctx context.Context, id string, msg rig.Message) (Meta, error) {
	return s.AppendBatch(ctx, id, msg)
}

// AppendBatch writes several messages in one transaction: all of them land, or
// none do. The session row is created when it is missing, which is what a run
// starting from a UI "new chat" needs.
func (s *Store) AppendBatch(ctx context.Context, id string, msgs ...rig.Message) (Meta, error) {
	if err := s.usable(); err != nil {
		return Meta{}, err
	}
	if len(msgs) == 0 {
		return s.meta(ctx, id)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Meta{}, fmt.Errorf("sqlite: append: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO rig_sessions(id, title, workspace_id, created_at, updated_at)
		 VALUES(?, '', '', ?, ?) ON CONFLICT(id) DO NOTHING`,
		id, now.UnixMilli(), now.UnixMilli()); err != nil {
		return Meta{}, fmt.Errorf("sqlite: append: ensure session %q: %w", id, err)
	}
	var next int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(ordinal)+1, 0) FROM rig_messages WHERE session_id = ?`, id).Scan(&next); err != nil {
		return Meta{}, fmt.Errorf("sqlite: append: next ordinal: %w", err)
	}
	added := 0
	for index, msg := range msgs {
		payload, err := json.Marshal(msg)
		if err != nil {
			return Meta{}, fmt.Errorf("sqlite: append: encode message: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO rig_messages(session_id, ordinal, payload) VALUES(?, ?, ?)`,
			id, next+index, payload); err != nil {
			return Meta{}, fmt.Errorf("sqlite: append: insert message: %w", err)
		}
		added++
	}
	// The title is the first user message and never changes afterwards: a
	// sidebar that renamed itself mid-conversation would be disorienting.
	if _, err := tx.ExecContext(ctx,
		`UPDATE rig_sessions SET title = ? WHERE id = ? AND title = ''`,
		firstUserTitle(msgs), id); err != nil {
		return Meta{}, fmt.Errorf("sqlite: append: title: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE rig_sessions SET updated_at = ?, revision = revision + 1,
		        message_count = message_count + ? WHERE id = ?`,
		now.UnixMilli(), added, id); err != nil {
		return Meta{}, fmt.Errorf("sqlite: append: update session: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Meta{}, fmt.Errorf("sqlite: append: commit: %w", err)
	}
	return s.meta(ctx, id)
}

// firstUserTitle returns the title a batch implies, or "" when the batch has no
// user message with text (an assistant-only opening turn keeps the placeholder).
func firstUserTitle(msgs []rig.Message) string {
	for _, msg := range msgs {
		if msg.Role != rig.RoleUser {
			continue
		}
		if title := titleFrom(msg); title != "" {
			return title
		}
	}
	return ""
}

// Load returns one session and its messages in order.
func (s *Store) Load(ctx context.Context, id string) (Meta, []rig.Message, error) {
	if err := s.usable(); err != nil {
		return Meta{}, nil, err
	}
	meta, err := s.meta(ctx, id)
	if err != nil {
		return Meta{}, nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT payload FROM rig_messages WHERE session_id = ? ORDER BY ordinal`, id)
	if err != nil {
		return Meta{}, nil, fmt.Errorf("sqlite: load %q: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	var messages []rig.Message
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return Meta{}, nil, fmt.Errorf("sqlite: load %q: %w", id, err)
		}
		var msg rig.Message
		if err := json.Unmarshal(payload, &msg); err != nil {
			return Meta{}, nil, fmt.Errorf("sqlite: load %q: decode: %w", id, err)
		}
		messages = append(messages, msg)
	}
	if err := rows.Err(); err != nil {
		return Meta{}, nil, fmt.Errorf("sqlite: load %q: %w", id, err)
	}
	return meta, messages, nil
}

// meta reads one session row.
func (s *Store) meta(ctx context.Context, id string) (Meta, error) {
	var (
		meta      Meta
		createdAt int64
		updatedAt int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, title, workspace_id, created_at, updated_at, message_count, bytes
		 FROM rig_sessions WHERE id = ?`, id).
		Scan(&meta.ID, &meta.Title, &meta.Workspace, &createdAt, &updatedAt, &meta.Messages, &meta.Bytes)
	if errors.Is(err, sql.ErrNoRows) {
		return Meta{}, ErrNotFound
	}
	if err != nil {
		return Meta{}, fmt.Errorf("sqlite: read session %q: %w", id, err)
	}
	meta.CreatedAt = time.UnixMilli(createdAt).UTC()
	meta.UpdatedAt = time.UnixMilli(updatedAt).UTC()
	return meta, nil
}

// List returns sessions newest-updated first, honouring the filter. It reads the
// sessions table only: the count is a column, so listing a thousand sessions
// never decodes a message.
func (s *Store) List(ctx context.Context, filter Filter) ([]Meta, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	query := `SELECT id, title, workspace_id, created_at, updated_at, message_count, bytes FROM rig_sessions`
	var (
		where []string
		args  []any
	)
	if filter.Workspace != "" {
		where = append(where, "workspace_id = ?")
		args = append(args, filter.Workspace)
	}
	if !filter.Since.IsZero() {
		where = append(where, "updated_at >= ?")
		args = append(args, filter.Since.UnixMilli())
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY updated_at DESC, revision DESC"
	if filter.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, filter.Limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var metas []Meta
	for rows.Next() {
		var (
			meta      Meta
			createdAt int64
			updatedAt int64
		)
		if err := rows.Scan(&meta.ID, &meta.Title, &meta.Workspace, &createdAt, &updatedAt, &meta.Messages, &meta.Bytes); err != nil {
			return nil, fmt.Errorf("sqlite: list sessions: %w", err)
		}
		meta.CreatedAt = time.UnixMilli(createdAt).UTC()
		meta.UpdatedAt = time.UnixMilli(updatedAt).UTC()
		metas = append(metas, meta)
	}
	return metas, rows.Err()
}

// Delete removes a session and, by cascade, its messages, events and checkpoint.
// Deleting a session that is not there is not an error: a double click must not
// fail.
func (s *Store) Delete(ctx context.Context, id string) error {
	if err := s.usable(); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM rig_sessions WHERE id = ?`, id); err != nil {
		return fmt.Errorf("sqlite: delete session %q: %w", id, err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM rig_events WHERE session_id = ?`, id); err != nil {
		return fmt.Errorf("sqlite: delete events of %q: %w", id, err)
	}
	return nil
}

// Replace swaps a session's whole history in one transaction: compaction writes
// the summary and drops the messages it replaced, and a crash must not leave
// half of each.
func (s *Store) Replace(ctx context.Context, id string, msgs []rig.Message) (Meta, error) {
	if err := s.usable(); err != nil {
		return Meta{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Meta{}, fmt.Errorf("sqlite: replace: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM rig_messages WHERE session_id = ?`, id); err != nil {
		return Meta{}, fmt.Errorf("sqlite: replace: clear: %w", err)
	}
	for index, msg := range msgs {
		payload, err := json.Marshal(msg)
		if err != nil {
			return Meta{}, fmt.Errorf("sqlite: replace: encode: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO rig_messages(session_id, ordinal, payload) VALUES(?, ?, ?)`,
			id, index, payload); err != nil {
			return Meta{}, fmt.Errorf("sqlite: replace: insert: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE rig_sessions SET message_count = ?, updated_at = ?, revision = revision + 1 WHERE id = ?`,
		len(msgs), time.Now().UTC().UnixMilli(), id); err != nil {
		return Meta{}, fmt.Errorf("sqlite: replace: update: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Meta{}, fmt.Errorf("sqlite: replace: commit: %w", err)
	}
	return s.meta(ctx, id)
}

// usable reports the closed state instead of letting a nil handle panic.
func (s *Store) usable() error {
	if s == nil || s.db == nil {
		return ErrClosed
	}
	return nil
}
