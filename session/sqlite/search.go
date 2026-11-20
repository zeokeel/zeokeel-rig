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
	// Mode 选择匹配与排序的路径。零值 SearchSubstring 就是历史行为；SearchFTS
	// 走 FTS5 trigram 索引 + bm25 相关度排序，并额外索引 CJK 的 unigram/bigram。
	// 下列两种情况自动降级为 substring（命中仍然正确，只是没有 bm25）：
	//   - 本构建的 SQLite 没有 FTS5（见 Store.FTS5Available）；
	//   - 构建带 FTS5、但这个库的索引没建好（见 Store.SearchIndexReady）。
	Mode SearchMode
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

// SearchMessages 跨会话查找命中的消息。
//
// 匹配走 `search_text`（写入时由 searchTextFor 解码好的一列），而不是
// `CAST(payload AS TEXT)`：后者看到的是 JSON 转义后的文本，正文里的引号、
// 尖括号、&、换行与 tool_call 参数里的引号一律失配。
//
// 升级前写入的老行 `search_text` 为空，这里用原 payload 兜底，避免"升一次级，
// 整段历史搜不到"。兜底那一支要全表扫（没有可用的索引），补齐之后老行会走
// 正常分支——见 BackfillSearchText。
//
// # 两条路径、两套排序
//
//   - `SearchSubstring`（零值）：LIKE 子串匹配，长度不限；排序用"命中次数 +
//     首次出现位置 + 文本长度"的轻量打分（见 substringOrder）。
//   - `SearchFTS`：FTS5 trigram 索引 + CJK 词表列，排序用 `bm25(...)` 升序
//     （bm25 越小越相关）。查询撑不满 trigram 窗口（1~2 个 ASCII 字符）时退回
//     substring——否则这类查询会在 fts 模式里静默零命中。
//
// 两种模式的 `ORDER BY` 都以 `(session_id, ordinal)` 收尾，并且**与 OFFSET 同源**
// （同一段 SQL 表达式）：只按相关度排序而没有稳定次序键时，等分的两条命中在两次
// 查询里的先后可以不同，翻页就会重复一条、漏掉一条。
func (s *Store) SearchMessages(ctx context.Context, query SearchQuery) ([]MessageHit, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	text := strings.TrimSpace(query.Text)
	if text == "" {
		return nil, nil
	}
	// SearchIndexReady 而不只是 fts：构建带 FTS5、但**这个库**的索引没建好（交叉
	// 构建升级、被手工清空、回填失败）时，走索引会静默少命中甚至 0 条。宁可退回
	// substring（命中集合更大且正确），也不能"打开成功、检索空白"。
	if query.Mode == SearchFTS && s.SearchIndexReady() {
		if match := ftsMatchExpression(text); match != "" {
			return s.searchMessagesFTS(ctx, query, text, match)
		}
	}
	return s.searchMessagesSubstring(ctx, query, text)
}

// searchHaystack 是 substring 模式里用于打分的文本：解码列优先；老行（升级前写入、
// search_text 仍是空串）退回 payload——它们靠 LIKE 的兜底分支才进得来。
const searchHaystack = `COALESCE(NULLIF(m.search_text, ''), CAST(m.payload AS TEXT))`

// substringOrder 是 substring 模式的相关度，按重要性从高到低：
//
//  1. 命中次数（同一段文字里出现越多越相关）；
//  2. 命中是否落在解码列里（instr = 0 表示只出现在 payload 兜底文本里，排后）；
//  3. 首次出现位置（越靠前越相关）；
//  4. 文本长度（同分下短文本更聚焦）；
//  5. (session_id, ordinal) 稳定次序键。
//
// 大小写一律交给 SQLite 的 lower()（与 LIKE 的 ASCII 折叠同一套规则），避免"Go
// 侧小写化、SQL 侧按原文比较"这种两边规则不一致导致的排序抖动。
//
// 这几项都是纯 SQL 表达式，所以 LIMIT/OFFSET 与 ORDER BY 永远同源。
const substringOrder = `
	((length(lower(` + searchHaystack + `)) - length(replace(lower(` + searchHaystack + `), lower(?2), ''))) / length(lower(?2))) DESC,
	CASE WHEN instr(lower(` + searchHaystack + `), lower(?2)) = 0 THEN 1 ELSE 0 END ASC,
	instr(lower(` + searchHaystack + `), lower(?2)) ASC,
	length(` + searchHaystack + `) ASC,
	m.session_id, m.ordinal`

