package sqlite

// 目录存储答不了的查询：哪些会话提到过这段文字，以及它们各自在什么时候做过什么
// 类型的事件。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	rig "github.com/zeokeel/zeokeel-rig"
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
	// Offset 跳过前 N 条命中（按本次查询的排序），用于跨会话的翻页：宿主拿不到
	// "上一页最后一条的 ordinal 是哪个会话的"，用 Offset 更直接。
	Offset int
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
//
// 匹配走 `search_text`（写入时由 searchTextFor 解码好的一列），而不是
// `CAST(payload AS TEXT)`：后者看到的是 JSON 转义后的文本，正文里的引号、
// 尖括号、&、换行与 tool_call 参数里的引号一律失配。
//
// 升级前写入的老行 `search_text` 为空，这里用原 payload 兜底，避免"升一次级，
// 整段历史搜不到"。兜底那一支要全表扫（没有可用的索引），补齐之后老行会走
// 正常分支——见 BackfillSearchText。
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
	pattern := "%" + escapeLike(text) + "%"
	sqlQuery := `SELECT m.session_id, s.title, m.ordinal, m.payload, m.search_text, s.updated_at
	             FROM rig_messages m JOIN rig_sessions s ON s.id = m.session_id
	             WHERE (m.search_text LIKE ? ESCAPE '\'
	                    OR (m.search_text = '' AND CAST(m.payload AS TEXT) LIKE ? ESCAPE '\'))`
	args := []any{pattern, pattern}
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
	if query.Offset > 0 {
		sqlQuery += ` OFFSET ?`
		args = append(args, query.Offset)
	}
	rows, err := s.db.QueryContext(ctx, sqlQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: search messages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var hits []MessageHit
	for rows.Next() {
		var (
			hit        MessageHit
			payload    []byte
			searchText string
			updatedAt  int64
		)
		if err := rows.Scan(&hit.SessionID, &hit.Title, &hit.Ordinal, &payload, &searchText, &updatedAt); err != nil {
			return nil, fmt.Errorf("sqlite: search messages: %w", err)
		}
		hit.Time = time.UnixMilli(updatedAt).UTC()
		hit.Role, hit.Snippet = decodeHit(payload, searchText, text)
		hits = append(hits, hit)
	}
	return hits, rows.Err()
}

// searchTextFor 把一条消息解码成一段可检索文本：正文分片、推理、工具名，以及
// 每个 tool_call 的参数（JSON 字符串原样拼上）。
//
// 它只在**写入**时调用（Append/AppendBatch/Replace），结果落进
// `rig_messages.search_text`；查询因此不必再面对 payload 的 JSON 转义。
// 参数按原样拼接（模型产出的就是 JSON 文本），所以 `{"command":"git status"}`
// 里的 `"command":"git` 也能被搜到。
//
// 解不出任何文本（例如只有图片分片）时返回空串：写入路径**不因此失败**，这类行
// 走 SearchMessages 的 payload 兜底分支。
func searchTextFor(msg rig.Message) string {
	var builder strings.Builder
	appendLine := func(value string) {
		if value == "" {
			return
		}
		if builder.Len() > 0 {
			builder.WriteByte('\n')
		}
		builder.WriteString(value)
	}
	for _, part := range msg.Content {
		// 图片分片只有 URL，正文检索没有意义；文本分片才是用户写下的字。
		if part.Type == rig.PartText {
			appendLine(part.Text)
		}
	}
	appendLine(msg.Reasoning)
	appendLine(msg.Name)
	for _, call := range msg.ToolCalls {
		appendLine(call.Name)
		appendLine(call.Arguments)
	}
	return builder.String()
}

// BackfillSearchText 给老库做一次性补齐：扫描 `search_text` 仍为空的消息，按
// payload 解码回填，返回真正更新的行数。
//
// 幂等：解码不出文本的行（图片分片、坏 payload）保持空串、不计入更新，所以第二次
// 调用在没有新写入时返回 0（把空串写回空串等于什么都没做，还会让"再跑一次"永远
// 非零）。坏 payload 也不算错误——回填不是修复数据的地方，写入路径照样能读。
func (s *Store) BackfillSearchText(ctx context.Context) (int, error) {
	if err := s.usable(); err != nil {
		return 0, err
	}
	type pendingRow struct {
		sessionID string
		ordinal   int
		payload   []byte
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT session_id, ordinal, payload FROM rig_messages WHERE search_text = ''`)
	if err != nil {
		return 0, fmt.Errorf("sqlite: backfill search text: %w", err)
	}
	var pending []pendingRow
	for rows.Next() {
		var row pendingRow
		if err := rows.Scan(&row.sessionID, &row.ordinal, &row.payload); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("sqlite: backfill search text: %w", err)
		}
		pending = append(pending, row)
	}
	iterateErr := rows.Err()
	_ = rows.Close()
	if iterateErr != nil {
		return 0, fmt.Errorf("sqlite: backfill search text: %w", iterateErr)
	}
	if len(pending) == 0 {
		return 0, nil
	}
	// 先读完再写：在同一个连接上边遍历结果集边 UPDATE 会把游标搅乱，也让
	// BUSY 重试说不清"这次事务到底更新了几行"。
	var updated int
	err = retryBusy(ctx, func() error {
		tx, beginErr := s.db.BeginTx(ctx, nil)
		if beginErr != nil {
			return beginErr
		}
		defer func() { _ = tx.Rollback() }()
		// 重试时重新计数，否则同一次补齐会被累加两遍。
		updated = 0
		for _, row := range pending {
			var msg rig.Message
			if decodeErr := json.Unmarshal(row.payload, &msg); decodeErr != nil {
				continue
			}
			text := searchTextFor(msg)
			if text == "" {
				continue
			}
			result, execErr := tx.ExecContext(ctx,
				`UPDATE rig_messages SET search_text = ?
				 WHERE session_id = ? AND ordinal = ? AND search_text = ''`,
				text, row.sessionID, row.ordinal)
			if execErr != nil {
				return execErr
			}
			if affected, affectedErr := result.RowsAffected(); affectedErr == nil && affected > 0 {
				updated++
			}
		}
		return tx.Commit()
	})
	if err != nil {
		return 0, fmt.Errorf("sqlite: backfill search text: %w", err)
	}
	return updated, nil
}

// decodeHit pulls the role and a snippet out of a stored message.
//
// 摘要优先从正文分片里取；命中只出现在推理或工具参数里时（正文里没有针），退回
// 解码列 searchText，否则这类命中会带着空摘要出现在列表里。
func decodeHit(payload []byte, searchText, needle string) (role, snippet string) {
	var msg struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(payload, &msg); err != nil {
		return "", snippetAround(searchText, needle)
	}
	role = msg.Role
	var builder strings.Builder
	for _, part := range msg.Content {
		if part.Type == "text" {
			builder.WriteString(part.Text)
		}
	}
	haystack := builder.String()
	if indexRunes([]rune(strings.ToLower(haystack)), []rune(strings.ToLower(needle))) < 0 {
		haystack = searchText
	}
	return role, snippetAround(haystack, needle)
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
