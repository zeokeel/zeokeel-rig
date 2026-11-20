package sqlite

// 内部用例：迁移账本自身的守卫。
//
// 这些用例不测"某个表有没有"，而是测**迁移机制**能不能守住两条纪律：
//  1. `migrations` 的键必须正好覆盖 1..coreVersion（漏一段 / 漏 bump 都会红）；
//  2. 每段里的语句必须可重复执行（ALTER 靠执行器探测列，其余靠 IF NOT EXISTS）。

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// TestCoreVersionMatchesMigrationLedger 是 coreVersion 的自动化护栏。
//
// 旧实现的护栏只是一行注释（"改 schema 必须手工 +1"），而 `rig_memory_pages`
// 就真的漏 bump 过一次。现在漏一段、漏 bump、多开一段，都会被这条用例拦住。
func TestCoreVersionMatchesMigrationLedger(t *testing.T) {
	for version := 1; version <= coreVersion; version++ {
		statements, ok := migrations[version]
		if !ok {
			t.Fatalf("migrations 缺 v%d（coreVersion = %d）", version, coreVersion)
		}
		if len(statements) == 0 {
			t.Fatalf("migrations[v%d] 是空的：空段等于把 user_version 写到一个没升级的库上", version)
		}
	}
	// 多开一段却没 bump coreVersion：那段永远不会执行，改 schema 的人会以为生效了。
	for version := range migrations {
		if version > coreVersion {
			t.Fatalf("migrations 里有 v%d，但 coreVersion = %d（漏 bump）", version, coreVersion)
		}
		if version < 1 {
			t.Fatalf("migrations 里有非法版本 v%d", version)
		}
	}
}

// TestMigrationStatementsAreRepeatable 逐条检查"重复执行是安全的"。
//
// 判据只有两种合法形状：
//   - `... IF NOT EXISTS ...`（CREATE TABLE/INDEX）；
//   - `ALTER TABLE ... ADD [COLUMN] ...`（由 execMigrationStatement 先探测列）。
//
// 出现第三种形状（例如裸 INSERT、DROP、RENAME）就必须同时给执行器加守卫，
// 否则"中断后再跑一次"会炸。
func TestMigrationStatementsAreRepeatable(t *testing.T) {
	for version, statements := range migrations {
		for index, statement := range statements {
			if strings.Contains(strings.ToUpper(statement), "IF NOT EXISTS") {
				continue
			}
			if _, _, ok := parseAddColumn(statement); ok {
				continue
			}
			t.Fatalf("migrations[%d][%d] 既不是 IF NOT EXISTS 也不是 ADD COLUMN，没有幂等保证：%s",
				version, index, firstLineOf(statement))
		}
	}
}

// expectedAlters 是迁移台账里允许出现的"给已有表加列"清单：段号 → 表.列。
//
// 段号一起钉住：历史段落不应被挪动（挪动意味着老库的升级路径变了）。新加一列时
// 在这里登记一行，测试仍然守住两条纪律——① 每条 ALTER 都是执行器认得的形状
// （parseAddColumn 的探测覆盖范围）；② 台账里没有"漏登记"的加列语句。
var expectedAlters = map[int][2]string{
	// v3：会话的消息载荷编码版本。
	3: {"rig_sessions", "format_version"},
	// v5：溢出结果的作用域（按会话分区回收）。
	5: {"rig_spill", "scope"},
	// v6：消息的可检索文本（解码列，替代对 payload 做 LIKE）。
	6: {"rig_messages", "search_text"},
	// v7 / v8：花费的工作区与 run 归属（一段只加一列）。
	7: {"rig_costs", "workspace_id"},
	8: {"rig_costs", "run_id"},
}

// TestMigrationLedgerKeepsAddColumnCapability 钉住"给已有表加列"的能力还在。
//
// 旧版本这个用例只看**最新一段**（当时最新是 v3，那一段正好是给 rig_sessions 加
// format_version 的 ALTER）。v4 是"只新增一张表"的段，天然没有 ALTER——于是"最新
// 一段必须有 ALTER"不再是条成立的不变量；v5 又加了一列（rig_spill.scope），于是
// "台账里只能有一个 ALTER"也不再成立。改成对着**允许清单**检查：每条 ALTER 的
// 表.列都对得上、落在预期段号上，并且清单里的每一项都真的在台账里。
func TestMigrationLedgerKeepsAddColumnCapability(t *testing.T) {
	seen := make(map[int]string, len(expectedAlters))
	for version := 1; version <= coreVersion; version++ {
		for _, statement := range migrations[version] {
			table, column, ok := parseAddColumn(statement)
			if !ok {
				continue
			}
			want, allowed := expectedAlters[version]
			if !allowed || want[0] != table || want[1] != column {
				t.Fatalf("migrations[%d] 的 ALTER 是 %s.%s，不在允许清单里（清单：%v）",
					version, table, column, expectedAlters)
			}
			if previous, twice := seen[version]; twice {
				t.Fatalf("migrations[%d] 里有多条 ALTER ... ADD COLUMN（%s 与 %s.%s）：一段只加一列",
					version, previous, table, column)
			}
			seen[version] = table + "." + column
		}
	}
	for version, want := range expectedAlters {
		if _, ok := seen[version]; !ok {
			t.Fatalf("migrations[%d] 缺少 %s.%s 的 ALTER：老库上的这一列永远不会存在",
				version, want[0], want[1])
		}
	}
	// 历史段落不被挪动：format_version 必须仍然在段 3（v5 的 scope 在段 5）。
	if got := seen[3]; got != "rig_sessions.format_version" {
		t.Fatalf("段 3 的 ALTER 是 %q，want rig_sessions.format_version（历史段落不应被挪动）", got)
	}
}

