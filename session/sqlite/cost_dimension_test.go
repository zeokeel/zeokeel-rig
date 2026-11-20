package sqlite_test

// 成本维度的回归：`rig_costs` 只记 `session_id`，于是"这个工作区花了多少""这个
// 子代理 run 花了多少"要么算不出、要么归到别的名下。这里给每条记录加上
// `workspace_id` / `run_id`（空串 = 未设置），并提供按会话、按工作区的聚合。
//
// 同一个文件还钉住"删会话不留孤儿花费"：`rig_costs` 上没有外键可用，Delete 必须
// 显式把该会话的行一起删掉，否则成本视图会给一个已经不存在的会话记账。

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	sqlitestore "github.com/zeokeel/zeokeel-rig/session/sqlite"
)

// TestCostDimensionsFilterAndGroup 是成本维度的主回归：同一会话两条成本（不同
// model / workspace / run）→ 过滤与分组都必须分得清。
func TestCostDimensionsFilterAndGroup(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	a, err := db.Create(ctx, sqlitestore.Meta{Title: "a"})
	if err != nil {
		t.Fatalf("Create a: %v", err)
	}
	b, err := db.Create(ctx, sqlitestore.Meta{Title: "b"})
	if err != nil {
		t.Fatalf("Create b: %v", err)
	}
	for _, entry := range []struct {
		session string
		value   sqlitestore.CostEntry
	}{
		{a.ID, sqlitestore.CostEntry{Model: "m1", WorkspaceID: "ws-a", RunID: "run-1", PromptTokens: 10, CostUSD: 0.10}},
		{a.ID, sqlitestore.CostEntry{Model: "m2", WorkspaceID: "ws-b", RunID: "run-2", PromptTokens: 20, CostUSD: 0.05}},
		{b.ID, sqlitestore.CostEntry{Model: "m1", WorkspaceID: "ws-b", RunID: "run-3", PromptTokens: 30, CostUSD: 0.30}},
	} {
		if err := db.SaveCost(ctx, entry.session, entry.value); err != nil {
			t.Fatalf("SaveCost(%+v): %v", entry.value, err)
		}
	}

	// 过滤：两个新维度都要真的参与 WHERE，而不是被忽略。
	for _, tc := range []struct {
		filter   sqlitestore.CostFilter
		requests int
		cost     float64
	}{
		{sqlitestore.CostFilter{WorkspaceID: "ws-a"}, 1, 0.10},
		{sqlitestore.CostFilter{RunID: "run-2"}, 1, 0.05},
		{sqlitestore.CostFilter{WorkspaceID: "ws-b", SessionID: a.ID}, 1, 0.05},
		{sqlitestore.CostFilter{WorkspaceID: "ws-a", SessionID: b.ID}, 0, 0},
	} {
		totals, err := db.CostTotals(ctx, tc.filter)
		if err != nil {
			t.Fatalf("CostTotals(%+v): %v", tc.filter, err)
		}
		if totals.Requests != tc.requests || !near(totals.CostUSD, tc.cost) {
			t.Fatalf("CostTotals(%+v) = %d 条 / %v，want %d 条 / %v",
				tc.filter, totals.Requests, totals.CostUSD, tc.requests, tc.cost)
		}
	}

	// 按会话分组：花费从高到低，b 0.30 在前，a 两条合计 0.15。
	bySession, err := db.CostBySession(ctx, sqlitestore.CostFilter{})
	if err != nil {
		t.Fatalf("CostBySession: %v", err)
	}
	if len(bySession) != 2 {
		t.Fatalf("按会话分组 = %d 组，want 2：%+v", len(bySession), bySession)
	}
	if bySession[0].SessionID != b.ID || bySession[0].Requests != 1 || !near(bySession[0].CostUSD, 0.30) {
		t.Fatalf("第一组 = %+v，want b 1 条 0.30", bySession[0])
	}
	if bySession[1].SessionID != a.ID || bySession[1].Requests != 2 ||
		bySession[1].PromptTokens != 30 || !near(bySession[1].CostUSD, 0.15) {
		t.Fatalf("第二组 = %+v，want a 2 条 30 prompt / 0.15", bySession[1])
	}

	// 按工作区分组：ws-b 两条合计 0.35，ws-a 一条 0.10。
	byWorkspace, err := db.CostByWorkspace(ctx, sqlitestore.CostFilter{})
	if err != nil {
		t.Fatalf("CostByWorkspace: %v", err)
	}
	if len(byWorkspace) != 2 {
		t.Fatalf("按工作区分组 = %d 组，want 2：%+v", len(byWorkspace), byWorkspace)
	}
	if byWorkspace[0].WorkspaceID != "ws-b" || byWorkspace[0].Requests != 2 || !near(byWorkspace[0].CostUSD, 0.35) {
		t.Fatalf("第一组 = %+v，want ws-b 2 条 0.35", byWorkspace[0])
	}
	if byWorkspace[1].WorkspaceID != "ws-a" || byWorkspace[1].Requests != 1 || !near(byWorkspace[1].CostUSD, 0.10) {
		t.Fatalf("第二组 = %+v，want ws-a 1 条 0.10", byWorkspace[1])
	}

	// 过滤与分组叠加：会话 a 在 ws-b 里只有 0.05 那一条。
	scoped, err := db.CostByWorkspace(ctx, sqlitestore.CostFilter{SessionID: a.ID, WorkspaceID: "ws-b"})
	if err != nil {
		t.Fatalf("CostByWorkspace(scoped): %v", err)
	}
	if len(scoped) != 1 || scoped[0].Requests != 1 || !near(scoped[0].CostUSD, 0.05) {
		t.Fatalf("过滤后的分组 = %+v，want 1 组 1 条 0.05", scoped)
	}
}

