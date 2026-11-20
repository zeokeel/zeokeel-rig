package sqlite_test

// 溢出结果按会话作用域回收。
//
// 同一个库里放着所有会话：`rig_spill` 如果只按 `seq` 全局裁剪，会话 A 的一次
// `GC(MaxFiles)` 就会删掉会话 B 的句柄——模型手里的 `ref:` 从此读不回来，等于
// 把 B 的工具输出永久丢掉。文件后端（agent/spill.go 的 pruneSpillFiles）早就按
// 文件名里的作用域前缀分区了；这个文件钉住数据库后端做出同样的行为。
//
// 迁移那半：`scope` 列是 coreVersion 5 才有的。老库的表已经存在，`CREATE TABLE
// IF NOT EXISTS` 对它一条语句都不生效，所以必须走 `ALTER TABLE ... ADD COLUMN`
// （见 schema.go 的注释），并且老行要落进"空作用域"这一桶。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	rig "github.com/zeokeel/zeokeel-rig"
	sqlitestore "github.com/zeokeel/zeokeel-rig/session/sqlite"
)

// TestSpillGCIsScopedToSession 是本次修复的主回归。
//
// 两个作用域 A/B 各存 3 条：A 按 MaxFiles=1 回收后 A 剩 1 条，**B 的 3 条一条
// 不少**；此后 B 按 MaxFiles=2 回收也只动 B，A 剩下的那条还在。
func TestSpillGCIsScopedToSession(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctxA := rig.WithSpillScope(context.Background(), "session-a")
	ctxB := rig.WithSpillScope(context.Background(), "session-b")
	spill := db.Spill()

	save := func(ctx context.Context, label string) string {
		t.Helper()
		ref, err := spill.Save(ctx, rig.ToolCall{Name: "bash", ID: label}, "内容-"+label)
		if err != nil {
			t.Fatalf("Save %s: %v", label, err)
		}
		return ref.ID
	}
	refsA := []string{save(ctxA, "a1"), save(ctxA, "a2"), save(ctxA, "a3")}
	refsB := []string{save(ctxB, "b1"), save(ctxB, "b2"), save(ctxB, "b3")}

	dropped, err := spill.GC(ctxA, 1)
	if err != nil {
		t.Fatalf("GC(ctxA, 1): %v", err)
	}
	if dropped != 2 {
		t.Fatalf("GC(ctxA, 1) 删了 %d 条，want 2（只算 A 自己的）", dropped)
	}
	// A 只剩最新那条。
	load := func(id string) error {
		_, err := spill.Load(context.Background(), id)
		return err
	}
	for _, id := range refsA[:2] {
		if err := load(id); !errors.Is(err, rig.ErrSpillNotFound) {
			t.Fatalf("A 的旧句柄 %s 没有被回收：%v", id, err)
		}
	}
	if err := load(refsA[2]); err != nil {
		t.Fatalf("A 的最新句柄被自己删掉了：%v", err)
	}
	// B 的 3 条一条都不能少：这是"跨会话互删"这条数据丢失 bug 的断言。
	for _, id := range refsB {
		if err := load(id); err != nil {
			t.Fatalf("B 的句柄 %s 被 A 的回收删掉了（跨会话数据丢失）：%v", id, err)
		}
	}

	dropped, err = spill.GC(ctxB, 2)
	if err != nil {
		t.Fatalf("GC(ctxB, 2): %v", err)
	}
	if dropped != 1 {
		t.Fatalf("GC(ctxB, 2) 删了 %d 条，want 1", dropped)
	}
	if err := load(refsB[0]); !errors.Is(err, rig.ErrSpillNotFound) {
		t.Fatalf("B 的旧句柄没有被回收：%v", err)
	}
	for _, id := range refsB[1:] {
		if err := load(id); err != nil {
			t.Fatalf("B 被保留的句柄 %s 读不回来：%v", id, err)
		}
	}
	// 反向：B 的回收不得动 A 剩下的那条。
	if err := load(refsA[2]); err != nil {
		t.Fatalf("A 剩下的句柄被 B 的回收删掉了：%v", err)
	}
}

// TestSpillEmptyScopeIsItsOwnPartition 钉住空作用域的语义：它只是"另一桶"，不是
// 通配符。老数据（scope 为空串）与不经过 agent 的调用方都落在这里。
func TestSpillEmptyScopeIsItsOwnPartition(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctxEmpty := context.Background()
	ctxSome := rig.WithSpillScope(context.Background(), "session-a")
	spill := db.Spill()

	save := func(ctx context.Context, label string) string {
		t.Helper()
		ref, err := spill.Save(ctx, rig.ToolCall{Name: "bash", ID: label}, "内容-"+label)
		if err != nil {
			t.Fatalf("Save %s: %v", label, err)
		}
		return ref.ID
	}
	emptyOld := save(ctxEmpty, "e1")
	emptyNew := save(ctxEmpty, "e2")
	scoped := save(ctxSome, "s1")

	if dropped, err := spill.GC(ctxEmpty, 1); err != nil || dropped != 1 {
		t.Fatalf("GC(空作用域, 1) = %d, %v；want 1, nil", dropped, err)
	}
	if _, err := spill.Load(ctxEmpty, emptyOld); !errors.Is(err, rig.ErrSpillNotFound) {
		t.Fatalf("空作用域的旧行没有被回收：%v", err)
	}
	if _, err := spill.Load(ctxEmpty, emptyNew); err != nil {
		t.Fatalf("空作用域的最新行被误删：%v", err)
	}
	// 关键断言：空作用域的 GC **不得**碰有作用域的行。
	if _, err := spill.Load(ctxEmpty, scoped); err != nil {
		t.Fatalf("空作用域的 GC 删掉了别的会话的句柄：%v", err)
	}
}

