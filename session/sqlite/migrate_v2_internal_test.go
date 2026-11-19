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

// TestLatestMigrationAddsAColumn 钉住"这一次真的有 ALTER"：如果哪天有人把 v3 段
// 改成又一个 CREATE TABLE，那"给已有表加列"的能力又没了。
func TestLatestMigrationAddsAColumn(t *testing.T) {
	statements := migrations[coreVersion]
	found := false
	for _, statement := range statements {
		if table, column, ok := parseAddColumn(statement); ok {
			found = true
			if table != "rig_sessions" || column != "format_version" {
				t.Fatalf("最新一段的 ALTER 是 %s.%s，want rig_sessions.format_version", table, column)
			}
		}
	}
	if !found {
		t.Fatalf("coreVersion 对应的段里没有 ALTER TABLE ... ADD COLUMN：%v", statements)
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