// searchMessagesSubstring 是零值模式（历史行为）的实现。
func (s *Store) searchMessagesSubstring(ctx context.Context, query SearchQuery, text string) ([]MessageHit, error) {
	limit := query.Limit
	if limit <= 0 {
		limit = DefaultSearchLimit
	}
	pattern := "%" + escapeLike(text) + "%"
	// ?1 出现两次是有意的：解码列与 payload 兜底用同一个 LIKE 模式；?2 是排序用的
	// 查询文本（大小写折叠在 SQL 里做）。参数按占位符编号显式绑定，后面的过滤条件
	// 从 ?3 起依次编号。
	sqlQuery := `SELECT m.session_id, s.title, m.ordinal, m.payload, m.search_text, s.updated_at
	             FROM rig_messages m JOIN rig_sessions s ON s.id = m.session_id
	             WHERE (m.search_text LIKE ?1 ESCAPE '\'
	                    OR (m.search_text = '' AND CAST(m.payload AS TEXT) LIKE ?1 ESCAPE '\'))`
	args := []any{pattern, text}
	next := 3
	if query.SessionID != "" {
		sqlQuery += fmt.Sprintf(` AND m.session_id = ?%d`, next)
		args = append(args, query.SessionID)
		next++
	}
	if query.AfterOrdinal > 0 {
		sqlQuery += fmt.Sprintf(` AND m.ordinal > ?%d`, next)
		args = append(args, query.AfterOrdinal)
		next++
	}
	sqlQuery += ` ORDER BY` + substringOrder
	sqlQuery += fmt.Sprintf(` LIMIT ?%d`, next)
	args = append(args, limit)
	next++
	if query.Offset > 0 {
		sqlQuery += fmt.Sprintf(` OFFSET ?%d`, next)
		args = append(args, query.Offset)
	}
	return s.scanMessageHits(ctx, sqlQuery, args, text)
}

// searchMessagesFTS 是 fts5 模式：MATCH 走 trigram 索引与 CJK 词表列，排序用 bm25。
//
// `bm25(...)` 返回的是**越小越相关**的负数（SQLite 的约定），所以是 ASC；等分时
// 由 (session_id, ordinal) 兜底，保证翻页稳定。
func (s *Store) searchMessagesFTS(ctx context.Context, query SearchQuery, text, match string) ([]MessageHit, error) {
	limit := query.Limit
	if limit <= 0 {
		limit = DefaultSearchLimit
	}
	// bm25() 只接受**真实表名**（别名会报 no such column），所以这里不写 `f`
	// 别名；索引表名与列名因此是同一个字符串，读起来也直白。
	sqlQuery := `SELECT m.session_id, s.title, m.ordinal, m.payload, m.search_text, s.updated_at
	             FROM rig_messages_fts
	             JOIN rig_messages m ON m.rowid = rig_messages_fts.rowid
	             JOIN rig_sessions s ON s.id = m.session_id
	             WHERE rig_messages_fts MATCH ?1`
	args := []any{match}
	next := 2
	if query.SessionID != "" {
		sqlQuery += fmt.Sprintf(` AND m.session_id = ?%d`, next)
		args = append(args, query.SessionID)
		next++
	}
	if query.AfterOrdinal > 0 {
		sqlQuery += fmt.Sprintf(` AND m.ordinal > ?%d`, next)
		args = append(args, query.AfterOrdinal)
		next++
	}
	sqlQuery += ` ORDER BY bm25(rig_messages_fts) ASC, m.session_id, m.ordinal`
	sqlQuery += fmt.Sprintf(` LIMIT ?%d`, next)
	args = append(args, limit)
	next++
	if query.Offset > 0 {
		sqlQuery += fmt.Sprintf(` OFFSET ?%d`, next)
		args = append(args, query.Offset)
	}
	return s.scanMessageHits(ctx, sqlQuery, args, text)
}

