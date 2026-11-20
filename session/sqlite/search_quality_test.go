package sqlite_test

// 检索质量（E2）：模式、相关度、CJK 分词与翻页。
//
// 这一组用例钉住的是"搜得到、排得对、翻得干净"三件事：
//   - 两种模式（substring / fts5）在普通 ASCII 文本上**命中集合相同**——模式只该
//     影响排序与能力，不该让同一句话在一种模式里消失；
//   - fts5 模式按 bm25 相关度排序，高频命中的那条排第一；
//   - CJK 没有空格，2 字词（"磁盘"）必须命中，靠的是写入时算好的 bigram 辅助列；
//   - 翻页（Offset）与排序**同源**：跨会话多次翻页不重不漏，两种模式都成立。

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	rig "github.com/zeokeel/zeokeel-rig"
	sqlitestore "github.com/zeokeel/zeokeel-rig/session/sqlite"
)

// hitKeys 把一次检索的结果压成 (session, ordinal) 的稳定键：跨模式比较命中集合时
// 只关心"哪些消息回来了"，不关心顺序。
func hitKeys(hits []sqlitestore.MessageHit) []string {
	keys := make([]string, 0, len(hits))
	for _, hit := range hits {
		keys = append(keys, hit.SessionID+"#"+itoa(hit.Ordinal))
	}
	return keys
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

// sameKeys 报告两个键集合是否完全相同（顺序无关、无重复）。
func sameKeys(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	seen := make(map[string]int, len(left))
	for _, key := range left {
		seen[key]++
	}
	for _, key := range right {
		seen[key]--
		if seen[key] < 0 {
			return false
		}
	}
	return true
}

// TestSearchModesReturnSameHitsForPlainAscii：对普通 ASCII 文本，"哪些消息命中"
// 不该取决于模式——substring 与 fts5（trigram）命中集合必须一致。
func TestSearchModesReturnSameHitsForPlainAscii(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	if !db.FTS5Available() {
		t.Skip("本次构建的 SQLite 没有 FTS5：fts5 模式会降级为 substring，两条路径必然相同")
	}
	ctx := context.Background()
	first, _ := db.Create(ctx, sqlitestore.Meta{Title: "发布"})
	second, _ := db.Create(ctx, sqlitestore.Meta{Title: "其它"})
	if _, err := db.AppendBatch(ctx, first.ID,
		userMessage("the release checklist is ready"),
		assistantMessage("weather looks fine today")); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if _, err := db.Append(ctx, second.ID, userMessage("ship the release notes")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	for _, text := range []string{"release", "checklist", "release notes"} {
		substring, err := db.SearchMessages(ctx, sqlitestore.SearchQuery{Text: text})
		if err != nil {
			t.Fatalf("substring search %q: %v", text, err)
		}
		fts, err := db.SearchMessages(ctx, sqlitestore.SearchQuery{Text: text, Mode: sqlitestore.SearchFTS})
		if err != nil {
			t.Fatalf("fts search %q: %v", text, err)
		}
		if !sameKeys(hitKeys(substring), hitKeys(fts)) {
			t.Fatalf("模式间命中集合不同（%q）：substring=%v fts=%v", text, hitKeys(substring), hitKeys(fts))
		}
		if len(substring) == 0 {
			t.Fatalf("%q 一条都没命中：用例本身失效，无法比较两种模式", text)
		}
	}
}

// TestSearchFTSRanksByRelevance：三条消息，只有一条高频命中 → 它排第一。
//
// 排序是检索质量的另一半：旧实现固定 `ORDER BY updated_at DESC`，于是"最相关的那条"
// 可能落在第 47 位；bm25 让高频命中浮到顶部。
func TestSearchFTSRanksByRelevance(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	if !db.FTS5Available() {
		t.Skip("本次构建的 SQLite 没有 FTS5")
	}
	ctx := context.Background()
	created, _ := db.Create(ctx, sqlitestore.Meta{})
	if _, err := db.AppendBatch(ctx, created.ID,
		userMessage("please deploy the build now"),
		userMessage("deploy deploy deploy deploy the build now"),
		userMessage("nothing to see here")); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	hits, err := db.SearchMessages(ctx, sqlitestore.SearchQuery{Text: "deploy", Mode: sqlitestore.SearchFTS})
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits = %d, want 2（第三句不含 deploy）", len(hits))
	}
	if hits[0].Ordinal != 1 {
		t.Fatalf("榜首 ordinal = %d, want 1（高频命中的那条）：%+v", hits[0].Ordinal, hitKeys(hits))
	}
}

// TestSearchSubstringRanksByHitCount：substring 模式也有相关度——命中次数多的
// 那条排第一（历史行为是 `ORDER BY updated_at DESC`，跟"多相关"毫无关系）。
func TestSearchSubstringRanksByHitCount(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	created, _ := db.Create(ctx, sqlitestore.Meta{})
	if _, err := db.AppendBatch(ctx, created.ID,
		userMessage("please deploy the build now"),
		userMessage("deploy deploy deploy the build now"),
		userMessage("nothing to see here")); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	hits, err := db.SearchMessages(ctx, sqlitestore.SearchQuery{Text: "deploy"})
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits = %d, want 2", len(hits))
	}
	if hits[0].Ordinal != 1 {
		t.Fatalf("榜首 ordinal = %d, want 1（高频命中的那条）：%v", hits[0].Ordinal, hitKeys(hits))
	}
}

