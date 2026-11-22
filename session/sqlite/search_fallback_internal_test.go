package sqlite

// 降级路径：SQLite 编译时没有 FTS5（或 trigram 分词器）时，库必须照常能打开、
// 写入，检索退化为 substring——**不是** panic、也不是"打不开"。
//
// 真实构建（modernc.org/sqlite）带 FTS5，制造不了"编译时没开"的局面，所以这里
// 替换探测函数 fts5Probe（Open 用它决定是否执行建索引段）。
//
// 探测函数的形状是 (可用, 错误)：只有"本构建真的没有 FTS5"才返回 false, nil；
// 其它错误由 Open 报出来，不会被静默当成降级。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	rig "github.com/zeokeel/zeokeel-rig"
)

// TestSearchFallsBackWhenFTS5Unavailable：请求 fts5 模式但没有 FTS5 时，
// 命中集合与 substring 模式一致，且没有 fts 表、没有触发器。
func TestSearchFallsBackWhenFTS5Unavailable(t *testing.T) {
	original := fts5Probe
	fts5Probe = func(context.Context, *sql.DB) (bool, error) { return false, nil }
	t.Cleanup(func() { fts5Probe = original })

	path := filepath.Join(t.TempDir(), "nofts.sqlite")
	store, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("没有 FTS5 时 Open 必须成功（降级，不是失败）: %v", err)
	}
	defer func() { _ = store.Close() }()
	if store.FTS5Available() {
		t.Fatal("FTS5Available() = true，want false（探测函数已被换成不可用）")
	}
	ctx := context.Background()
	// 迁移必须把建索引段整个跳过：虚拟表与触发器都不该出现。
	schema, err := store.Schema(ctx)
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}
	for _, table := range schema.Tables {
		if table == "rig_messages_fts" {
			t.Fatalf("没有 FTS5 时仍然建了虚拟表；tables = %v", schema.Tables)
		}
	}
	var triggers int
	if err := store.db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'trigger'`).Scan(&triggers); err != nil {
		t.Fatalf("count triggers: %v", err)
	}
	if triggers != 0 {
		t.Fatalf("没有 FTS5 时仍建了 %d 个触发器", triggers)
	}
	// 写入与检索照常：这条消息只该是"没有 bm25 排序"，不是"搜不到"。
	meta, err := store.Create(ctx, Meta{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := store.Append(ctx, meta.ID, rig.Message{
		Role:    rig.RoleUser,
		Content: []rig.ContentPart{rig.TextPart("我们把文件写到磁盘上")},
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	substring, err := store.SearchMessages(ctx, SearchQuery{Text: "磁盘"})
	if err != nil {
		t.Fatalf("substring search: %v", err)
	}
	fts, err := store.SearchMessages(ctx, SearchQuery{Text: "磁盘", Mode: SearchFTS})
	if err != nil {
		t.Fatalf("fts search（应降级而不是报错）: %v", err)
	}
	if len(substring) != 1 || len(fts) != 1 {
		t.Fatalf("降级后命中数 substring=%d fts=%d, want 1/1", len(substring), len(fts))
	}
	if substring[0].SessionID != fts[0].SessionID || substring[0].Ordinal != fts[0].Ordinal {
		t.Fatalf("降级前后命中了不同的消息：%+v vs %+v", substring[0], fts[0])
	}
	// 降级构建上的回填只补 CJK 列，不碰不存在的虚拟表。
	if _, err := store.BackfillSearchIndex(ctx); err != nil {
		t.Fatalf("没有 FTS5 时 BackfillSearchIndex 不该报错: %v", err)
	}
	// 降级构建必须**保留** search_cjk 列：跳过的是虚拟表与触发器，不是列本身。
	// 列一旦缺席，换成带 FTS5 的构建再打开时就没有"已备好的词表"可用，整条交叉
	// 构建路径都会失效。
	var columns []string
	rows, err := store.db.QueryContext(ctx, `SELECT name FROM pragma_table_info('rig_messages')`)
	if err != nil {
		t.Fatalf("pragma_table_info: %v", err)
	}
	for rows.Next() {
		var name string
		if scanErr := rows.Scan(&name); scanErr != nil {
			_ = rows.Close()
			t.Fatalf("scan column: %v", scanErr)
		}
		columns = append(columns, name)
	}
	_ = rows.Close()
	if !containsString(columns, "search_cjk") {
		t.Fatalf("降级构建上 search_cjk 列丢了：columns = %v", columns)
	}
	if store.SearchIndexReady() {
		t.Fatal("没有 FTS5 时 SearchIndexReady() 必须是 false（宿主据此判断检索已降级）")
	}
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// assertFTSIntegrity 让 FTS5 自检倒排索引与内容表是否逐行对齐。
//
// `rank = 1` 表示信任内容表：索引缺行会报 `database disk image is malformed`。
func assertFTSIntegrity(t *testing.T, store *Store) {
	t.Helper()
	if _, err := store.db.ExecContext(context.Background(),
		`INSERT INTO rig_messages_fts(rig_messages_fts, rank) VALUES('integrity-check', 1)`); err != nil {
		t.Fatalf("fts integrity-check 不干净: %v", err)
	}
}

// TestCrossBuildUpgradeKeepsSearchWorking：库先被**没有 FTS5 的构建**升级到 v13
// （只跑 ALTER，老行的 search_cjk 还是空串，没有虚拟表也没有触发器），再用**带
// FTS5 的构建**打开。
//
// 这正是文档承诺支持的交叉构建路径，也是旧代码丢检索的地方：Open 走的是裸
// BackfillSearchIndex（先 UPDATE 后 rebuild），`au` 的 delete 撞上空索引 → 整体
// 失败 → 虚拟表建好但内容空，检索永久 0 条，而 Open 只打一条 slog。宿主要么拿到
// 错误，要么拿到一个明确"索引未就绪"的状态，不能是"看起来能搜、其实永远空白"。
func TestCrossBuildUpgradeKeepsSearchWorking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crossbuild.sqlite")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	payload, err := json.Marshal(rig.Message{
		Role:    rig.RoleUser,
		Content: []rig.ContentPart{rig.TextPart("我们把文件写到磁盘上")},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	release, err := json.Marshal(rig.Message{
		Role:    rig.RoleUser,
		Content: []rig.ContentPart{rig.TextPart("release checklist")},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// v12 的形状：消息表只有 search_text（v13 才加 search_cjk），没有检索索引。
	for _, statement := range []string{
		`CREATE TABLE rig_sessions (
			id TEXT PRIMARY KEY, title TEXT NOT NULL DEFAULT '',
			workspace_id TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL, message_count INTEGER NOT NULL DEFAULT 0,
			bytes INTEGER NOT NULL DEFAULT 0, revision INTEGER NOT NULL DEFAULT 0,
			format_version INTEGER NOT NULL DEFAULT 1,
			title_cleared INTEGER NOT NULL DEFAULT 0,
			next_ordinal INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE rig_messages (
			session_id TEXT NOT NULL REFERENCES rig_sessions(id) ON DELETE CASCADE,
			ordinal INTEGER NOT NULL,
			payload BLOB NOT NULL,
			search_text TEXT NOT NULL DEFAULT '')`,
		`CREATE UNIQUE INDEX rig_messages_session_ordinal ON rig_messages(session_id, ordinal)`,
		`INSERT INTO rig_sessions(id, title, created_at, updated_at, message_count, next_ordinal)
		 VALUES ('s1', '老会话', 1700000000000, 1700000000000, 2, 2)`,
		`PRAGMA user_version = 12`,
	} {
		if _, err := raw.Exec(statement); err != nil {
			_ = raw.Close()
			t.Fatalf("建 v12 库失败（%s）: %v", statement, err)
		}
	}
	for _, row := range []struct {
		ordinal    int
		payload    []byte
		searchText string
	}{
		{ordinal: 0, payload: payload, searchText: "我们把文件写到磁盘上"},
		{ordinal: 1, payload: release, searchText: "release checklist"},
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

	// 第一段：没有 FTS5 的构建升级到 v13——只加列，不建索引。
	original := fts5Probe
	fts5Probe = func(context.Context, *sql.DB) (bool, error) { return false, nil }
	oldBuild, err := Open(Options{Path: path})
	fts5Probe = original
	if err != nil {
		t.Fatalf("无 FTS5 构建 Open: %v", err)
	}
	if oldBuild.FTS5Available() || oldBuild.SearchIndexReady() {
		t.Fatalf("无 FTS5 构建下 FTS5Available=%v SearchIndexReady=%v，want false/false",
			oldBuild.FTS5Available(), oldBuild.SearchIndexReady())
	}
	ctx := context.Background()
	var cjk string
	if err := oldBuild.db.QueryRowContext(ctx,
		`SELECT search_cjk FROM rig_messages WHERE session_id = 's1' AND ordinal = 0`).Scan(&cjk); err != nil {
		_ = oldBuild.Close()
		t.Fatalf("v13 段没有加 search_cjk 列: %v", err)
	}
	if cjk != "" {
		_ = oldBuild.Close()
		t.Fatalf("老行 search_cjk = %q，want 空串（无 FTS5 的构建不该算词表）", cjk)
	}
	if err := oldBuild.Close(); err != nil {
		t.Fatalf("Close 无 FTS5 构建: %v", err)
	}

	// 第二段：换成带 FTS5 的构建打开同一个库。
	store, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("带 FTS5 构建 Open: %v", err)
	}
	defer func() { _ = store.Close() }()
	if !store.FTS5Available() {
		t.Fatal("带 FTS5 的构建 FTS5Available() = false")
	}
	if !store.SearchIndexReady() {
		t.Fatal("交叉构建打开后 SearchIndexReady() = false：索引没建好，宿主会以为检索可用")
	}
	for text, want := range map[string]int{"磁盘": 1, "release": 1, "写到磁盘": 1} {
		hits, err := store.SearchMessages(ctx, SearchQuery{Text: text, Mode: SearchFTS})
		if err != nil {
			t.Fatalf("交叉构建后 fts 检索 %q: %v", text, err)
		}
		if len(hits) != want {
			t.Fatalf("交叉构建后 fts 检索 %q = %d 条, want %d（索引没建起来）", text, len(hits), want)
		}
	}
	var triggers int
	if err := store.db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'trigger'`).Scan(&triggers); err != nil {
		t.Fatalf("count triggers: %v", err)
	}
	if triggers != 3 {
		t.Fatalf("交叉构建后触发器 = %d, want 3", triggers)
	}
	if err := store.db.QueryRowContext(ctx,
		`SELECT search_cjk FROM rig_messages WHERE session_id = 's1' AND ordinal = 0`).Scan(&cjk); err != nil {
		t.Fatalf("read search_cjk: %v", err)
	}
	if !strings.Contains(cjk, "#磁盘#") {
		t.Fatalf("交叉构建后 search_cjk = %q，want 含 #磁盘#（词表没有回填）", cjk)
	}
	assertFTSIntegrity(t, store)
}