// v4SpillStatements 是 coreVersion 4 时代 rig_spill 的 DDL 逐字快照：**没有**
// scope 列。老库的表已经存在，所以 v5 只能靠 ALTER 加列。
const v4SpillTable = `CREATE TABLE rig_spill (
	seq        INTEGER PRIMARY KEY AUTOINCREMENT,
	id         TEXT    NOT NULL UNIQUE,
	tool       TEXT    NOT NULL DEFAULT '',
	bytes      INTEGER NOT NULL DEFAULT 0,
	content    TEXT    NOT NULL,
	created_at INTEGER NOT NULL)`

// buildV4Database 造一个"v4 时代的库"：rig_sessions 已有 format_version（v3），
// rig_spill 还没有 scope（v5），并放进去两条老溢出结果。
func buildV4Database(t *testing.T, path string) {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer func() { _ = raw.Close() }()
	for _, statement := range []string{
		`CREATE TABLE rig_sessions (
			id            TEXT PRIMARY KEY,
			title         TEXT    NOT NULL DEFAULT '',
			workspace_id  TEXT    NOT NULL DEFAULT '',
			created_at    INTEGER NOT NULL,
			updated_at    INTEGER NOT NULL,
			message_count INTEGER NOT NULL DEFAULT 0,
			bytes         INTEGER NOT NULL DEFAULT 0,
			revision      INTEGER NOT NULL DEFAULT 0,
			format_version INTEGER NOT NULL DEFAULT 1)`,
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
		`CREATE TABLE rig_memory_pages (
			name       TEXT PRIMARY KEY,
			title      TEXT    NOT NULL DEFAULT '',
			summary    TEXT    NOT NULL DEFAULT '',
			content    TEXT    NOT NULL DEFAULT '',
			updated_at INTEGER NOT NULL,
			bytes      INTEGER NOT NULL DEFAULT 0)`,
		v4SpillTable,
		`CREATE TABLE rig_import_state (
			session_id    TEXT PRIMARY KEY REFERENCES rig_sessions(id) ON DELETE CASCADE,
			source_digest TEXT    NOT NULL,
			message_count INTEGER NOT NULL)`,
		`CREATE TABLE rig_schema_extensions (name TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)`,
		`PRAGMA user_version = 4`,
		`INSERT INTO rig_spill(id, tool, bytes, content, created_at)
		 VALUES('legacy-1', 'bash', 6, '老结果一', 1700000000000)`,
		`INSERT INTO rig_spill(id, tool, bytes, content, created_at)
		 VALUES('legacy-2', 'bash', 6, '老结果二', 1700000000001)`,
	} {
		if _, err := raw.Exec(statement); err != nil {
			t.Fatalf("v4 建库失败（%s）: %v", statement, err)
		}
	}
}

