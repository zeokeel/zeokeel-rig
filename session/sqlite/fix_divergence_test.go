package sqlite_test

// ImportDir 的"库内是源前缀"假定：上一版只要条数不同就 `messages[have:]` 续写，
// **没有验证库内那几条真的就是源的前几条**。若库里是 `m1,X`（X 不属于源），
// 续跑会把 `m2,m3` 接在后面 → 历史变成 `m1,X,m2,m3`，静默污染，且 Conflicts 不报。
//
// 这组用例要求：续跑前做前缀一致性校验；不一致就不写库、记一条 Conflicts、
// 只跳过这一个会话（其它会话照常导入）。

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	sqlitestore "github.com/zeokeel/zeokeel-rig/session/sqlite"
	"github.com/zeokeel/zeokeel-rig/session/store"
)

// addSourceSession 在已关闭的源目录里追加一个会话（用于证明"其它会话照常导入"）。
func addSourceSession(t *testing.T, dir, text string) store.Meta {
	t.Helper()
	legacy, err := store.NewDir(dir)
	if err != nil {
		t.Fatalf("NewDir(%s): %v", dir, err)
	}
	defer func() { _ = legacy.Close() }()
	meta, err := legacy.Create(context.Background(), store.Meta{Title: text})
	if err != nil {
		t.Fatalf("Create(%s): %v", text, err)
	}
	if _, err := legacy.Append(context.Background(), meta.ID, userMessage(text)); err != nil {
		t.Fatalf("Append(%s): %v", text, err)
	}
	return meta
}

// TestImportDirRefusesToAppendAfterDivergence 是本次修复的主用例：库内第 2 条与源
// 不同 → 不续写（库内仍是 m1,X），Conflicts 恰好 1 条且点出 #2，别的会话照常导入。
func TestImportDirRefusesToAppendAfterDivergence(t *testing.T) {
	ctx := context.Background()
	dir, meta := importFixture(t, "m1", "m2", "m3")
	healthy := addSourceSession(t, dir, "healthy")

	db, _ := openTemp(t, sqlitestore.Options{})
	// 库内：m1（与源一致）+ X（源里没有的东西）——典型的"库与源已经分叉"。
	if _, err := db.Create(ctx, sqlitestore.Meta{ID: meta.ID, Title: "分叉"}); err != nil {
		t.Fatalf("db.Create: %v", err)
	}
	if _, err := db.AppendBatch(ctx, meta.ID, userMessage("m1"), assistantMessage("X")); err != nil {
		t.Fatalf("db.AppendBatch: %v", err)
	}

	report, err := sqlitestore.ImportDir(ctx, db, sqlitestore.ImportOptions{Dir: dir})
	if err != nil {
		t.Fatalf("ImportDir: %v", err)
	}

	if len(report.Conflicts) != 1 {
		t.Fatalf("Conflicts = %v，want 恰好 1 条（其它会话不该被牵连）", report.Conflicts)
	}
	conflict := report.Conflicts[0]
	if !strings.Contains(conflict, meta.ID) {
		t.Fatalf("Conflicts[0] 没有点出会话 ID：%q", conflict)
	}
	if !strings.Contains(conflict, "#2") {
		t.Fatalf("Conflicts[0] 没有指出第 2 条不一致：%q", conflict)
	}
	if !strings.Contains(conflict, "does not match source") {
		t.Fatalf("Conflicts[0] 文案不符合约定格式：%q", conflict)
	}
	if !strings.Contains(conflict, "existing=") || !strings.Contains(conflict, "source=") {
		t.Fatalf("Conflicts[0] 没有给出两边的内容：%q", conflict)
	}
	t.Logf("冲突原文：%s", conflict)

	_, messages, err := db.Load(ctx, meta.ID)
	if err != nil {
		t.Fatalf("db.Load: %v", err)
	}
	if got := textsOfMessages(messages); got != "m1,X" {
		t.Fatalf("分叉会话被写库了：%q（共 %d 条），want 原样 \"m1,X\"", got, len(messages))
	}

	// 其它会话不受影响：照常导入。
	healthyMeta, healthyMessages, err := db.Load(ctx, healthy.ID)
	if err != nil {
		t.Fatalf("db.Load(healthy): %v", err)
	}
	if got := textsOfMessages(healthyMessages); got != "healthy" {
		t.Fatalf("healthy 会话没被导入：%q", got)
	}
	if healthyMeta.Messages != 1 {
		t.Fatalf("healthy.Messages = %d，want 1", healthyMeta.Messages)
	}
	if report.Imported != 1 {
		t.Fatalf("report.Imported = %d，want 1（只有 healthy 被导入）", report.Imported)
	}

	// 再跑一次仍然拒绝、仍然不写库（幂等）。
	again, err := sqlitestore.ImportDir(ctx, db, sqlitestore.ImportOptions{Dir: dir})
	if err != nil {
		t.Fatalf("ImportDir(第二次): %v", err)
	}
	if len(again.Conflicts) != 1 {
		t.Fatalf("第二次 Conflicts = %v，want 1", again.Conflicts)
	}
	if _, messages, err := db.Load(ctx, meta.ID); err != nil {
		t.Fatalf("第二次之后 db.Load: %v", err)
	} else if got := textsOfMessages(messages); got != "m1,X" {
		t.Fatalf("第二次导入改动了分叉会话：%q", got)
	}
}

