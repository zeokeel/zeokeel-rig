package sqlite_test

// ImportRows 的按 ordinal 对账：删过消息（库内留空洞）的会话再导入同一批时，
// 必须按"源 ordinal → 库内 ordinal"的集合补齐，而不是按切片位置判分叉。

import (
	"context"
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