// TestAddColumnStatementRunsOnlyOnce 是幂等性的实证：直接在一个真事务里把 v3 的
// ALTER 跑两遍。没有列探测时第二遍会 `duplicate column name`。
func TestAddColumnStatementRunsOnlyOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "twice.sqlite")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.Exec(`CREATE TABLE rig_sessions (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	tx, err := raw.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	statement := `ALTER TABLE rig_sessions ADD COLUMN format_version INTEGER NOT NULL DEFAULT 1`
	for attempt := 1; attempt <= 2; attempt++ {
		if err := execMigrationStatement(context.Background(), tx, statement); err != nil {
			t.Fatalf("第 %d 次执行 ALTER: %v", attempt, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	var count int
	if err := raw.QueryRow(
		`SELECT count(*) FROM pragma_table_info('rig_sessions') WHERE name = 'format_version'`).Scan(&count); err != nil {
		t.Fatalf("count columns: %v", err)
	}
	if count != 1 {
		t.Fatalf("format_version 出现 %d 次，want 1", count)
	}
}

// TestParseAddColumn 覆盖识别器认得的形状与**不**认得的形状。
func TestParseAddColumn(t *testing.T) {
	cases := []struct {
		statement string
		table     string
		column    string
		ok        bool
	}{
		{`ALTER TABLE rig_sessions ADD COLUMN format_version INTEGER NOT NULL DEFAULT 1`, "rig_sessions", "format_version", true},
		{`alter table t add x TEXT`, "t", "x", true},
		{`ALTER TABLE "quoted table" ADD COLUMN "col name" TEXT`, "quoted table", "col name", true},
		{`CREATE TABLE IF NOT EXISTS t (id TEXT)`, "", "", false},
		{`ALTER TABLE t RENAME TO u`, "", "", false},
		{`ALTER TABLE t DROP COLUMN x`, "", "", false},
		{`DROP TABLE t`, "", "", false},
	}
	for _, tc := range cases {
		table, column, ok := parseAddColumn(tc.statement)
		if ok != tc.ok || table != tc.table || column != tc.column {
			t.Fatalf("parseAddColumn(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.statement, table, column, ok, tc.table, tc.column, tc.ok)
		}
	}
}

// TestApplyCoreMigrationsSkipsAppliedSegments：已经是最新的库不执行任何语句。
func TestApplyCoreMigrationsSkipsAppliedSegments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "current.sqlite")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer func() { _ = raw.Close() }()
	tx, err := raw.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	got, err := applyCoreMigrations(context.Background(), tx, coreVersion)
	if err != nil {
		t.Fatalf("applyCoreMigrations(coreVersion): %v", err)
	}
	if got != coreVersion {
		t.Fatalf("返回版本 = %d, want %d", got, coreVersion)
	}
	// 一句 DDL 都没执行，所以 coreVersion 之外什么都不该存在。
	if err := tx.QueryRow(`PRAGMA user_version`).Scan(&got); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if got != 0 {
		t.Fatalf("跳过路径写了 user_version = %d", got)
	}
}

// TestUpgradeRepairsAMissingIndex：分段之后**不能**丢掉旧实现的唯一自愈能力。
// 手工删掉一个 v1 索引，再从一个 v1 版本的库升级——索引必须被补回来。
func TestUpgradeRepairsAMissingIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "noindex.sqlite")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	for _, statement := range []string{
		`CREATE TABLE rig_sessions (id TEXT PRIMARY KEY, title TEXT NOT NULL DEFAULT '',
			workspace_id TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL, message_count INTEGER NOT NULL DEFAULT 0,
			bytes INTEGER NOT NULL DEFAULT 0, revision INTEGER NOT NULL DEFAULT 0)`,
		// 索引被"手工删掉"：v1 时代的库本来就没有它。
		`PRAGMA user_version = 1`,
	} {
		if _, err := raw.Exec(statement); err != nil {
			_ = raw.Close()
			t.Fatalf("建库失败（%s）: %v", statement, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}
	store, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = store.Close() }()
	schema, err := store.Schema(context.Background())
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}
	indexes := map[string]bool{}
	for _, name := range schema.Indexes {
		indexes[name] = true
	}
	for _, want := range []string{"rig_sessions_updated_at", "rig_messages_session_ordinal"} {
		if !indexes[want] {
			t.Fatalf("升级没有补回索引 %q；indexes = %v", want, schema.Indexes)
		}
	}
}