// TestSearchCJKMatchesBigram：中文没有空格，2 字词必须命中。
//
// trigram 只看 3 字符窗口，2 字 CJK 词自身撑不满一个窗口，所以写入时另存一列
// unigram + bigram 的辅助文本；查询侧用同一套词切分。
func TestSearchCJKMatchesBigram(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	if !db.FTS5Available() {
		t.Skip("本次构建的 SQLite 没有 FTS5")
	}
	ctx := context.Background()
	created, _ := db.Create(ctx, sqlitestore.Meta{})
	if _, err := db.Append(ctx, created.ID, userMessage("我们把文件写到磁盘上")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// 辅助列必须**在写入时**就算好：触发器不会分词，等检索时再算就晚了。这一条
	// 同时排除"命中其实来自 substring 降级"的可能——没有词表，fts 模式对 2 字查询
	// 只能靠降级，那这条用例就守不住任何东西。
	var storedCJK string
	if err := db.DB().QueryRowContext(ctx,
		`SELECT search_cjk FROM rig_messages WHERE session_id = ?`, created.ID).Scan(&storedCJK); err != nil {
		t.Fatalf("read search_cjk: %v", err)
	}
	for _, token := range []string{"#磁盘#", "#盘上#", "#盘#"} {
		if !strings.Contains(storedCJK, token) {
			t.Fatalf("search_cjk = %q，缺词项 %s", storedCJK, token)
		}
	}
	for _, text := range []string{"磁盘", "盘上", "写到磁盘"} {
		hits, err := db.SearchMessages(ctx, sqlitestore.SearchQuery{Text: text, Mode: sqlitestore.SearchFTS})
		if err != nil {
			t.Fatalf("fts search %q: %v", text, err)
		}
		if len(hits) != 1 {
			t.Fatalf("fts search %q = %d 条, want 1", text, len(hits))
		}
		// 单字（unigram）也不能丢：它最容易被 trigram 的 3 字符窗口吞掉。
		single, err := db.SearchMessages(ctx, sqlitestore.SearchQuery{Text: "盘", Mode: sqlitestore.SearchFTS})
		if err != nil || len(single) != 1 {
			t.Fatalf("fts search \"盘\" = %d 条 (err %v), want 1", len(single), err)
		}
	}
}

// TestSearchOffsetPagesWithoutDuplicates：跨会话多次翻页不重不漏，两种模式都测。
//
// Offset 与 ORDER BY 必须**同源**：只要排序表达式和偏移量出自同一段 SQL，翻页就
// 不会漏页；历史上"拼接字符串排序 + 独立 ORDER BY"就是这么漏掉一个会话的。
func TestSearchOffsetPagesWithoutDuplicates(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	sessions := make([]string, 0, 3)
	for index := 0; index < 3; index++ {
		meta, err := db.Create(ctx, sqlitestore.Meta{Title: "分页" + itoa(index)})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		sessions = append(sessions, meta.ID)
		for ordinal := 0; ordinal < 4; ordinal++ {
			if _, err := db.Append(ctx, meta.ID, userMessage("paging marker 出现在这里")); err != nil {
				t.Fatalf("Append: %v", err)
			}
		}
	}
	modes := []struct {
		name string
		mode sqlitestore.SearchMode
	}{
		{name: "substring", mode: sqlitestore.SearchSubstring},
		{name: "fts5", mode: sqlitestore.SearchFTS},
	}
	for _, tc := range modes {
		if tc.mode == sqlitestore.SearchFTS && !db.FTS5Available() {
			t.Logf("跳过 fts5 翻页：本次构建没有 FTS5（已降级为 substring）")
			continue
		}
		seen := make(map[string]bool, 12)
		offset := 0
		for page := 0; page < 10; page++ {
			hits, err := db.SearchMessages(ctx, sqlitestore.SearchQuery{
				Text: "paging marker", Mode: tc.mode, Limit: 2, Offset: offset,
			})
			if err != nil {
				t.Fatalf("%s: SearchMessages(offset=%d): %v", tc.name, offset, err)
			}
			if len(hits) == 0 {
				break
			}
			for _, hit := range hits {
				key := hit.SessionID + "#" + itoa(hit.Ordinal)
				if seen[key] {
					t.Fatalf("%s: 第 %d 页出现重复命中 %s", tc.name, page, key)
				}
				seen[key] = true
			}
			offset += len(hits)
		}
		if len(seen) != len(sessions)*4 {
			t.Fatalf("%s: 翻页覆盖 %d 条, want %d（漏页）", tc.name, len(seen), len(sessions)*4)
		}
	}
}

// TestUpgradeBuildsSearchIndex：老库（v12）升级到 13 之后，fts 表被建好、数据回填
// 正确——既包括 ASCII 的 search_text，也包括 CJK 的 bigram 辅助列。
func TestUpgradeBuildsSearchIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v12.sqlite")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	oldPayload, err := json.Marshal(userMessage("release checklist"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	cjkPayload, err := json.Marshal(userMessage("我们把文件写到磁盘上"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	statements := []string{
		// v12 的会话行形状（段 12 已加 next_ordinal）。
		`CREATE TABLE rig_sessions (
			id TEXT PRIMARY KEY, title TEXT NOT NULL DEFAULT '',
			workspace_id TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL, message_count INTEGER NOT NULL DEFAULT 0,
			bytes INTEGER NOT NULL DEFAULT 0, revision INTEGER NOT NULL DEFAULT 0,
			format_version INTEGER NOT NULL DEFAULT 1,
			title_cleared INTEGER NOT NULL DEFAULT 0,
			next_ordinal INTEGER NOT NULL DEFAULT 0)`,
		// v12 的消息表：只有 search_text（v13 才加 search_cjk）。
		`CREATE TABLE rig_messages (
			session_id TEXT NOT NULL REFERENCES rig_sessions(id) ON DELETE CASCADE,
			ordinal INTEGER NOT NULL,
			payload BLOB NOT NULL,
			search_text TEXT NOT NULL DEFAULT '')`,
		`CREATE UNIQUE INDEX rig_messages_session_ordinal ON rig_messages(session_id, ordinal)`,
		`INSERT INTO rig_sessions(id, title, created_at, updated_at, message_count, next_ordinal)
		 VALUES ('s1', '老会话', 1700000000000, 1700000000000, 2, 2)`,
		`PRAGMA user_version = 12`,
	}
	for _, statement := range statements {
		if _, err := raw.Exec(statement); err != nil {
			_ = raw.Close()
			t.Fatalf("建 v12 库失败（%s）: %v", statement, err)
		}
	}
	// 老行是**真正的 v12 形状**：`search_text` 已经填好（v6 起就有这一列），只差
	// v13 才加的 `search_cjk`。旧用例把 search_text 也写成空串，走的其实是 v6 之前
	// 那条"一次补两列"的路径——v12→v13 这条只补 CJK 的路径此前没有覆盖。
	for _, row := range []struct {
		ordinal    int
		payload    []byte
		searchText string
	}{
		{ordinal: 0, payload: oldPayload, searchText: "release checklist"},
		{ordinal: 1, payload: cjkPayload, searchText: "我们把文件写到磁盘上"},
	} {
		if _, err := raw.Exec(
			`INSERT INTO rig_messages(session_id, ordinal, payload, search_text) VALUES('s1', ?, ?, ?)`,
			row.ordinal, row.payload, row.searchText); err != nil {
			_ = raw.Close()
			t.Fatalf("插入老消息: %v", err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}
	store, err := sqlitestore.Open(sqlitestore.Options{Path: path})
	if err != nil {
		t.Fatalf("升级 Open: %v", err)
	}
	defer func() { _ = store.Close() }()
	schema := schemaOf(t, store)
	if schema.Version != 13 {
		t.Fatalf("升级后版本 = %d, want 13", schema.Version)
	}
	if !store.FTS5Available() {
		t.Skip("本次构建的 SQLite 没有 FTS5：无法断言 fts 表")
	}
	if !has(schema.Tables, "rig_messages_fts") {
		t.Fatalf("升级没有建出 rig_messages_fts；tables = %v", schema.Tables)
	}
	ctx := context.Background()
	// search_cjk 只补上了 CJK 那一行；ASCII 行算出来就是空串，不该被当成"没补"。
	for _, row := range []struct {
		ordinal int
		want    string
	}{{ordinal: 0, want: ""}, {ordinal: 1, want: "#磁盘#"}} {
		var cjk string
		if err := store.DB().QueryRowContext(ctx,
			`SELECT search_cjk FROM rig_messages WHERE session_id = 's1' AND ordinal = ?`,
			row.ordinal).Scan(&cjk); err != nil {
			t.Fatalf("读 search_cjk: %v", err)
		}
		if !strings.Contains(cjk, row.want) {
			t.Fatalf("ordinal %d 的 search_cjk = %q，want 含 %q", row.ordinal, cjk, row.want)
		}
	}
	for text, want := range map[string]int{"release": 1, "磁盘": 1} {
		hits, err := store.SearchMessages(ctx, sqlitestore.SearchQuery{Text: text, Mode: sqlitestore.SearchFTS})
		if err != nil {
			t.Fatalf("升级后 fts 检索 %q: %v", text, err)
		}
		if len(hits) != want {
			t.Fatalf("升级后 fts 检索 %q = %d 条, want %d（回填不完整）", text, len(hits), want)
		}
	}
	// 触发器必须让**升级之后**的写入也进索引。
	if _, err := store.Append(ctx, "s1", userMessage("fresh deploy note")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	hits, err := store.SearchMessages(ctx, sqlitestore.SearchQuery{Text: "deploy", Mode: sqlitestore.SearchFTS})
	if err != nil || len(hits) != 1 {
		t.Fatalf("升级后新写入的检索 = %d 条 (err %v), want 1", len(hits), err)
	}
	if !strings.Contains(hits[0].Snippet, "fresh deploy note") {
		t.Fatalf("新命中的摘要 = %q，want 含正文", hits[0].Snippet)
	}
}

// ftsIntegrityCheck 让 FTS5 自检索引与内容表是否一致。
//
// `rank = 1` 表示**信任内容表**：它会把每条内容行都拿去和倒排索引核对，所以"索引
// 空着、内容表有行"这种半坏状态会报 `database disk image is malformed`，而不是
// 悄悄通过（不带 rank 的默认形状不查这一项）。
func ftsIntegrityCheck(t *testing.T, db *sqlitestore.Store) {
	t.Helper()
	if _, err := db.DB().Exec(
		`INSERT INTO rig_messages_fts(rig_messages_fts, rank) VALUES('integrity-check', 1)`); err != nil {
		t.Fatalf("fts integrity-check 不干净: %v", err)
	}
}

// TestBackfillSearchIndexRecoversFromEmptyIndex：索引被清空、CJK 词表待补的库，
// 再调一次 BackfillSearchIndex 必须**整体**修好。
//
// 顺序是这条用例的要点：`UPDATE rig_messages` 会触发 `au`，它的第一句是 `'delete'`
// 命令。索引里没有这一行时删它，FTS5 直接判索引损坏（`SQLITE_CORRUPT_VTAB`），
// 于是回填中途失败、末尾的 `rebuild` 永远不执行——检索从此对该库永久 0 条，
// Replace/Delete 还会报 malformed。所以必须先 rebuild 再补列。
func TestBackfillSearchIndexRecoversFromEmptyIndex(t *testing.T) {
	db, path := openTemp(t, sqlitestore.Options{})
	if !db.FTS5Available() {
		t.Skip("本次构建的 SQLite 没有 FTS5：没有可清空的索引")
	}
	ctx := context.Background()
	created, err := db.Create(ctx, sqlitestore.Meta{Title: "索引恢复"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := db.AppendBatch(ctx, created.ID,
		userMessage("release checklist 我们把文件写到磁盘上"),
		userMessage("the release build is green")); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	// 造出"内容在、倒排索引空、search_cjk 待补"的状态。触发器必须先摘掉：装着
	// 触发器去改 search_cjk 会立刻触发 `au` 的 delete，那正是要修的那次崩溃，
	// 会把用例变成"造状态失败"而不是"验证回填能恢复"。
	for _, statement := range []string{
		`DROP TRIGGER IF EXISTS rig_messages_fts_ai`,
		`DROP TRIGGER IF EXISTS rig_messages_fts_ad`,
		`DROP TRIGGER IF EXISTS rig_messages_fts_au`,
		`INSERT INTO rig_messages_fts(rig_messages_fts) VALUES('delete-all')`,
		`UPDATE rig_messages SET search_cjk = ''`,
	} {
		if _, err := db.DB().Exec(statement); err != nil {
			t.Fatalf("构造破损索引失败（%s）: %v", statement, err)
		}
	}
	// 触发器装回去（迁移段里的 `CREATE TRIGGER IF NOT EXISTS` 就是这件事），
	// 这样回填走的是"触发器在场"的真实路径。
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := sqlitestore.Open(sqlitestore.Options{Path: path})
	if err != nil {
		t.Fatalf("重新 Open: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	var triggers int
	if err := reopened.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'trigger'`).Scan(&triggers); err != nil {
		t.Fatalf("count triggers: %v", err)
	}
	if triggers != 3 {
		t.Fatalf("重新 Open 后触发器 = %d, want 3", triggers)
	}
	if updated, err := reopened.BackfillSearchIndex(ctx); err != nil {
		t.Fatalf("BackfillSearchIndex 在索引空、词表待补的库上必须成功: %v", err)
	} else if updated == 0 {
		t.Fatalf("BackfillSearchIndex 补了 0 行：CJK 列没有被补上")
	}
	for _, text := range []string{"磁盘", "release"} {
		hits, err := reopened.SearchMessages(ctx, sqlitestore.SearchQuery{Text: text, Mode: sqlitestore.SearchFTS})
		if err != nil {
			t.Fatalf("回填后 fts 检索 %q: %v", text, err)
		}
		if len(hits) == 0 {
			t.Fatalf("回填后 fts 检索 %q = 0 条：索引没恢复", text)
		}
	}
	ftsIntegrityCheck(t, reopened)
	if !reopened.SearchIndexReady() {
		t.Fatal("BackfillSearchIndex 成功后 SearchIndexReady() = false：fts 查询会静默降级")
	}
	// 回填之后写入路径也不能再踩 malformed：Replace/Delete 都走触发器。
	if _, err := reopened.Replace(ctx, created.ID, []rig.Message{userMessage("recovered 磁盘")}); err != nil {
		t.Fatalf("回填后 Replace: %v", err)
	}
	if err := reopened.Delete(ctx, created.ID); err != nil {
		t.Fatalf("回填后 Delete: %v", err)
	}
}

// TestFTSRequiresAllTerms：查询按空白切成词项，词项之间是 AND——"error 磁盘"不能
// 命中只说"磁盘"、不含 "error" 的消息。
//
// 旧实现把 CJK 词项与整串短语用 OR 连起来，于是查询里的 ASCII 部分被完全丢弃，
// 命中了不相关的消息。
func TestFTSRequiresAllTerms(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	if !db.FTS5Available() {
		t.Skip("本次构建的 SQLite 没有 FTS5")
	}
	ctx := context.Background()
	created, err := db.Create(ctx, sqlitestore.Meta{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := db.AppendBatch(ctx, created.ID,
		userMessage("error: 磁盘写满了"),                       // 0：两个词项都有
		userMessage("磁盘空间不足"),                             // 1：没有 error
		userMessage("error handling notes")); err != nil { // 2：没有磁盘
		t.Fatalf("AppendBatch: %v", err)
	}
	substring, err := db.SearchMessages(ctx, sqlitestore.SearchQuery{Text: "error 磁盘"})
	if err != nil {
		t.Fatalf("substring search: %v", err)
	}
	fts, err := db.SearchMessages(ctx, sqlitestore.SearchQuery{Text: "error 磁盘", Mode: sqlitestore.SearchFTS})
	if err != nil {
		t.Fatalf("fts search: %v", err)
	}
	if len(fts) != 1 || fts[0].Ordinal != 0 {
		t.Fatalf("fts \"error 磁盘\" = %v, want 只命中 [#0]", hitKeys(fts))
	}
	// 每个命中都必须**同时**含两个词项；只说"磁盘"的那条不能回来。
	for _, hit := range fts {
		if !strings.Contains(hit.Snippet, "error") {
			t.Fatalf("命中了不含 error 的消息（ordinal %d）：%q", hit.Ordinal, hit.Snippet)
		}
	}
	// 放宽的方向只有一个：fts 的命中集合是 substring 的超集（多词项不要求相邻），
	// 但**绝不能漏掉** substring 能命中的消息。
	for _, hit := range substring {
		key := hit.SessionID + "#" + itoa(hit.Ordinal)
		found := false
		for _, ftsHit := range fts {
			if ftsHit.SessionID+"#"+itoa(ftsHit.Ordinal) == key {
				found = true
			}
		}
		if !found {
			t.Fatalf("substring 命中 %s 但 fts 没命中：fts=%v", key, hitKeys(fts))
		}
	}
}

// TestFTSAsciiMatchesSubstringExactly：纯 ASCII 查询（含标点）在两种模式下命中
// 集合必须**完全相同**。
//
// 这条用例钉住转义：`"`、`-`、`#`、`.`、`/` 都不是 FTS5 语法，进 MATCH 前只做
// 字面量转义（`"` → `""`、整串加引号），不剥字符、也不让 CJK 词表替它命中。
func TestFTSAsciiMatchesSubstringExactly(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	if !db.FTS5Available() {
		t.Skip("本次构建的 SQLite 没有 FTS5")
	}
	ctx := context.Background()
	created, err := db.Create(ctx, sqlitestore.Meta{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := db.AppendBatch(ctx, created.ID,
		userMessage("我们把文件写到磁盘上"),
		userMessage("磁盘空间不足，见 #磁盘# 标记"),
		userMessage(`git commit -m "fix" 之后 deploy`),
		userMessage("a.b/c 路径"),
		userMessage("release checklist 已就绪")); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	queries := []string{
		"#磁盘#", // 哨兵是写入侧的东西，查询侧按字面量对待
		"磁盘\"", // 引号只该匹配真的带引号的那段
		"-磁盘",  // 前缀减号不是 FTS5 的"排除"语法
		"磁盘-",
		`git commit -m "fix"`, // 含 `"`、空格、`-` 的整串
		"a.b/c",               // `.` 与 `/` 不是语法
		"写到磁盘",
		"磁盘上我们", // 只出现在一个更长的词里：必须 0 条
	}
	for _, text := range queries {
		substring, err := db.SearchMessages(ctx, sqlitestore.SearchQuery{Text: text})
		if err != nil {
			t.Fatalf("substring %q: %v", text, err)
		}
		fts, err := db.SearchMessages(ctx, sqlitestore.SearchQuery{Text: text, Mode: sqlitestore.SearchFTS})
		if err != nil {
			t.Fatalf("fts %q: %v", text, err)
		}
		if !sameKeys(hitKeys(substring), hitKeys(fts)) {
			t.Fatalf("%q 的命中集合不同：substring=%v fts=%v", text, hitKeys(substring), hitKeys(fts))
		}
	}
}
