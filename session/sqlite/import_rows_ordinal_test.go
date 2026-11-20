package sqlite_test

// ImportRows 的按 ordinal 对账：删过消息（库内留空洞）的会话再导入同一批时，
// 必须按"源 ordinal → 库内 ordinal"的集合补齐，而不是按切片位置判分叉。

import (
	"context"
	"fmt"
	"strings"
	"testing"

	rig "github.com/zeokeel/zeokeel-rig"
	sqlitestore "github.com/zeokeel/zeokeel-rig/session/sqlite"
)

// TestImportRowsFillsHolesAfterDelete 钉住"3 条 → 删掉第 2 条 → 导入同一批"：
// 无 Conflicts、库里仍是 3 条（空洞被填回同一个 ordinal）、高水位推进到 > 2。
func TestImportRowsFillsHolesAfterDelete(t *testing.T) {
	ctx := context.Background()
	target, path := openTemp(t, sqlitestore.Options{})
	rows := sqlitestore.SessionRows{
		Meta:     sqlitestore.Meta{ID: "holes-1", Title: "删过消息的会话"},
		Messages: []rig.Message{userMessage("一"), assistantMessage("二"), userMessage("三")},
	}
	if _, err := sqlitestore.ImportRows(ctx, target, rows); err != nil {
		t.Fatalf("ImportRows(第一次): %v", err)
	}
	if _, err := target.DeleteMessages(ctx, "holes-1", []int{1}); err != nil {
		t.Fatalf("DeleteMessages: %v", err)
	}
	if ordinals := rawMessageOrdinals(t, path, "holes-1"); len(ordinals) != 2 {
		t.Fatalf("删除后 ordinal = %v，want 2 条（0、2）", ordinals)
	}

	report, err := sqlitestore.ImportRows(ctx, target, rows)
	if err != nil {
		t.Fatalf("ImportRows(补洞): %v", err)
	}
	if len(report.Conflicts) != 0 {
		t.Fatalf("补洞报了冲突：%v（空洞不是分叉）", report.Conflicts)
	}
	if len(report.Failed) != 0 {
		t.Fatalf("补洞失败：%v", report.Failed)
	}
	if report.Imported != 1 {
		t.Fatalf("补洞报告 Imported = %d，want 1", report.Imported)
	}
	_, messages, err := target.Load(ctx, "holes-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(messages) != 3 || messages[1].Text() != "二" {
		t.Fatalf("补洞后历史 = %+v，want 3 条且第 2 条是「二」", messages)
	}
	if ordinals := rawMessageOrdinals(t, path, "holes-1"); len(ordinals) != 3 || ordinals[1] != 1 {
		t.Fatalf("补洞后 ordinal = %v，want [0 1 2]", ordinals)
	}
	if next := rawNextOrdinal(t, path, "holes-1"); next <= 2 {
		t.Fatalf("补洞后 next_ordinal = %d，want > 2（续写不能落回空洞）", next)
	}
}