// TestDeleteRemovesCostRows 钉住"删会话不留孤儿"：删除之后成本视图里查不到它，
// 别的会话的账一条不少。
func TestDeleteRemovesCostRows(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	a, err := db.Create(ctx, sqlitestore.Meta{Title: "a"})
	if err != nil {
		t.Fatalf("Create a: %v", err)
	}
	b, err := db.Create(ctx, sqlitestore.Meta{Title: "b"})
	if err != nil {
		t.Fatalf("Create b: %v", err)
	}
	if err := db.SaveCost(ctx, a.ID, sqlitestore.CostEntry{Model: "m", WorkspaceID: "ws", RunID: "r1", CostUSD: 0.02}); err != nil {
		t.Fatalf("SaveCost a: %v", err)
	}
	if err := db.SaveCost(ctx, b.ID, sqlitestore.CostEntry{Model: "m", WorkspaceID: "ws", RunID: "r2", CostUSD: 0.01}); err != nil {
		t.Fatalf("SaveCost b: %v", err)
	}
	if err := db.Delete(ctx, a.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	gone, err := db.CostTotals(ctx, sqlitestore.CostFilter{SessionID: a.ID})
	if err != nil {
		t.Fatalf("CostTotals(deleted): %v", err)
	}
	if gone.Requests != 0 || gone.CostUSD != 0 {
		t.Fatalf("删掉的会话仍有 %+v 的成本行：孤儿没清", gone)
	}
	remaining, err := db.CostTotals(ctx, sqlitestore.CostFilter{})
	if err != nil {
		t.Fatalf("CostTotals(all): %v", err)
	}
	if remaining.Requests != 1 || !near(remaining.CostUSD, 0.01) {
		t.Fatalf("剩下的成本 = %+v，want 只有 b 的 0.01", remaining)
	}
	bySession, err := db.CostBySession(ctx, sqlitestore.CostFilter{})
	if err != nil {
		t.Fatalf("CostBySession: %v", err)
	}
	if len(bySession) != 1 || bySession[0].SessionID != b.ID {
		t.Fatalf("按会话分组 = %+v，want 只剩 b", bySession)
	}
}

// v5CostsTable 是 coreVersion 5 时代 rig_costs 的 DDL 逐字快照：**没有**
// workspace_id / run_id。老库的表已经存在，新列只能靠 ALTER 加上。
const v5CostsTable = `CREATE TABLE rig_costs (
	session_id        TEXT    NOT NULL,
	model             TEXT    NOT NULL DEFAULT '',
	prompt_tokens     INTEGER NOT NULL DEFAULT 0,
	completion_tokens INTEGER NOT NULL DEFAULT 0,
	cached_tokens     INTEGER NOT NULL DEFAULT 0,
	cost_usd          REAL    NOT NULL DEFAULT 0,
	at                INTEGER NOT NULL)`

// TestCostRowsMigrateIntoEmptyDimensionBuckets：v5 老库升级后，老花费行的两个
// 新维度是空串，并且仍然能被聚合（不能被升级"丢账"）。
func TestCostRowsMigrateIntoEmptyDimensionBuckets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v5-costs.sqlite")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	for _, statement := range []string{
		v5CostsTable,
		// v5 的 rig_spill 已经有 scope：段 5 的索引修复会碰它，缺表就等于"v5 库是坏的"。
		`CREATE TABLE rig_spill (
			seq        INTEGER PRIMARY KEY AUTOINCREMENT,
			id         TEXT    NOT NULL UNIQUE,
			tool       TEXT    NOT NULL DEFAULT '',
			bytes      INTEGER NOT NULL DEFAULT 0,
			content    TEXT    NOT NULL,
			created_at INTEGER NOT NULL,
			scope      TEXT    NOT NULL DEFAULT '')`,
		`PRAGMA user_version = 5`,
		`INSERT INTO rig_costs(session_id, model, prompt_tokens, completion_tokens, cached_tokens, cost_usd, at)
		 VALUES('legacy-session', 'old-model', 7, 3, 1, 0.25, 1700000000000)`,
	} {
		if _, err := raw.Exec(statement); err != nil {
			_ = raw.Close()
			t.Fatalf("v5 建库失败（%s）: %v", statement, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	db, err := sqlitestore.Open(sqlitestore.Options{Path: path})
	if err != nil {
		t.Fatalf("Open(v5 → 最新): %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	total, err := db.CostTotals(ctx, sqlitestore.CostFilter{SessionID: "legacy-session"})
	if err != nil {
		t.Fatalf("CostTotals(legacy): %v", err)
	}
	if total.Requests != 1 || total.PromptTokens != 7 || !near(total.CostUSD, 0.25) {
		t.Fatalf("老花费行 = %+v，want 1 条 / 7 prompt / 0.25", total)
	}
	byWorkspace, err := db.CostByWorkspace(ctx, sqlitestore.CostFilter{})
	if err != nil {
		t.Fatalf("CostByWorkspace: %v", err)
	}
	if len(byWorkspace) != 1 || byWorkspace[0].WorkspaceID != "" ||
		byWorkspace[0].Requests != 1 || !near(byWorkspace[0].CostUSD, 0.25) {
		t.Fatalf("老行的工作区分组 = %+v，want 空串桶里的 1 条 0.25", byWorkspace)
	}
	bySession, err := db.CostBySession(ctx, sqlitestore.CostFilter{})
	if err != nil {
		t.Fatalf("CostBySession: %v", err)
	}
	if len(bySession) != 1 || bySession[0].SessionID != "legacy-session" {
		t.Fatalf("老行的会话分组 = %+v，want legacy-session", bySession)
	}
	dimensions := rawStrings(t, path, `SELECT workspace_id || '/' || run_id FROM rig_costs`)
	if len(dimensions) != 1 || dimensions[0] != "/" {
		t.Fatalf("老行的两个新维度 = %v，want 空串", dimensions)
	}
}

// near 比较金额（REAL 累加没有精确表示）。
func near(got, want float64) bool {
	diff := got - want
	if diff < 0 {
		diff = -diff
	}
	return diff < 1e-9
}
