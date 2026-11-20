package sqlite_test

// 导入的可选面（事件/花费/检查点）必须与消息一样幂等：重复导入同一份数据不能让
// 花费翻倍、也不能让事件重复追加。

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	rig "github.com/zeokeel/zeokeel-rig"
	sqlitestore "github.com/zeokeel/zeokeel-rig/session/sqlite"
)

// openImportStore 返回一个空的临时库。
func openImportStore(t *testing.T) *sqlitestore.Store {
	t.Helper()
	store, _ := openTemp(t, sqlitestore.Options{})
	return store
}

// TestImportRowsIsIdempotentForFaces 钉住"重复导入可选面也不翻倍"。
//
// 花费行在 At 为零值时用"现在"补齐，于是每次导入都写成一行新记录（2 次导入 = 2 行，
// 花费翻倍）；事件在 Seq<=0 时走 AppendEvent，每次也追加一条。
func TestImportRowsIsIdempotentForFaces(t *testing.T) {
	store := openImportStore(t)
	ctx := context.Background()
	rows := sqlitestore.SessionRows{
		Meta:     sqlitestore.Meta{ID: "s-faces", Title: "带可选面的会话"},
		Messages: []rig.Message{rig.NewMessage(rig.RoleUser, "hi")},
		Costs:    []sqlitestore.CostEntry{{Model: "m", PromptTokens: 10, CompletionTokens: 5, CostUSD: 1}},
		Events:   []rig.SessionEvent{{Type: "run.start", Payload: []byte(`{"a":1}`)}, {Type: "run.end", Payload: []byte(`{"b":2}`)}},
	}
	if _, err := sqlitestore.ImportRows(ctx, store, rows); err != nil {
		t.Fatalf("first import: %v", err)
	}
	if _, err := sqlitestore.ImportRows(ctx, store, rows); err != nil {
		t.Fatalf("second import: %v", err)
	}

	costs, err := store.SessionCosts(ctx, "s-faces")
	if err != nil {
		t.Fatalf("SessionCosts: %v", err)
	}
	if len(costs) != 1 {
		t.Fatalf("重复导入后花费行数 = %d, want 1（At 零值不该导致翻倍）", len(costs))
	}
	events, err := store.SessionLog().Events(ctx, "s-faces", 0, 0)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("重复导入后事件数 = %d, want 2（Seq 缺失也要幂等）", len(events))
	}
}

// TestVerifyExportDetectsMissingOptionalFace 钉住"可选面丢了要能发现"。
func TestVerifyExportDetectsMissingOptionalFace(t *testing.T) {
	store := openImportStore(t)
	ctx := context.Background()
	if _, err := store.Append(ctx, "s-verify", rig.NewMessage(rig.RoleUser, "hello")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := store.SaveCost(ctx, "s-verify", sqlitestore.CostEntry{Model: "m", PromptTokens: 1, CompletionTokens: 1, CostUSD: 1}); err != nil {
		t.Fatalf("SaveCost: %v", err)
	}
	dir := t.TempDir()
	if _, err := sqlitestore.ExportToDir(ctx, store, sqlitestore.ExportOptions{Dir: dir, IncludeCosts: true, Overwrite: true}); err != nil {
		t.Fatalf("ExportToDir: %v", err)
	}
	report, err := store.VerifyExport(ctx, dir)
	if err != nil {
		t.Fatalf("VerifyExport: %v", err)
	}
	if len(report.Mismatches) != 0 {
		t.Fatalf("完整导出不该有 mismatch：%+v", report.Mismatches)
	}

	// 删掉花费面：往返已经不可能还原，必须报出来。
	if err := os.RemoveAll(filepath.Join(dir, "costs")); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	report, err = store.VerifyExport(ctx, dir)
	if err != nil {
		t.Fatalf("VerifyExport: %v", err)
	}
	if len(report.Mismatches) == 0 {
		t.Fatal("花费面被删掉之后 VerifyExport 仍然报 0 mismatch")
	}
}
