package sqlstore

// Decoded search text: messages and event payloads are stored as JSON, but the
// search queries must match the decoded text (quotes and backslashes are user
// content, not escaping). The `search_text` column holds the decoded,
// case-insensitive match surface; the `text` column keeps the plain message text
// used for snippets.

import (
	"context"
	"encoding/json"
	"fmt"

	rig "github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/session/sqlsearch"
)

// encodedMessage is one message prepared for insertion: the JSON payload plus
// the decoded text columns.
type encodedMessage struct {
	payload    []byte
	text       string
	searchText string
}

// messageSearchTextColumns delegates the decode to the SDK (JSON escaping is not
// user content; tool arguments are decoded recursively).
func messageSearchTextColumns(message rig.Message) (text, searchText string) {
	return sqlsearch.MessageColumns(message)
}

// eventPayloadSearchText delegates the payload decode to the SDK.
func eventPayloadSearchText(payload []byte) string {
	return sqlsearch.EventText(payload)
}

// migrateSearchColumns adds the decoded-text columns to databases created
// before they existed and backfills every existing row once. SQLite has no ADD
// COLUMN IF NOT EXISTS, so the column list is inspected first; reopening an
// already-migrated database is a no-op.
func (s *Store) migrateSearchColumns(ctx context.Context) error {
	messageTextAdded, err := s.ensureColumn(ctx, "messages", "text", "TEXT NOT NULL DEFAULT ''")
	if err != nil {
		return err
	}
	messageSearchAdded, err := s.ensureColumn(ctx, "messages", "search_text", "TEXT NOT NULL DEFAULT ''")
	if err != nil {
		return err
	}
	eventTextAdded, err := s.ensureColumn(ctx, "events", "text", "TEXT NOT NULL DEFAULT ''")
	if err != nil {
		return err
	}
	if messageTextAdded || messageSearchAdded {
		if err := s.backfillMessageSearchText(ctx, ""); err != nil {
			return err
		}
	} else if err := s.backfillMessageSearchText(ctx, "WHERE COALESCE(search_text, '') = ''"); err != nil {
		// A partially migrated/older writer may have left the decoded column
		// empty; fill those rows on open without touching populated rows.
		return err
	}
	if eventTextAdded {
		if err := s.backfillEventSearchText(ctx, ""); err != nil {
			return err
		}
	} else if err := s.backfillEventSearchText(ctx, "WHERE COALESCE(text, '') = ''"); err != nil {
		return err
	}
	return nil
}

// ensureColumn adds one column when missing and reports whether it was added.
func (s *Store) ensureColumn(ctx context.Context, table, column, definition string) (bool, error) {
	exists, err := s.hasColumn(ctx, table, column)
	if err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	if _, err := s.db.ExecContext(ctx, "ALTER TABLE "+table+" ADD COLUMN "+column+" "+definition); err != nil {
		return false, fmt.Errorf("sqlstore: add %s.%s: %w", table, column, err)
	}
	return true, nil
}

// backfillMessageSearchText populates text/search_text for pre-migration rows
// from their JSON payloads. Rows with corrupt payloads get empty text; they were
// already skipped by the pre-fix search path.
func (s *Store) backfillMessageSearchText(ctx context.Context, where string) error {
	type row struct {
		id      int64
		payload []byte
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, payload FROM messages `+where)
	if err != nil {
		return fmt.Errorf("sqlstore: backfill message text: %w", err)
	}
	var pending []row
	for rows.Next() {
		var entry row
		if err := rows.Scan(&entry.id, &entry.payload); err != nil {
			_ = rows.Close()
			return fmt.Errorf("sqlstore: backfill message text: %w", err)
		}
		pending = append(pending, entry)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("sqlstore: backfill message text: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("sqlstore: backfill message text: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlstore: backfill message text: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	statement, err := tx.PrepareContext(ctx, `UPDATE messages SET text = ?, search_text = ? WHERE id = ?`)
	if err != nil {
		return fmt.Errorf("sqlstore: backfill message text: %w", err)
	}
	defer func() { _ = statement.Close() }()
	for _, entry := range pending {
		var message rig.Message
		if err := json.Unmarshal(entry.payload, &message); err != nil {
			continue
		}
		text, searchText := messageSearchTextColumns(message)
		if _, err := statement.ExecContext(ctx, text, searchText, entry.id); err != nil {
			return fmt.Errorf("sqlstore: backfill message text: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlstore: backfill message text: %w", err)
	}
	return nil
}

// backfillEventSearchText populates events.text from decoded payloads.
func (s *Store) backfillEventSearchText(ctx context.Context, where string) error {
	type row struct {
		seq     int64
		payload []byte
	}
	rows, err := s.db.QueryContext(ctx, `SELECT seq, payload FROM events `+where)
	if err != nil {
		return fmt.Errorf("sqlstore: backfill event text: %w", err)
	}
	var pending []row
	for rows.Next() {
		var entry row
		if err := rows.Scan(&entry.seq, &entry.payload); err != nil {
			_ = rows.Close()
			return fmt.Errorf("sqlstore: backfill event text: %w", err)
		}
		pending = append(pending, entry)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("sqlstore: backfill event text: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("sqlstore: backfill event text: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlstore: backfill event text: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	statement, err := tx.PrepareContext(ctx, `UPDATE events SET text = ? WHERE seq = ?`)
	if err != nil {
		return fmt.Errorf("sqlstore: backfill event text: %w", err)
	}
	defer func() { _ = statement.Close() }()
	for _, entry := range pending {
		if _, err := statement.ExecContext(ctx, eventPayloadSearchText(entry.payload), entry.seq); err != nil {
			return fmt.Errorf("sqlstore: backfill event text: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlstore: backfill event text: %w", err)
	}
	return nil
}
