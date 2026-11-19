package sqlite

// 目录存储答不了的查询：哪些会话提到过这段文字，以及它们各自在什么时候做过什么
// 类型的事件。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// SearchQuery 收窄一次消息检索。
type SearchQuery struct {
	// Text 匹配消息正文，大小写不敏感。文本里的 LIKE 通配符按**字面量**匹配：
	// 一个下划线就是一个下划线。
	Text string
	// SessionID 限定到某一个会话；空表示在所有会话里找。
	SessionID string
	// AfterOrdinal 续接上一页：只返回 ordinal 更大的消息。消息是**按会话**读的，
	// 所以分页只在单个会话内有意义——请与 SessionID 一起用。
	AfterOrdinal int
	// Limit 限制结果条数；0 使用 DefaultSearchLimit。
	Limit int
}

// MessageHit 是一条命中的消息，带上列表展示需要的上下文。
type MessageHit struct {
	SessionID string
	Title     string
	Ordinal   int
	Role      string
	Time      time.Time
	// Snippet 是命中文本及其两侧的一小段上下文。
	Snippet string
}

// EventQuery 收窄一次事件检索。
type EventQuery struct {
	// SessionID 限定到某一个会话；空表示在所有会话里找。
	SessionID string
	// Type 精确匹配事件种类（空表示匹配所有种类）。
	Type string
	// Since / Until 限定事件时间；零值表示不限。
	Since time.Time
	Until time.Time
	// Text 匹配 payload 文本；空表示匹配所有 payload。
	Text string
	// Limit 限制结果条数；0 使用 DefaultSearchLimit。
	Limit int
}

// DefaultSearchLimit 是没指定条数时一次检索的上限。
const DefaultSearchLimit = 50

// snippetContext 是命中位置两侧各保留的上下文字符数。
const snippetContext = 48

// SearchMessages 跨会话查找命中的消息，最新的在前。
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

// escapeLike 让 LIKE 把通配符当成普通字符。
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
