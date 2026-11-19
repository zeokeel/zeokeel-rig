package sqlite_test

// 分段迁移（coreVersion 3）的回归：老库里的表**已经存在**，所以
// `CREATE TABLE IF NOT EXISTS` 对它一条语句都不生效——给已有表加列必须走
// `ALTER TABLE ... ADD COLUMN`，否则升级"成功"、运行期 `no such column`。

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	sqlitestore "github.com/zeokeel/zeokeel-rig/session/sqlite"
)

// v1Statements 是 coreVersion 1 时代的 DDL 逐字快照。
//
// 它故意**不含** rig_memory_pages / rig_spill（v2 才加），rig_sessions 也**没有**
// format_version（v3 才加）——这正是"已有表需要 ALTER"的那个形状。v1 时代的库
// 也没有任何索引（早期 Open 只建表），所以升级必须能把索引一并补上。
var v1Statements = []string{
	`CREATE TABLE rig_sessions (
		id            TEXT PRIMARY KEY,
		title         TEXT    NOT NULL DEFAULT '',
		workspace_id  TEXT    NOT NULL DEFAULT '',
		created_at    INTEGER NOT NULL,
		updated_at    INTEGER NOT NULL,
		message_count INTEGER NOT NULL DEFAULT 0,
		bytes         INTEGER NOT NULL DEFAULT 0,
		revision      INTEGER NOT NULL DEFAULT 0)`,
	`CREATE TABLE rig_messages (
		session_id TEXT    NOT NULL REFERENCES rig_sessions(id) ON DELETE CASCADE,
		ordinal    INTEGER NOT NULL,
		payload    BLOB    NOT NULL)`,
	`CREATE TABLE rig_events (
		session_id TEXT    NOT NULL,
		seq        INTEGER NOT NULL,
		kind       TEXT    NOT NULL,
		at         INTEGER NOT NULL,
		payload    BLOB    NOT NULL)`,
	`CREATE TABLE rig_costs (
		session_id        TEXT    NOT NULL,
		model             TEXT    NOT NULL DEFAULT '',
		prompt_tokens     INTEGER NOT NULL DEFAULT 0,
		completion_tokens INTEGER NOT NULL DEFAULT 0,
		cached_tokens     INTEGER NOT NULL DEFAULT 0,
		cost_usd          REAL    NOT NULL DEFAULT 0,
		at                INTEGER NOT NULL)`,
	`CREATE TABLE rig_checkpoints (
		session_id TEXT PRIMARY KEY REFERENCES rig_sessions(id) ON DELETE CASCADE,
		payload    BLOB    NOT NULL,
		updated_at INTEGER NOT NULL)`,
	`CREATE TABLE rig_schema_extensions (name TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)`,
}