// TestProbeErrorFailsOpenInsteadOfDegrading：探测返回的**非"没有 FTS5"错误**必须
// 让 Open 失败，而不是被当成"本构建没有 FTS5"。
//
// 为什么这是 P1 级：把瞬时失败当成降级，迁移会跳过建索引段却照样把 user_version
// 推进到 13；同一个构建之后每次打开都会落进"索引表不存在"的分支，索引再也不建。
// 用户看到的是"库能打开、检索永远 0 条"。
func TestProbeErrorFailsOpenInsteadOfDegrading(t *testing.T) {
	original := fts5Probe
	fts5Probe = func(context.Context, *sql.DB) (bool, error) {
		return false, errors.New("sqlite: probe fts5: disk I/O error")
	}
	t.Cleanup(func() { fts5Probe = original })

	path := filepath.Join(t.TempDir(), "probe-error.sqlite")
	store, err := Open(Options{Path: path})
	if err == nil {
		_ = store.Close()
		t.Fatal("探测报错时 Open 必须失败，不能静默降级成 substring")
	}
	if !strings.Contains(err.Error(), "probe fts5") {
		t.Fatalf("Open 的错误 = %v，want 保留探测错误", err)
	}
	// 迁移根本不该跑：库的 user_version 必须还是 0（没有被推进到 13 却缺索引）。
	raw, openErr := sql.Open("sqlite", "file:"+path)
	if openErr != nil {
		t.Fatalf("open raw: %v", openErr)
	}
	defer func() { _ = raw.Close() }()
	var version int
	if err := raw.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != 0 {
		t.Fatalf("探测失败却把 user_version 推进到 %d：索引会永远缺失", version)
	}
}

// TestFTS5UnsupportedClassification：只有"模块/分词器不存在"算"本构建没有 FTS5"，
// 其它错误（锁、I/O、临时表建不出来）都不能被当成降级理由。
func TestFTS5UnsupportedClassification(t *testing.T) {
	unsupported := []string{
		"no such module: fts5",
		"unknown tokenizer: trigram",
		"no such tokenizer: trigram",
	}
	for _, message := range unsupported {
		if !fts5Unsupported(errors.New(message)) {
			t.Fatalf("%q 应判为“本构建没有 FTS5”", message)
		}
	}
	for _, message := range []string{
		"database is locked",
		"disk I/O error",
		"database disk image is malformed",
	} {
		if fts5Unsupported(errors.New(message)) {
			t.Fatalf("%q 不该判为“本构建没有 FTS5”（它该让 Open 报错）", message)
		}
	}
	if fts5Unsupported(nil) {
		t.Fatal("nil 不是“没有 FTS5”")
	}
}
