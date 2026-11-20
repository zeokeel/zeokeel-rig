package sqlite_test

// 导入/导出对称：Export 是改 schema 的唯一安全网，它的背面 ImportRows 必须能表达
// "一个会话的元数据 + 消息（可选事件/花费/检查点）"，而且重复导入同一份数据不能
// 产生第二份历史。可选面默认不导出（保持既有行为），打开开关后必须真的往返。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	rig "github.com/zeokeel/zeokeel-rig"
	sqlitestore "github.com/zeokeel/zeokeel-rig/session/sqlite"
)

// TestExportImportCompareSessionsAgreesOnTitle 是往返的主口径：库 → 目录 → 新库
// 之后，CompareSessions（含标题）必须报告"一致"。
//
// 旧实现里标题一定不同——目录后端按首条用户消息重扫，显式标题就这么丢了——所以
// 这条用例同时钉住"导出目录带版本化的会话元数据清单"。
func TestExportImportCompareSessionsAgreesOnTitle(t *testing.T) {
	ctx := context.Background()
	source, _ := openTemp(t, sqlitestore.Options{})
	created, err := source.Create(ctx, sqlitestore.Meta{Title: "宿主改过的标题", Workspace: "repo-a"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := source.AppendBatch(ctx, created.ID, userMessage("你好"), assistantMessage("在")); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "export")
	if err := source.Export(ctx, dir); err != nil {
		t.Fatalf("Export: %v", err)
	}

	target, _ := openTemp(t, sqlitestore.Options{})
	if _, err := sqlitestore.ImportDir(ctx, target, sqlitestore.ImportOptions{Dir: dir}); err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	got, messages, err := target.Load(ctx, created.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("往返后 %d 条消息，want 2", len(messages))
	}
	if got.Title != "宿主改过的标题" {
		t.Fatalf("往返后标题 = %q，want %q", got.Title, "宿主改过的标题")
	}
	if got.Workspace != "repo-a" {
		t.Fatalf("往返后工作区 = %q，want repo-a", got.Workspace)
	}
	report, err := sqlitestore.CompareSessions(ctx, source, target)
	if err != nil {
		t.Fatalf("CompareSessions: %v", err)
	}
	if len(report.Mismatches) != 0 {
		t.Fatalf("往返报告了差异：%+v", report.Mismatches)
	}
}

// newImportedRows 造一批"要导入的行"：元数据 + 两条消息 + 事件 + 花费 + 检查点。
func newImportedRows() sqlitestore.SessionRows {
	sessionID := "imported-1"
	return sqlitestore.SessionRows{
		Meta: sqlitestore.Meta{
			ID: sessionID, Title: "导入进来的标题", Workspace: "repo-a",
		},
		Messages: []rig.Message{userMessage("一"), assistantMessage("二")},
		Events: []rig.SessionEvent{
			{Seq: 1, SessionID: sessionID, Type: "turn.start", Payload: json.RawMessage(`{"n":1}`)},
			{Seq: 2, SessionID: sessionID, Type: "turn.end", Payload: json.RawMessage(`{"n":2}`)},
		},
		Costs: []sqlitestore.CostEntry{
			{Model: "m1", PromptTokens: 10, CompletionTokens: 5, CostUSD: 0.5,
				At: time.UnixMilli(1_700_000_000_000).UTC()},
		},
		Checkpoint: &rig.Checkpoint{
			SessionID: sessionID, TurnID: "turn-1", Messages: []rig.Message{userMessage("一")},
		},
	}
}

// TestImportRowsIsIdempotent 钉住"重复导入同一会话不产生重复消息"。
//
// 这是 ImportDir 的幂等承诺在新入口上的延续：宿主从任意来源（备份、另一个库、
// 宿主自己的格式）重复投递同一个会话，第二次必须是 Skipped，而不是把历史写两遍。
func TestImportRowsIsIdempotent(t *testing.T) {
	ctx := context.Background()
	target, _ := openTemp(t, sqlitestore.Options{})
	rows := newImportedRows()

	first, err := sqlitestore.ImportRows(ctx, target, rows)
	if err != nil {
		t.Fatalf("ImportRows(第一次): %v", err)
	}
	if first.Sessions != 1 || first.Imported != 1 || first.Messages != 2 || len(first.Failed) != 0 || len(first.Conflicts) != 0 {
		t.Fatalf("第一次导入报告 = %+v", first)
	}
	if first.Events != 2 || first.Costs != 1 || first.Checkpoints != 1 {
		t.Fatalf("可选面计数 = 事件 %d / 花费 %d / 检查点 %d，want 2 / 1 / 1",
			first.Events, first.Costs, first.Checkpoints)
	}

	second, err := sqlitestore.ImportRows(ctx, target, rows)
	if err != nil {
		t.Fatalf("ImportRows(第二次): %v", err)
	}
	if second.Imported != 0 || len(second.Skipped) != 1 || second.Skipped[0] != "imported-1" {
		t.Fatalf("第二次导入报告 = %+v（重复导入必须 Skipped）", second)
	}
	if len(second.Conflicts) != 0 {
		t.Fatalf("重复导入报了冲突：%v", second.Conflicts)
	}

	meta, messages, err := target.Load(ctx, "imported-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("重复导入后 %d 条消息，want 2", len(messages))
	}
	if meta.Title != "导入进来的标题" {
		t.Fatalf("标题 = %q，want 导入进来的标题", meta.Title)
	}
	if events, err := target.SessionLog().Events(ctx, "imported-1", 0, 0); err != nil || len(events) != 2 {
		t.Fatalf("重复导入后事件 %d 条（err %v），want 2", len(events), err)
	}
	if totals, err := target.CostTotals(ctx, sqlitestore.CostFilter{}); err != nil || totals.Requests != 1 {
		t.Fatalf("重复导入后花费 %d 行（err %v），want 1", totals.Requests, err)
	}
	checkpoint, err := target.Checkpoints().Load(ctx, "imported-1")
	if err != nil || checkpoint == nil || checkpoint.TurnID != "turn-1" {
		t.Fatalf("重复导入后检查点 = %+v / %v", checkpoint, err)
	}
}

// TestImportRowsReportsConflictWithoutDuplicating：库内历史与投递的行在中间分叉时，
// 必须给一条**可读**记录，并且一条消息都不写（既不能重复、也不能覆盖）。
func TestImportRowsReportsConflictWithoutDuplicating(t *testing.T) {
	ctx := context.Background()
	target, _ := openTemp(t, sqlitestore.Options{})
	if _, err := target.Create(ctx, sqlitestore.Meta{ID: "conflict-1", Title: "库内那份"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := target.AppendBatch(ctx, "conflict-1", userMessage("一"), assistantMessage("库内回答")); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	rows := sqlitestore.SessionRows{
		Meta:     sqlitestore.Meta{ID: "conflict-1", Title: "库内那份"},
		Messages: []rig.Message{userMessage("一"), assistantMessage("投递回答"), userMessage("三")},
	}
	report, err := sqlitestore.ImportRows(ctx, target, rows)
	if err != nil {
		t.Fatalf("ImportRows: %v", err)
	}
	if report.Imported != 0 || len(report.Conflicts) != 1 {
		t.Fatalf("分叉报告 = %+v（want Imported=0、1 条冲突）", report)
	}
	if !strings.Contains(report.Conflicts[0], "#2") {
		t.Fatalf("冲突记录没有定位到第几条：%q", report.Conflicts[0])
	}
	_, messages, err := target.Load(ctx, "conflict-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(messages) != 2 || messages[1].Text() != "库内回答" {
		t.Fatalf("分叉之后库内历史被改动：%+v", messages)
	}
}

// TestImportRowsRejectsEmptyID：没有会话 ID 的行集合无法定位，必须报错而不是
// 悄悄生成一个新会话（那样每次导入都会多出一个孤儿会话）。
func TestImportRowsRejectsEmptyID(t *testing.T) {
	ctx := context.Background()
	target, _ := openTemp(t, sqlitestore.Options{})
	if _, err := sqlitestore.ImportRows(ctx, target, sqlitestore.SessionRows{}); err == nil {
		t.Fatal("空 ID 的 ImportRows = nil error")
	}
}

// TestExportIncludeCostsRoundTrips：打开 IncludeCosts 之后花费行必须真的往返，
// 并且重复导入不产生第二份花费。
func TestExportIncludeCostsRoundTrips(t *testing.T) {
	ctx := context.Background()
	source, _ := openTemp(t, sqlitestore.Options{})
	meta, err := source.Create(ctx, sqlitestore.Meta{Title: "有花费的会话"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := source.Append(ctx, meta.ID, userMessage("问")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	for _, entry := range []sqlitestore.CostEntry{
		{Model: "m1", PromptTokens: 10, CompletionTokens: 5, CostUSD: 0.5},
		{Model: "m1", PromptTokens: 20, CompletionTokens: 7, CostUSD: 0.25},
	} {
		if err := source.SaveCost(ctx, meta.ID, entry); err != nil {
			t.Fatalf("SaveCost: %v", err)
		}
	}
	dir := filepath.Join(t.TempDir(), "export")
	report, err := sqlitestore.ExportToDir(ctx, source, sqlitestore.ExportOptions{Dir: dir, IncludeCosts: true})
	if err != nil {
		t.Fatalf("ExportToDir: %v", err)
	}
	if report.Costs != 2 {
		t.Fatalf("导出报告 Costs = %d，want 2", report.Costs)
	}
	if containsString(report.NotExported, "costs") {
		t.Fatalf("既然导出了花费，NotExported 不该再列 costs：%v", report.NotExported)
	}
	if !containsString(report.NotExported, "events") || !containsString(report.NotExported, "checkpoints") {
		t.Fatalf("没打开的仍要如实列出：%v", report.NotExported)
	}

	target, _ := openTemp(t, sqlitestore.Options{})
	imported, err := sqlitestore.ImportDir(ctx, target, sqlitestore.ImportOptions{Dir: dir})
	if err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	if imported.Costs != 2 {
		t.Fatalf("导入报告 Costs = %d，want 2", imported.Costs)
	}
	totals, err := target.CostTotals(ctx, sqlitestore.CostFilter{})
	if err != nil {
		t.Fatalf("CostTotals: %v", err)
	}
	if totals.Requests != 2 || totals.PromptTokens != 30 || totals.CompletionTokens != 12 || totals.CostUSD != 0.75 {
		t.Fatalf("往返后合计 = %+v", totals)
	}
	if _, err := sqlitestore.ImportDir(ctx, target, sqlitestore.ImportOptions{Dir: dir}); err != nil {
		t.Fatalf("第二次 ImportDir: %v", err)
	}
	again, err := target.CostTotals(ctx, sqlitestore.CostFilter{})
	if err != nil {
		t.Fatalf("CostTotals(第二次): %v", err)
	}
	if again != totals {
		t.Fatalf("重复导入把花费写了两遍：%+v → %+v", totals, again)
	}
}

// TestExportIncludeEventsAndCheckpointsRoundTrips：事件与检查点同样往返，并且
// 重复导入不会把事件按序号之外再插一份。
func TestExportIncludeEventsAndCheckpointsRoundTrips(t *testing.T) {
	ctx := context.Background()
	source, _ := openTemp(t, sqlitestore.Options{})
	meta, err := source.Create(ctx, sqlitestore.Meta{Title: "有事件与检查点的会话"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := source.Append(ctx, meta.ID, userMessage("问")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	for _, event := range []rig.SessionEvent{
		{SessionID: meta.ID, Type: "turn.start", Payload: json.RawMessage(`{"n":1}`)},
		{SessionID: meta.ID, Type: "turn.end", Payload: json.RawMessage(`{"n":2}`)},
	} {
		if err := source.SessionLog().AppendEvent(ctx, event); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}
	checkpoint := &rig.Checkpoint{
		SessionID: meta.ID, TurnID: "turn-1", Messages: []rig.Message{userMessage("问")},
	}
	if err := source.Checkpoints().Save(ctx, meta.ID, checkpoint); err != nil {
		t.Fatalf("Save checkpoint: %v", err)
	}

	dir := filepath.Join(t.TempDir(), "export")
	report, err := sqlitestore.ExportToDir(ctx, source, sqlitestore.ExportOptions{
		Dir: dir, IncludeEvents: true, IncludeCheckpoints: true,
	})
	if err != nil {
		t.Fatalf("ExportToDir: %v", err)
	}
	if report.Events != 2 || report.Checkpoints != 1 {
		t.Fatalf("导出报告 = 事件 %d / 检查点 %d，want 2 / 1", report.Events, report.Checkpoints)
	}

	target, _ := openTemp(t, sqlitestore.Options{})
	imported, err := sqlitestore.ImportDir(ctx, target, sqlitestore.ImportOptions{Dir: dir})
	if err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	if imported.Events != 2 || imported.Checkpoints != 1 {
		t.Fatalf("导入报告 = 事件 %d / 检查点 %d，want 2 / 1", imported.Events, imported.Checkpoints)
	}
	events, err := target.SessionLog().Events(ctx, meta.ID, 0, 0)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 2 || events[0].Type != "turn.start" || !strings.Contains(string(events[1].Payload), `"n":2`) {
		t.Fatalf("往返后事件 = %+v", events)
	}
	restored, err := target.Checkpoints().Load(ctx, meta.ID)
	if err != nil {
		t.Fatalf("Load checkpoint: %v", err)
	}
	if restored == nil || restored.TurnID != "turn-1" || len(restored.Messages) != 1 {
		t.Fatalf("往返后检查点 = %+v", restored)
	}

	// 第二次导入：事件不重复（同一 seq 覆盖/跳过），检查点仍是同一行。
	if _, err := sqlitestore.ImportDir(ctx, target, sqlitestore.ImportOptions{Dir: dir}); err != nil {
		t.Fatalf("第二次 ImportDir: %v", err)
	}
	again, err := target.SessionLog().Events(ctx, meta.ID, 0, 0)
	if err != nil {
		t.Fatalf("Events(第二次): %v", err)
	}
	if len(again) != 2 {
		t.Fatalf("重复导入后事件 %d 条，want 2", len(again))
	}
}

// TestExportDefaultOmitsOptionalFaces：三个开关默认关闭时，导出目录里不该出现
// 事件/花费/检查点的文件（保持既有行为），报告必须如实列出未导出的面。
func TestExportDefaultOmitsOptionalFaces(t *testing.T) {
	ctx := context.Background()
	source, _ := openTemp(t, sqlitestore.Options{})
	meta, err := source.Create(ctx, sqlitestore.Meta{Title: "默认导出"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := source.Append(ctx, meta.ID, userMessage("问")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := source.SessionLog().AppendEvent(ctx, rig.SessionEvent{SessionID: meta.ID, Type: "turn.start"}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if err := source.SaveCost(ctx, meta.ID, sqlitestore.CostEntry{Model: "m1", PromptTokens: 1}); err != nil {
		t.Fatalf("SaveCost: %v", err)
	}
	if err := source.Checkpoints().Save(ctx, meta.ID, &rig.Checkpoint{SessionID: meta.ID, TurnID: "t"}); err != nil {
		t.Fatalf("Save checkpoint: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "export")
	report, err := sqlitestore.ExportToDir(ctx, source, sqlitestore.ExportOptions{Dir: dir})
	if err != nil {
		t.Fatalf("ExportToDir: %v", err)
	}
	for _, face := range []string{"events", "costs", "checkpoints"} {
		if !containsString(report.NotExported, face) {
			t.Fatalf("NotExported = %v，缺 %q", report.NotExported, face)
		}
		if _, statErr := os.Stat(filepath.Join(dir, face)); !os.IsNotExist(statErr) {
			t.Fatalf("默认导出不该写出 %s/（stat err = %v）", face, statErr)
		}
	}
	if report.Events != 0 || report.Costs != 0 || report.Checkpoints != 0 {
		t.Fatalf("默认导出报告了可选面计数：%+v", report)
	}
}

// TestExportKeepsAnUnreadableManifest：目标目录里的清单读不懂时，导出不能把它
// 覆盖成"只剩这次的会话"——那可能是另一次导出的资产。报告要给出 Failed。
func TestExportKeepsAnUnreadableManifest(t *testing.T) {
	ctx := context.Background()
	source, _ := openTemp(t, sqlitestore.Options{})
	meta, err := source.Create(ctx, sqlitestore.Meta{Title: "会话"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := source.Append(ctx, meta.ID, userMessage("问")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	dir := t.TempDir()
	manifest := filepath.Join(dir, "sessions.meta.jsonl")
	const broken = "这不是 JSON\n"
	if err := os.WriteFile(manifest, []byte(broken), 0o600); err != nil {
		t.Fatalf("写坏清单: %v", err)
	}
	report, err := sqlitestore.ExportToDir(ctx, source, sqlitestore.ExportOptions{Dir: dir})
	if err != nil {
		t.Fatalf("ExportToDir: %v", err)
	}
	if len(report.Failed) == 0 {
		t.Fatalf("坏清单没有进 Failed：%+v", report)
	}
	data, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("读回清单: %v", err)
	}
	if string(data) != broken {
		t.Fatalf("读不懂的清单被覆盖了：%q", data)
	}
}