// TestImportDirResumesWhenPrefixMatches 是正对照：库内确实是源的前缀时照常续写。
//
// 前缀里刻意带工具调用、图片分片与 tool 关联字段——身份比较不能只看纯文本。
func TestImportDirResumesWhenPrefixMatches(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "sessions")
	legacy, err := store.NewDir(dir)
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	meta, err := legacy.Create(ctx, store.Meta{Title: "前缀一致"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	first := userMessage("m1")
	second := rig.Message{
		Role: rig.RoleAssistant,
		Content: []rig.ContentPart{
			rig.TextPart("先看截图"),
			rig.ImagePart("https://example.com/shot.png", "high"),
		},
		ToolCalls: []rig.ToolCall{{ID: "call-1", Name: "read_file", Arguments: `{"path":"a.go"}`}},
		Reasoning: "这条推理只是展示用",
	}
	third := rig.Message{Role: rig.RoleTool, ToolCallID: "call-1", Name: "read_file",
		Content: []rig.ContentPart{rig.TextPart("package a")}}
	fourth := userMessage("m4")
	if _, err := legacy.AppendBatch(ctx, meta.ID, first, second, third, fourth); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db, _ := openTemp(t, sqlitestore.Options{})
	if _, err := db.Create(ctx, sqlitestore.Meta{ID: meta.ID, Title: "前缀一致"}); err != nil {
		t.Fatalf("db.Create: %v", err)
	}
	// 库内只写前两条，且与源逐字段相同。
	if _, err := db.AppendBatch(ctx, meta.ID, first, second); err != nil {
		t.Fatalf("db.AppendBatch: %v", err)
	}

	report, err := sqlitestore.ImportDir(ctx, db, sqlitestore.ImportOptions{Dir: dir})
	if err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	if len(report.Conflicts) != 0 {
		t.Fatalf("前缀一致却被判冲突：%v", report.Conflicts)
	}
	if report.Imported != 1 {
		t.Fatalf("report.Imported = %d，want 1", report.Imported)
	}
	_, messages, err := db.Load(ctx, meta.ID)
	if err != nil {
		t.Fatalf("db.Load: %v", err)
	}
	if len(messages) != 4 {
		t.Fatalf("导入后 %d 条，want 4", len(messages))
	}
	if got := textsOfMessages(messages); got != "m1,先看截图,package a,m4" {
		t.Fatalf("导入后消息 = %q", got)
	}
	if len(messages[1].ToolCalls) != 1 || messages[1].ToolCalls[0].ID != "call-1" {
		t.Fatalf("工具调用没有随续写保持：%+v", messages[1])
	}
	if messages[2].ToolCallID != "call-1" {
		t.Fatalf("tool 关联字段丢了：%+v", messages[2])
	}
}

// TestImportDirPrefixCheckKeepsEmptyEdges：库内为空 / 源为空 / 长度相等三种边界
// 保持既有语义（空 → 全量写；等长 → 跳过），不前缀校验误伤。
func TestImportDirPrefixCheckKeepsEmptyEdges(t *testing.T) {
	ctx := context.Background()

	// 库内为空（0 条，但会话行已存在）：全量写入。
	t.Run("existing empty", func(t *testing.T) {
		dir, meta := importFixture(t, "m1", "m2")
		db, _ := openTemp(t, sqlitestore.Options{})
		if _, err := db.Create(ctx, sqlitestore.Meta{ID: meta.ID, Title: "空壳"}); err != nil {
			t.Fatalf("db.Create: %v", err)
		}
		report, err := sqlitestore.ImportDir(ctx, db, sqlitestore.ImportOptions{Dir: dir})
		if err != nil {
			t.Fatalf("ImportDir: %v", err)
		}
		if len(report.Conflicts) != 0 {
			t.Fatalf("空壳会话被判冲突：%v", report.Conflicts)
		}
		if _, messages, err := db.Load(ctx, meta.ID); err != nil {
			t.Fatalf("db.Load: %v", err)
		} else if got := textsOfMessages(messages); got != "m1,m2" {
			t.Fatalf("空壳会话导入 = %q，want m1,m2", got)
		}
	})

	// 长度相等：按既有语义跳过（不校验、不写）。
	t.Run("equal length", func(t *testing.T) {
		dir, meta := importFixture(t, "m1", "m2")
		db, _ := openTemp(t, sqlitestore.Options{})
		if _, err := db.Create(ctx, sqlitestore.Meta{ID: meta.ID, Title: "等长"}); err != nil {
			t.Fatalf("db.Create: %v", err)
		}
		if _, err := db.AppendBatch(ctx, meta.ID, userMessage("m1"), assistantMessage("m2")); err != nil {
			t.Fatalf("db.AppendBatch: %v", err)
		}
		report, err := sqlitestore.ImportDir(ctx, db, sqlitestore.ImportOptions{Dir: dir})
		if err != nil {
			t.Fatalf("ImportDir: %v", err)
		}
		if report.Imported != 0 || len(report.Conflicts) != 0 {
			t.Fatalf("等长情况 = Imported %d / Conflicts %v，want 0/0（既有语义是 Skipped）",
				report.Imported, report.Conflicts)
		}
		if len(report.Skipped) != 1 {
			t.Fatalf("Skipped = %v，want 1 条", report.Skipped)
		}
	})
}

// TestImportDirDetectsDivergenceAtEqualLength 补最后一个缺口：库内条数与源相等、
// 但内容不同（`m1,X` vs `m1,m2`）时，旧行为是"当作已完整"跳过——静默把分叉留在库里。
//
// 这一档只校验**最后一条**（ORDER BY ordinal DESC LIMIT 1），不读整份历史：等长分支是
// "重跑迁移"的常见路径，全量 Load 会让每次重跑都扫一遍所有会话。
func TestImportDirDetectsDivergenceAtEqualLength(t *testing.T) {
	ctx := context.Background()
	dir, meta := importFixture(t, "m1", "m2")

	db, _ := openTemp(t, sqlitestore.Options{})
	if _, err := db.Create(ctx, sqlitestore.Meta{ID: meta.ID, Title: "等长分叉"}); err != nil {
		t.Fatalf("db.Create: %v", err)
	}
	if _, err := db.AppendBatch(ctx, meta.ID, userMessage("m1"), assistantMessage("X")); err != nil {
		t.Fatalf("db.AppendBatch: %v", err)
	}

	// 试运行也要把这条冲突预览出来（宿主靠它决定要不要真的迁移），且零写入。
	dry, err := sqlitestore.ImportDir(ctx, db, sqlitestore.ImportOptions{Dir: dir, DryRun: true})
	if err != nil {
		t.Fatalf("ImportDir(DryRun): %v", err)
	}
	if len(dry.Conflicts) != 1 {
		t.Fatalf("DryRun.Conflicts = %v，want 1 条（预览也要报分叉）", dry.Conflicts)
	}
	if dry.Imported != 0 {
		t.Fatalf("DryRun.Imported = %d，want 0", dry.Imported)
	}
	if _, messages, err := db.Load(ctx, meta.ID); err != nil {
		t.Fatalf("DryRun 之后 db.Load: %v", err)
	} else if got := textsOfMessages(messages); got != "m1,X" {
		t.Fatalf("DryRun 写库了：%q，want 仍是 m1,X", got)
	}

	report, err := sqlitestore.ImportDir(ctx, db, sqlitestore.ImportOptions{Dir: dir})
	if err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	if len(report.Conflicts) != 1 {
		t.Fatalf("Conflicts = %v，want 恰好 1 条（等长但内容分叉必须报出来）", report.Conflicts)
	}
	conflict := report.Conflicts[0]
	if !strings.Contains(conflict, "does not match source") {
		t.Fatalf("Conflicts[0] 文案不符合约定格式：%q", conflict)
	}
	if !strings.Contains(conflict, "#2") {
		t.Fatalf("Conflicts[0] 应指出最后一条（#2）：%q", conflict)
	}
	if report.Imported != 0 || len(report.Skipped) != 0 {
		t.Fatalf("Imported=%d Skipped=%v，want 0/空（分叉会话既不该导入也不该算跳过）",
			report.Imported, report.Skipped)
	}
	t.Logf("冲突原文：%s", conflict)

	_, messages, err := db.Load(ctx, meta.ID)
	if err != nil {
		t.Fatalf("db.Load: %v", err)
	}
	if got := textsOfMessages(messages); got != "m1,X" {
		t.Fatalf("分叉会话被改写了：%q（%d 条），want 原样 m1,X", got, len(messages))
	}
}