// buildV1Database 造一个"v1 时代的库"，并放进去一条会话与一条消息，用来证明
// 升级既不丢数据、也不只是把版本号写上去。
func buildV1Database(t *testing.T, path string) {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer func() { _ = raw.Close() }()
	for _, statement := range append(v1Statements, `PRAGMA user_version = 1`) {
		if _, err := raw.Exec(statement); err != nil {
			t.Fatalf("v1 建库失败（%s）: %v", statement, err)
		}
	}
	if _, err := raw.Exec(
		`INSERT INTO rig_sessions(id, title, workspace_id, created_at, updated_at, message_count)
		 VALUES('v1-session', '旧标题', 'ws-1', 1700000000000, 1700000000001, 1)`); err != nil {
		t.Fatalf("insert v1 session: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO rig_messages(session_id, ordinal, payload)
		 VALUES('v1-session', 0, ?)`,
		`{"role":"user","content":[{"type":"text","text":"升级前写下的中文"}]}`); err != nil {
		t.Fatalf("insert v1 message: %v", err)
	}
}

// TestMigrateFromV1AddsNewColumn 是本次 coreVersion 3 的主回归。
//
// 红的方式：把 coreVersion 改回 2（或删掉 v3 段落），v3 的 ALTER 就不再执行，
// 于是 `format_version` 列不存在——测试第一条断言就红。
func TestMigrateFromV1AddsNewColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.sqlite")
	buildV1Database(t, path)

	// 升级前的库确实没有这一列（否则这条用例什么也没证明）。
	if columns := rawSessionColumns(t, path); has(columns, "format_version") {
		t.Fatalf("v1 库不该有 format_version，columns = %v", columns)
	}

	db, err := sqlitestore.Open(sqlitestore.Options{Path: path})
	if err != nil {
		t.Fatalf("Open(v1 → 最新): %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	schema := schemaOf(t, db)
	if !has(schema.SessionsColumns, "format_version") {
		t.Fatalf("升级后 rig_sessions 仍缺 format_version；columns = %v", schema.SessionsColumns)
	}
	// 版本号必须推进到本构建的 coreVersion：拿一次全新 Open 的版本作对照，
	// 这样用例不写死 3，coreVersion 再 bump 时不需要改测试。
	fresh, _ := openTemp(t, sqlitestore.Options{})
	want := schemaOf(t, fresh).Version
	if schema.Version != want {
		t.Fatalf("升级后 user_version = %d, want %d", schema.Version, want)
	}
	if want <= 1 {
		t.Fatalf("coreVersion = %d：分段迁移至少要经过 v2/v3 两段", want)
	}

	// 老数据必须原样可读；新列要有可用的默认值（老行不会因为 NOT NULL 而失败）。
	meta, messages, err := db.Load(ctx, "v1-session")
	if err != nil {
		t.Fatalf("升级后 Load: %v", err)
	}
	if meta.Title != "旧标题" || meta.Workspace != "ws-1" || len(messages) != 1 {
		t.Fatalf("升级后 meta = %+v, messages = %d", meta, len(messages))
	}
	if got := rawFormatVersion(t, path, "v1-session"); got != 1 {
		t.Fatalf("老行的 format_version = %d, want 1（默认值）", got)
	}
	// v1 时代缺失的索引必须被基线段补回来。
	for _, want := range []string{"rig_sessions_updated_at", "rig_messages_session_ordinal"} {
		if !has(schema.Indexes, want) {
			t.Fatalf("升级后仍缺索引 %q；indexes = %v", want, schema.Indexes)
		}
	}
	// 再开一次必须幂等（ALTER 不允许重复执行，所以每段都必须自己探测）。
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	again, err := sqlitestore.Open(sqlitestore.Options{Path: path})
	if err != nil {
		t.Fatalf("二次 Open（幂等）: %v", err)
	}
	t.Cleanup(func() { _ = again.Close() })
	if got := schemaOf(t, again).Version; got != want {
		t.Fatalf("二次 Open 后 user_version = %d, want %d", got, want)
	}
}

// TestNewRowsGetTheCurrentFormatVersion 钉住 format_version 的写入侧：新会话必须
// 拿到本构建的格式号，否则"按格式号挑读取路径"这件事从第一天就是坏的。
func TestNewRowsGetTheCurrentFormatVersion(t *testing.T) {
	db, path := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	meta, err := db.Create(ctx, sqlitestore.Meta{Title: "新会话"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := rawFormatVersion(t, path, meta.ID); got != 1 {
		t.Fatalf("新行 format_version = %d, want 1", got)
	}
}

// rawSessionColumns 直接读 pragma，绕开 Store（预检/断言都要能在"还没升级"时用）。
func rawSessionColumns(t *testing.T, path string) []string {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer func() { _ = raw.Close() }()
	rows, err := raw.Query(`SELECT name FROM pragma_table_info('rig_sessions')`)
	if err != nil {
		t.Fatalf("pragma_table_info: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var columns []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate columns: %v", err)
	}
	return columns
}

// rawFormatVersion 读一行的 format_version。
func rawFormatVersion(t *testing.T, path, sessionID string) int {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer func() { _ = raw.Close() }()
	var version int
	if err := raw.QueryRow(
		fmt.Sprintf(`SELECT format_version FROM rig_sessions WHERE id = ?`), sessionID).Scan(&version); err != nil {
		t.Fatalf("read format_version: %v", err)
	}
	return version
}