// TestMigrateAddsSpillScopeColumn 是 coreVersion 5 的主回归：v4 老库升级后
// rig_spill 有新列、索引，老行的 scope 为空串，并且回收不会误删老行。
func TestMigrateAddsSpillScopeColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v4.sqlite")
	buildV4Database(t, path)

	// 升级前的库确实没有 scope 列（否则这条用例什么也没证明）。
	if columns := rawSpillColumns(t, path); has(columns, "scope") {
		t.Fatalf("v4 库不该有 scope 列，columns = %v", columns)
	}

	db, err := sqlitestore.Open(sqlitestore.Options{Path: path})
	if err != nil {
		t.Fatalf("Open(v4 → 最新): %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	// 版本号必须推进到本构建的 coreVersion：拿一个全新库作对照，不写死 5。
	fresh, _ := openTemp(t, sqlitestore.Options{})
	want := schemaOf(t, fresh).Version
	schema := schemaOf(t, db)
	if schema.Version != want {
		t.Fatalf("升级后 user_version = %d, want %d", schema.Version, want)
	}
	if want < 5 {
		t.Fatalf("coreVersion = %d：本次迁移至少要走到 v5", want)
	}
	if !has(schema.Indexes, "rig_spill_scope_seq") {
		t.Fatalf("升级后仍缺索引 rig_spill_scope_seq；indexes = %v", schema.Indexes)
	}
	if !has(rawSpillColumns(t, path), "scope") {
		t.Fatalf("升级后 rig_spill 仍缺 scope 列")
	}

	// 老行的 scope 必须是空串（列的默认值），而不是 NULL 或别的什么。
	for _, id := range []string{"legacy-1", "legacy-2"} {
		if scope := rawSpillScope(t, path, id); scope != "" {
			t.Fatalf("老行 %s 的 scope = %q，want 空串", id, scope)
		}
	}

	// 有作用域的回收不得误删老行（老行属于空作用域这一桶）。
	spill := db.Spill()
	if dropped, err := spill.GC(rig.WithSpillScope(ctx, "session-new"), 1); err != nil || dropped != 0 {
		t.Fatalf("GC(有作用域, 1) = %d, %v；want 0, nil", dropped, err)
	}
	// 空作用域的回收只按空作用域这一桶裁剪：keep=2 时一条都不删。
	if dropped, err := spill.GC(ctx, 2); err != nil || dropped != 0 {
		t.Fatalf("GC(空作用域, 2) = %d, %v；want 0, nil", dropped, err)
	}
	for _, id := range []string{"legacy-1", "legacy-2"} {
		if _, err := spill.Load(ctx, id); err != nil {
			t.Fatalf("升级后的老句柄 %s 读不回来：%v", id, err)
		}
	}
	// keep=1 时只删空作用域里最旧的那条，保留最新。
	if dropped, err := spill.GC(ctx, 1); err != nil || dropped != 1 {
		t.Fatalf("GC(空作用域, 1) = %d, %v；want 1, nil", dropped, err)
	}
	if _, err := spill.Load(ctx, "legacy-1"); !errors.Is(err, rig.ErrSpillNotFound) {
		t.Fatalf("空作用域最旧的老行没有被回收：%v", err)
	}
	if _, err := spill.Load(ctx, "legacy-2"); err != nil {
		t.Fatalf("空作用域最新的老行被误删：%v", err)
	}

	// 再开一次必须幂等（ALTER 不允许重复执行，靠执行器的列探测）。
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

// TestNewSpillRowsCarryTheirScope 钉住写入侧：Save 必须把 ctx 上的作用域写进新列，
// 否则回收时的分区从第一天就是坏的。
func TestNewSpillRowsCarryTheirScope(t *testing.T) {
	db, path := openTemp(t, sqlitestore.Options{})
	ctx := rig.WithSpillScope(context.Background(), "session-write")
	ref, err := db.Spill().Save(ctx, rig.ToolCall{Name: "bash", ID: "w1"}, "正文")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if scope := rawSpillScope(t, path, ref.ID); scope != "session-write" {
		t.Fatalf("新行的 scope = %q，want session-write", scope)
	}
}

// rawSpillColumns 直接读 pragma，绕开 Store（断言要能在"还没升级"时用）。
func rawSpillColumns(t *testing.T, path string) []string {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer func() { _ = raw.Close() }()
	rows, err := raw.Query(`SELECT name FROM pragma_table_info('rig_spill')`)
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

// rawSpillScope 读一条溢出结果的作用域。
func rawSpillScope(t *testing.T, path, id string) string {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer func() { _ = raw.Close() }()
	var scope string
	if err := raw.QueryRow(
		fmt.Sprintf(`SELECT scope FROM rig_spill WHERE id = ?`), id).Scan(&scope); err != nil {
		t.Fatalf("read scope of %s: %v", id, err)
	}
	return scope
}

// TestDeleteRemovesScopedSpillRows 钉住"删会话把它的溢出结果一起清掉"。
//
// 溢出回收改成按会话作用域之后，`rig_spill` 里属于被删会话的行没有任何回收路径
// （GC 只认当前作用域、keep<=0 直接 return），于是这些 ≥16KiB 的正文会永久留在库里。
func TestDeleteRemovesScopedSpillRows(t *testing.T) {
	store, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()

	meta, err := store.Create(ctx, sqlitestore.Meta{ID: "s-del"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	spill := store.Spill()
	for index := 0; index < 3; index++ {
		if _, err := spill.Save(rig.WithSpillScope(ctx, "s-del"), rig.ToolCall{Name: "bash"}, strings.Repeat("x", 32)); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}
	// 另一个会话的行不该被牵连。
	if _, err := spill.Save(rig.WithSpillScope(ctx, "keep-me"), rig.ToolCall{Name: "bash"}, "keep"); err != nil {
		t.Fatalf("Save(other): %v", err)
	}

	if err := store.Delete(ctx, meta.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	var remaining int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM rig_spill WHERE scope = ?`, "s-del").Scan(&remaining); err != nil {
		t.Fatalf("count spill: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("删会话后仍留下 %d 条 rig_spill 行（没有回收路径）", remaining)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM rig_spill WHERE scope = ?`, "keep-me").Scan(&remaining); err != nil {
		t.Fatalf("count other spill: %v", err)
	}
	if remaining != 1 {
		t.Fatalf("别的会话的溢出行被误删：剩 %d, want 1", remaining)
	}
}
