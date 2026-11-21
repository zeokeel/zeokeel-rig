package sqlstore

// Cross-session message full-text search: the durable half behind the old
// shell's session search. The SQL matches the decoded search_text column
// (message text, reasoning and decoded tool-call values — never raw JSON
// escaping) plus the session title, newest message first; snippets are built
// from the plain text column.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	rig "github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/session/sqlsearch"

	)

const (
	// defaultSearchMessageLimit is used for limit <= 0.
	defaultSearchMessageLimit = 50
	// maxSearchMessageLimit caps one SearchMessages result set.
	maxSearchMessageLimit = 200
	// unnamedSessionTitle is shown when a session has no display title.
	unnamedSessionTitle = "Untitled session"
)

var _ Searcher = (*Store)(nil)

// SearchMessages implements Searcher. It finds messages whose
// payload text contains query (case-insensitive), newest first, bounded by
// limit. LIKE wildcards in query are matched literally.
func (s *Store) SearchMessages(ctx context.Context, query string, limit int) ([]Hit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return []Hit{}, nil
	}
	if limit <= 0 {
		limit = defaultSearchMessageLimit
	}
	if limit > maxSearchMessageLimit {
		limit = maxSearchMessageLimit
	}
	pattern := sqlsearch.EscapeLike(trimmed)
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.session_id, m.ordinal, m.payload, m.text, m.created_at, COALESCE(s.title, '')
		FROM messages m
		JOIN sessions s ON s.id = m.session_id
		WHERE LOWER(m.text) LIKE '%' || LOWER(?) || '%' ESCAPE '\'
		   OR LOWER(m.search_text) LIKE '%' || LOWER(?) || '%' ESCAPE '\'
		   OR LOWER(COALESCE(s.title, '')) LIKE '%' || LOWER(?) || '%' ESCAPE '\'
		ORDER BY m.id DESC
		LIMIT ?`, pattern, pattern, pattern, limit)
	if err != nil {
		return nil, fmt.Errorf("sqlstore: search messages: %w", err)
	}
	defer rows.Close()

	hits := make([]Hit, 0, limit)
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var (
			sessionID string
			ordinal   sql.NullInt64
			payload   []byte
			text      string
			created   sql.NullInt64
			title     string
		)
		if err := rows.Scan(&sessionID, &ordinal, &payload, &text, &created, &title); err != nil {
			return nil, fmt.Errorf("sqlstore: scan message search hit: %w", err)
		}
		var message rig.Message
		if err := json.Unmarshal(payload, &message); err != nil {
			// Corrupt payloads are not searchable text; skip instead of failing
			// the whole query.
			continue
		}
		if strings.TrimSpace(title) == "" {
			title = unnamedSessionTitle
		}
		ordinalValue := 0
		if ordinal.Valid && ordinal.Int64 > 0 {
			ordinalValue = int(ordinal.Int64)
		}
		hits = append(hits, Hit{
			SessionID:    sessionID,
			Ordinal:      ordinalValue,
			SessionTitle: title,
			Role:         string(message.Role),
			Snippet:      sqlsearch.Snippet(text, trimmed),
			CreatedAt:    timeFromNanos(created.Int64),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlstore: search messages: %w", err)
	}
	return hits, nil
}
