package sqlite

// Queries the directory store cannot answer: which conversations mention this
// text, and which of them did what kind of event, when.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// SearchQuery narrows a message search.
type SearchQuery struct {
	// Text matches the message body, case-insensitively. LIKE wildcards in the
	// text are matched literally: an underscore is an underscore.
	Text string
	// SessionID limits the search to one conversation; empty searches all.
	SessionID string
	// AfterOrdinal continues a previous page: only messages whose ordinal is
	// greater are returned. Messages are read session by session, so a page is
	// only meaningful within one session; use SessionID with it.
	AfterOrdinal int
	// Limit caps the result set; 0 uses DefaultSearchLimit.
	Limit int
}

// MessageHit is one matching message with the context a list needs.
type MessageHit struct {
	SessionID string
	Title     string
	Ordinal   int
	Role      string
	Time      time.Time
	// Snippet is the matching text with a little context on each side.
	Snippet string
}

// EventQuery narrows an event search.
type EventQuery struct {
	// SessionID limits the search to one conversation; empty searches all.
	SessionID string
	// Type matches the event kind exactly (empty matches every kind).
	Type string
	// Since and Until bound the event time; zero values are unbounded.
	Since time.Time
	Until time.Time
	// Text matches the payload text; empty matches every payload.
	Text string
	// Limit caps the result set; 0 uses DefaultSearchLimit.
	Limit int
}

// DefaultSearchLimit caps a search that did not ask for a limit.
const DefaultSearchLimit = 50

// snippetContext is how many runes of context each side of a match keeps.
const snippetContext = 48

// SearchMessages finds matching messages across sessions, newest first.
func (s *Store) SearchMessages(ctx context.Context, query SearchQuery) ([]MessageHit, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	text := strings.TrimSpace(query.Text)
	if text == "" {
		return nil, nil
	}
	limit := query.Limit
	if limit <= 0 {
		limit = DefaultSearchLimit
	}
	sqlQuery := `SELECT m.session_id, s.title, m.ordinal, m.payload, s.updated_at
	             FROM rig_messages m JOIN rig_sessions s ON s.id = m.session_id
	             WHERE CAST(m.payload AS TEXT) LIKE ? ESCAPE '\'`
	args := []any{"%" + escapeLike(text) + "%"}
	if query.SessionID != "" {
		sqlQuery += ` AND m.session_id = ?`
		args = append(args, query.SessionID)
	}
	if query.AfterOrdinal > 0 {
		sqlQuery += ` AND m.ordinal > ?`
		args = append(args, query.AfterOrdinal)
	}
	sqlQuery += ` ORDER BY s.updated_at DESC, m.session_id, m.ordinal LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, sqlQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: search messages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var hits []MessageHit
	for rows.Next() {
		var (
			hit       MessageHit
			payload   []byte
			updatedAt int64
		)
		if err := rows.Scan(&hit.SessionID, &hit.Title, &hit.Ordinal, &payload, &updatedAt); err != nil {
			return nil, fmt.Errorf("sqlite: search messages: %w", err)
		}
		hit.Time = time.UnixMilli(updatedAt).UTC()
		hit.Role, hit.Snippet = decodeHit(payload, text)
		hits = append(hits, hit)
	}
	return hits, rows.Err()
}

// decodeHit pulls the role and a snippet out of a stored message.
func decodeHit(payload []byte, needle string) (role, snippet string) {
	var msg struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(payload, &msg); err != nil {
		return "", ""
	}
	role = msg.Role
	var builder strings.Builder
	for _, part := range msg.Content {
		if part.Type == "text" {
			builder.WriteString(part.Text)
		}
	}
	return role, snippetAround(builder.String(), needle)
}

// snippetAround returns the match with context, marked by an ellipsis when the
// text was cut. Case-insensitive, like the search that produced it.
func snippetAround(text, needle string) string {
	if text == "" {
		return ""
	}
	runes := []rune(text)
	lowerRunes := []rune(strings.ToLower(text))
	lowerNeedle := []rune(strings.ToLower(needle))
	index := indexRunes(lowerRunes, lowerNeedle)
	if index < 0 {
		return string(runes[:min(len(runes), 2*snippetContext)])
	}
	start := max(0, index-snippetContext)
	end := min(len(runes), index+len(lowerNeedle)+snippetContext)
	snippet := string(runes[start:end])
	if start > 0 {
		snippet = "…" + snippet
	}
	if end < len(runes) {
		snippet += "…"
	}
	return strings.TrimSpace(snippet)
}

// indexRunes finds needle in haystack (both already lower-cased).
func indexRunes(haystack, needle []rune) int {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return -1
	}
	for start := 0; start+len(needle) <= len(haystack); start++ {
		match := true
		for offset := range needle {
			if haystack[start+offset] != needle[offset] {
				match = false
				break
			}
		}
		if match {
			return start
		}
	}
	return -1
}

// SearchEvents finds events by kind, time and payload text.
func (s *Store) SearchEvents(ctx context.Context, query EventQuery) ([]EventHit, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	limit := query.Limit
	if limit <= 0 {
		limit = DefaultSearchLimit
	}
	sqlQuery := `SELECT session_id, seq, kind, at, payload FROM rig_events WHERE 1 = 1`
	var args []any
	if query.SessionID != "" {
		sqlQuery += ` AND session_id = ?`
		args = append(args, query.SessionID)
	}
	if query.Type != "" {
		sqlQuery += ` AND kind = ?`
		args = append(args, query.Type)
	}
	if !query.Since.IsZero() {
		sqlQuery += ` AND at >= ?`
		args = append(args, query.Since.UnixMilli())
	}
	if !query.Until.IsZero() {
		sqlQuery += ` AND at <= ?`
		args = append(args, query.Until.UnixMilli())
	}
	if text := strings.TrimSpace(query.Text); text != "" {
		sqlQuery += ` AND CAST(payload AS TEXT) LIKE ? ESCAPE '\'`
		args = append(args, "%"+escapeLike(text)+"%")
	}
	sqlQuery += ` ORDER BY at DESC, seq DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, sqlQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: search events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var hits []EventHit
	for rows.Next() {
		var (
			hit EventHit
			at  int64
		)
		if err := rows.Scan(&hit.SessionID, &hit.Seq, &hit.Type, &at, &hit.Payload); err != nil {
			return nil, fmt.Errorf("sqlite: search events: %w", err)
		}
		hit.Time = time.UnixMilli(at).UTC()
		hits = append(hits, hit)
	}
	return hits, rows.Err()
}

// EventHit is one matching event.
type EventHit struct {
	SessionID string
	Seq       int64
	Type      string
	Time      time.Time
	Payload   []byte
}

// escapeLike makes LIKE treat its wildcards literally.
func escapeLike(text string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(text)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