// TestImportRowsStillDetectsContentConflict：同一个 ordinal 上内容不同仍然是冲突，
// 而且一条都不写——按 ordinal 对账放宽的只是"空洞"，不是"分叉"。
func TestImportRowsStillDetectsContentConflict(t *testing.T) {
	ctx := context.Background()
	target, _ := openTemp(t, sqlitestore.Options{})
	if _, err := target.Create(ctx, sqlitestore.Meta{ID: "conflict-ord-1", Title: "库内那份"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := target.AppendBatch(ctx, "conflict-ord-1", userMessage("一"), assistantMessage("库内回答")); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	rows := sqlitestore.SessionRows{
		Meta:     sqlitestore.Meta{ID: "conflict-ord-1", Title: "库内那份"},
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
		t.Fatalf("冲突记录没有定位到 ordinal 2：%q", report.Conflicts[0])
	}
	_, messages, err := target.Load(ctx, "conflict-ord-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(messages) != 2 || messages[1].Text() != "库内回答" {
		t.Fatalf("分叉之后库内历史被改动：%+v", messages)
	}
}

// TestImportRowsRejectsHolesWhenSourceStartsAfterZero 钉住"源历史不是从 ordinal 0
// 开始"时的保守路径：宿主删过头部消息（库内只剩 ordinal 1），投递的源只有一条
// 「二」——按切片位置对账会把它当成"库内缺 ordinal 0"补进去，历史里就出现两条
// 一模一样的内容（实测 `[0 1]` + `[二 二]`）。源无法表达库内坐标时必须记
// Conflicts 并**零写入**。
func TestImportRowsRejectsHolesWhenSourceStartsAfterZero(t *testing.T) {
	ctx := context.Background()
	target, path := openTemp(t, sqlitestore.Options{})
	if _, err := target.Create(ctx, sqlitestore.Meta{ID: "pruned-src", Title: "头部被删的会话"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := target.AppendBatch(ctx, "pruned-src", userMessage("一"), assistantMessage("二")); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if _, err := target.DeleteMessages(ctx, "pruned-src", []int{0}); err != nil {
		t.Fatalf("DeleteMessages: %v", err)
	}
	if ordinals := rawMessageOrdinals(t, path, "pruned-src"); fmt.Sprint(ordinals) != "[1]" {
		t.Fatalf("删除头部后 ordinal = %v，want [1]", ordinals)
	}

	rows := sqlitestore.SessionRows{
		Meta:     sqlitestore.Meta{ID: "pruned-src", Title: "头部被删的会话"},
		Messages: []rig.Message{assistantMessage("二")},
	}
	report, err := sqlitestore.ImportRows(ctx, target, rows)
	if err != nil {
		t.Fatalf("ImportRows: %v", err)
	}
	if len(report.Conflicts) != 1 {
		t.Fatalf("报告 Conflicts = %v，want 1 条（源不是从 ordinal 0 开始的完整历史）", report.Conflicts)
	}
	if !strings.Contains(report.Conflicts[0], "does not start at ordinal 0") {
		t.Fatalf("冲突原因 = %q，want 说清源历史不从 ordinal 0 开始", report.Conflicts[0])
	}
	if report.Imported != 0 {
		t.Fatalf("报告 Imported = %d，want 0（冲突时零写入）", report.Imported)
	}
	if ordinals := rawMessageOrdinals(t, path, "pruned-src"); fmt.Sprint(ordinals) != "[1]" {
		t.Fatalf("导入后 ordinal = %v，want [1]（不能补出重复内容）", ordinals)
	}
	_, messages, err := target.Load(ctx, "pruned-src")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(messages) != 1 || messages[0].Text() != "二" {
		t.Fatalf("导入后历史 = %+v，want 只有 1 条「二」", messages)
	}
}

// TestImportRowsHonorsStartOrdinalForPrunedSource 钉住可选字段 StartOrdinal：
// 源自己说得出"第一条是 ordinal 1"时按真实 ordinal 对账，不再报冲突、也不重复写。
func TestImportRowsHonorsStartOrdinalForPrunedSource(t *testing.T) {
	ctx := context.Background()
	target, path := openTemp(t, sqlitestore.Options{})
	if _, err := target.Create(ctx, sqlitestore.Meta{ID: "start-ord-1", Title: "带起始序号的源"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := target.AppendBatch(ctx, "start-ord-1", userMessage("一"), assistantMessage("二")); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if _, err := target.DeleteMessages(ctx, "start-ord-1", []int{0}); err != nil {
		t.Fatalf("DeleteMessages: %v", err)
	}

	rows := sqlitestore.SessionRows{
		Meta:         sqlitestore.Meta{ID: "start-ord-1", Title: "带起始序号的源"},
		StartOrdinal: 1,
		Messages:     []rig.Message{assistantMessage("二")},
	}
	report, err := sqlitestore.ImportRows(ctx, target, rows)
	if err != nil {
		t.Fatalf("ImportRows: %v", err)
	}
	if len(report.Conflicts) != 0 || len(report.Failed) != 0 {
		t.Fatalf("报告 = %+v，want 无冲突无失败（真实 ordinal 对得上）", report)
	}
	if len(report.Skipped) != 1 || report.Imported != 0 {
		t.Fatalf("报告 = %+v，want Skipped（源里这条已在库内）", report)
	}
	if ordinals := rawMessageOrdinals(t, path, "start-ord-1"); fmt.Sprint(ordinals) != "[1]" {
		t.Fatalf("导入后 ordinal = %v，want [1]", ordinals)
	}
}

// TestImportRowsFillsHolesByStartOrdinal：有 StartOrdinal 时补洞落在真实 ordinal 上
// （源只有 1 条也能补，因为坐标不再靠切片位置猜）。
func TestImportRowsFillsHolesByStartOrdinal(t *testing.T) {
	ctx := context.Background()
	target, path := openTemp(t, sqlitestore.Options{})
	if _, err := target.Create(ctx, sqlitestore.Meta{ID: "start-ord-2", Title: "按真实序号补洞"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := target.AppendBatch(ctx, "start-ord-2", userMessage("一"), assistantMessage("二"), userMessage("三")); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if _, err := target.DeleteMessages(ctx, "start-ord-2", []int{1}); err != nil {
		t.Fatalf("DeleteMessages: %v", err)
	}

	rows := sqlitestore.SessionRows{
		Meta:         sqlitestore.Meta{ID: "start-ord-2", Title: "按真实序号补洞"},
		StartOrdinal: 1,
		Messages:     []rig.Message{assistantMessage("二")},
	}
	report, err := sqlitestore.ImportRows(ctx, target, rows)
	if err != nil {
		t.Fatalf("ImportRows: %v", err)
	}
	if len(report.Conflicts) != 0 || len(report.Failed) != 0 {
		t.Fatalf("报告 = %+v，want 无冲突无失败", report)
	}
	if report.Imported != 1 {
		t.Fatalf("报告 Imported = %d，want 1", report.Imported)
	}
	if ordinals := rawMessageOrdinals(t, path, "start-ord-2"); fmt.Sprint(ordinals) != "[0 1 2]" {
		t.Fatalf("补洞后 ordinal = %v，want [0 1 2]", ordinals)
	}
	_, messages, err := target.Load(ctx, "start-ord-2")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(messages) != 3 || messages[1].Text() != "二" {
		t.Fatalf("补洞后历史 = %+v，want 3 条且第 2 条是「二」", messages)
	}
}