// scanMessageHits 执行一条已经排好序的检索语句并解出摘要。两种模式共用它，
// 命中行的列顺序因此只有一处定义。
func (s *Store) scanMessageHits(ctx context.Context, sqlQuery string, args []any, text string) ([]MessageHit, error) {
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

// BackfillSearchIndex 给老库补齐检索索引，返回补算过 CJK 词表的行数。
//
// 三步，**顺序不可交换**：
//  1. `INSERT INTO rig_messages_fts(rig_messages_fts) VALUES('rebuild')`：外部内容表
//     的标准重建命令，按 rig_messages 现有内容重排倒排索引。必须最先跑。
//  2. 补 `search_cjk`（bigram 只能由 Go 侧算，触发器不会分词）：它走 UPDATE，而
//     `au` 触发器的第一句是 `'delete'` 命令——删一条**索引里不存在**的记录，FTS5
//     直接判定索引损坏（`SQLITE_CORRUPT_VTAB: database disk image is malformed`）。
//     第 1 步让每一行都进了索引，delete 才有对应记录可删。
//  3. 再 rebuild 一次：让索引与最终的列值逐行一致。
//
// 旧实现把第 2 步放在第 1 步前面，于是"索引空/缺行"的库（虚拟表刚建出来、或由
// 无 FTS5 构建留下的库）在第一步 UPDATE 就炸掉，末尾的 rebuild 永不执行：检索对
// **任何**查询都返回 0 条，Replace/Delete 报 malformed。这就是这条用例
// TestBackfillSearchIndexRecoversFromEmptyIndex 守住的形状。
//
// 失败时把 SearchIndexReady 标成 false：SearchFTS 退回 substring（命中仍然正确），
// 宿主能看见"索引没修好"，而不是拿到一个永远空白的检索。
//
// 幂等：补算过的行不再满足 `search_cjk = ”`（纯 ASCII 行算出来也是空串，但它们的
// search_text 里没有 CJK，补算与不补算等价——游标照样前进，不会反复重扫同一批）；
// rebuild 重跑结果相同。调用方是 Open（升级提交之后），也可以由宿主手动再调。
func (s *Store) BackfillSearchIndex(ctx context.Context) (int, error) {
	if err := s.usable(); err != nil {
		return 0, err
	}
	if !s.fts {
		// 没有 FTS5 的构建：虚拟表不存在，rebuild 会报 no such table。CJK 列仍然
		// 补算（它对 substring 模式无害，且下次换成带 FTS5 的构建时直接可用）。
		return s.backfillSearchCJK(ctx)
	}
	// 顺序见函数注释：rebuild → 补列 → rebuild。
	if err := s.rebuildSearchIndex(ctx); err != nil {
		s.searchReady.Store(false)
		return 0, err
	}
	updated, err := s.backfillSearchCJK(ctx)
	if err != nil {
		s.searchReady.Store(false)
		return updated, err
	}
	if err := s.rebuildSearchIndex(ctx); err != nil {
		s.searchReady.Store(false)
		return updated, err
	}
	s.searchReady.Store(true)
	return updated, nil
}

// rebuildSearchIndex 用外部内容表的标准 `'rebuild'` 命令重排整份倒排索引。
//
// 幂等、与内容表逐行对齐：索引被手工删掉、或者列值刚被补算过之后都靠它收尾。
func (s *Store) rebuildSearchIndex(ctx context.Context) error {
	if !s.fts {
		return nil
	}
	err := retryBusy(ctx, func() error {
		if _, execErr := s.db.ExecContext(ctx,
			`INSERT INTO rig_messages_fts(rig_messages_fts) VALUES('rebuild')`); execErr != nil {
			return fmt.Errorf("rebuild fts index: %w", execErr)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("sqlite: rebuild search index: %w", err)
	}
	return nil
}

// backfillSearchCJK 只做第 1 步（补算 search_cjk），供两种构建共用。
func (s *Store) backfillSearchCJK(ctx context.Context) (int, error) {
	const batch = 500
	cursor := int64(-1)
	var total int
	for {
		type pendingRow struct {
			rowID int64
			text  string
		}
		pending := make([]pendingRow, 0, batch)
		rows, err := s.db.QueryContext(ctx,
			`SELECT rowid, search_text FROM rig_messages
			  WHERE search_cjk = '' AND search_text <> '' AND rowid > ?
			  ORDER BY rowid LIMIT ?`, cursor, batch)
		if err != nil {
			return total, fmt.Errorf("sqlite: backfill search cjk: %w", err)
		}
		for rows.Next() {
			var row pendingRow
			if err := rows.Scan(&row.rowID, &row.text); err != nil {
				_ = rows.Close()
				return total, fmt.Errorf("sqlite: backfill search cjk: %w", err)
			}
			pending = append(pending, row)
		}
		iterateErr := rows.Err()
		_ = rows.Close()
		if iterateErr != nil {
			return total, fmt.Errorf("sqlite: backfill search cjk: %w", iterateErr)
		}
		if len(pending) == 0 {
			return total, nil
		}
		cursor = pending[len(pending)-1].rowID
		var updated int
		err = retryBusy(ctx, func() error {
			tx, beginErr := s.db.BeginTx(ctx, nil)
			if beginErr != nil {
				return beginErr
			}
			defer func() { _ = tx.Rollback() }()
			// 重试时从 0 重新计数，否则同一批会被累加两遍。
			updated = 0
			for _, row := range pending {
				cjk := searchCJKFor(row.text)
				if cjk == "" {
					continue
				}
				if _, execErr := tx.ExecContext(ctx,
					`UPDATE rig_messages SET search_cjk = ? WHERE rowid = ? AND search_cjk = ''`,
					cjk, row.rowID); execErr != nil {
					return execErr
				}
				updated++
			}
			return tx.Commit()
		})
		if err != nil {
			return total, fmt.Errorf("sqlite: backfill search cjk: %w", err)
		}
		total += updated
	}
}

// searchColumnsFor 给一条消息算出两个可检索列：解码文本与 CJK 词表。
//
// 写入路径（Append/AppendBatch/Replace/ImportDir）共用它，保证"写进去的东西"与
// 检索声明的两个列永远同源。
func searchColumnsFor(msg rig.Message) (text, cjk string) {
	text = searchTextFor(msg)
	return text, searchCJKFor(text)
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
		switch part.Type {
		case rig.PartText:
			appendLine(part.Text)
		case rig.PartImageURL:
			// 图片分片没有正文，但 URL 是 payload 里本来就能被兜底分支搜到的字段：
			// 解码列如果漏了它，"升级前搜得到、升级后搜不到"，回填还会把这个命中
			// 永久抹掉。所以把 URL 也收进来（data URL 很长，但这里只存一份）。
			appendLine(part.ImageURL)
		default:
			appendLine(part.Text)
			appendLine(part.ImageURL)
		}
	}
	appendLine(msg.Reasoning)
	appendLine(msg.Name)
	// role=tool 的消息靠 tool_call_id 关联到调用，payload 兜底能搜到它。
	appendLine(msg.ToolCallID)
	for _, call := range msg.ToolCalls {
		appendLine(call.Name)
		appendLine(call.ID)
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
// backfillRow 是补齐批次里的一行。
type backfillRow struct {
	sessionID string
	ordinal   int
	payload   []byte
}

func (s *Store) BackfillSearchText(ctx context.Context) (int, error) {
	if err := s.usable(); err != nil {
		return 0, err
	}
	// 分批读写：老库可能有上百万条消息，一次性把 payload 读进内存会吃掉 GB 级内存
	// （升级那次 Open 正好是最没有余量的时候）。按 (session_id, ordinal) 游标推进，
	// 每批独立事务；解不出文本的行（纯图片/坏 payload）在这批里被跳过，游标越过它们
	// 之后不会被反复重扫。
	const backfillBatch = 500
	cursorSession, cursorOrdinal := "", -1
	var total int
	for {
		pending := make([]backfillRow, 0, backfillBatch)
		// 游标必须与 ORDER BY (session_id, ordinal) 的排序**完全一致**：早期版本用
		// `session_id || '#' || ordinal` 拼成一个字符串比较，而 '#' 之前的字符
		// （空格、'!'、'"' 等）会让拼接后的序与原序不同，于是某些会话被永久跳过。
		rows, err := s.db.QueryContext(ctx,
			`SELECT session_id, ordinal, payload FROM rig_messages
			  WHERE search_text = ''
			    AND (session_id > ?1 OR (session_id = ?1 AND ordinal > ?2))
			  ORDER BY session_id, ordinal LIMIT ?3`, cursorSession, cursorOrdinal, backfillBatch)
		if err != nil {
			return total, fmt.Errorf("sqlite: backfill search text: %w", err)
		}
		for rows.Next() {
			var row backfillRow
			if err := rows.Scan(&row.sessionID, &row.ordinal, &row.payload); err != nil {
				_ = rows.Close()
				return total, fmt.Errorf("sqlite: backfill search text: %w", err)
			}
			pending = append(pending, row)
		}
		iterateErr := rows.Err()
		_ = rows.Close()
		if iterateErr != nil {
			return total, fmt.Errorf("sqlite: backfill search text: %w", iterateErr)
		}
		if len(pending) == 0 {
			return total, nil
		}
		last := pending[len(pending)-1]
		cursorSession, cursorOrdinal = last.sessionID, last.ordinal
		updated, err := s.backfillBatch(ctx, pending)
		if err != nil {
			return total, err
		}
		total += updated
	}
}

// backfillBatch 在一个事务里补齐一批行，返回真正更新的行数。
//
// 先读完整批再写：在同一个连接上边遍历结果集边 UPDATE 会搅乱游标，也让 BUSY 重试
// 说不清"这次到底更新了几行"。
func (s *Store) backfillBatch(ctx context.Context, pending []backfillRow) (int, error) {
	var updated int
	err := retryBusy(ctx, func() error {
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
			text, cjk := searchColumnsFor(msg)
			if text == "" {
				continue
			}
			result, execErr := tx.ExecContext(ctx,
				`UPDATE rig_messages SET search_text = ?, search_cjk = ?
				 WHERE session_id = ? AND ordinal = ? AND search_text = ''`,
				text, cjk, row.sessionID, row.ordinal)
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
