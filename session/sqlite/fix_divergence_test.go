package sqlite_test

// ImportDir 的"库内是源前缀"假定：上一版只要条数不同就 `messages[have:]` 续写，
// **没有验证库内那几条真的就是源的前几条**。若库里是 `m1,X`（X 不属于源），
// 续跑会把 `m2,m3` 接在后面 → 历史变成 `m1,X,m2,m3`，静默污染，且 Conflicts 不报。
//
// 这组用例要求：续跑前做前缀一致性校验；不一致就不写库、记一条 Conflicts、
// 只跳过这一个会话（其它会话照常导入）。

import (
	"context"
	"database/sql"
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

	// 长度相等且一致：跳过（零冲突、零导入），并且**试运行不写指纹**。
	t.Run("equal length", func(t *testing.T) {
		dir, meta := importFixture(t, "m1", "m2")
		db, dbPath := openTemp(t, sqlitestore.Options{})
		if _, err := db.Create(ctx, sqlitestore.Meta{ID: meta.ID, Title: "等长"}); err != nil {
			t.Fatalf("db.Create: %v", err)
		}
		if _, err := db.AppendBatch(ctx, meta.ID, userMessage("m1"), assistantMessage("m2")); err != nil {
			t.Fatalf("db.AppendBatch: %v", err)
		}
		// 试运行：必须零写入——包括"顺手补写指纹"这一步。
		dry, err := sqlitestore.ImportDir(ctx, db, sqlitestore.ImportOptions{Dir: dir, DryRun: true})
		if err != nil {
			t.Fatalf("ImportDir(DryRun): %v", err)
		}
		if dry.Imported != 0 || len(dry.Conflicts) != 0 {
			t.Fatalf("DryRun Imported=%d Conflicts=%v，want 0/空", dry.Imported, dry.Conflicts)
		}
		if rows := digestRowCount(t, dbPath); rows != 0 {
			t.Fatalf("DryRun 写入 %d 行导入指纹：试运行必须零写入", rows)
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

// TestImportDirDetectsMidDivergenceAtEqualLength 补最后一个盲区：等长档此前只比**最后
// 一条**，所以"中间分叉、最后一条相同"（库 `m1,X,m3` vs 源 `m1,m2,m3`）会被当成
// 已完整跳过。
//
// 现在等长档按"导入时落库的源指纹"判断：库内指纹与当前源指纹不同即报冲突（不写库）。
// 旧库没有指纹记录时回退到"Load 全量逐条比"（一次性），比对一致就把指纹补写进去。
func TestImportDirDetectsMidDivergenceAtEqualLength(t *testing.T) {
	ctx := context.Background()
	dir, meta := importFixture(t, "m1", "m2", "m3")

	db, _ := openTemp(t, sqlitestore.Options{})
	if _, err := db.Create(ctx, sqlitestore.Meta{ID: meta.ID, Title: "中间分叉"}); err != nil {
		t.Fatalf("db.Create: %v", err)
	}
	// 中间那条换成源里没有的内容，最后一条与源相同——上一版只比最后一条，看不出来。
	if _, err := db.AppendBatch(ctx, meta.ID,
		userMessage("m1"), assistantMessage("X"), userMessage("m3")); err != nil {
		t.Fatalf("db.AppendBatch: %v", err)
	}

	report, err := sqlitestore.ImportDir(ctx, db, sqlitestore.ImportOptions{Dir: dir})
	if err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	if len(report.Conflicts) != 1 {
		t.Fatalf("Conflicts = %v，want 恰好 1 条（中间分叉在等长档也必须被发现）", report.Conflicts)
	}
	conflict := report.Conflicts[0]
	if !strings.Contains(conflict, "does not match source") || !strings.Contains(conflict, "#2") {
		t.Fatalf("Conflicts[0] 文案不对：%q", conflict)
	}
	if report.Imported != 0 || len(report.Skipped) != 0 {
		t.Fatalf("Imported=%d Skipped=%v，want 0/空", report.Imported, report.Skipped)
	}
	t.Logf("冲突原文：%s", conflict)

	_, messages, err := db.Load(ctx, meta.ID)
	if err != nil {
		t.Fatalf("db.Load: %v", err)
	}
	if got := textsOfMessages(messages); got != "m1,X,m3" {
		t.Fatalf("分叉会话被改写了：%q，want 原样 m1,X,m3", got)
	}
}

// TestImportDirDigestMakesEqualLengthRerunsCheap 证明"等长且一致"的重跑不再读消息：
// 首次导入后把 rig_messages 整张表删掉（保留 rig_sessions 与指纹记录），再跑一次仍然
// Skipped、零冲突、零失败——若实现去 Load 消息表，这里会报 no such table。
func TestImportDirDigestMakesEqualLengthRerunsCheap(t *testing.T) {
	ctx := context.Background()
	dir, meta := importFixture(t, "m1", "m2", "m3")

	db, dbPath := openTemp(t, sqlitestore.Options{})
	if _, err := db.Create(ctx, sqlitestore.Meta{ID: meta.ID, Title: "指纹"}); err != nil {
		t.Fatalf("db.Create: %v", err)
	}
	if _, err := db.AppendBatch(ctx, meta.ID, userMessage("m1"), assistantMessage("m2"), userMessage("m3")); err != nil {
		t.Fatalf("db.AppendBatch: %v", err)
	}

	// 第一次：旧库没有指纹记录 → 回退全量比对 → 一致 → 补写指纹 → Skipped。
	first, err := sqlitestore.ImportDir(ctx, db, sqlitestore.ImportOptions{Dir: dir})
	if err != nil {
		t.Fatalf("ImportDir(第一次): %v", err)
	}
	if first.Imported != 0 || len(first.Conflicts) != 0 {
		t.Fatalf("第一次 Imported=%d Conflicts=%v，want 0/空", first.Imported, first.Conflicts)
	}

	// 把消息表整个删掉：第二次若还去读消息，就会 failed（no such table）。
	raw, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.Exec(`DROP TABLE rig_messages`); err != nil {
		_ = raw.Close()
		t.Fatalf("DROP TABLE rig_messages: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}
	second, err := sqlitestore.ImportDir(ctx, db, sqlitestore.ImportOptions{Dir: dir})
	if err != nil {
		t.Fatalf("ImportDir(第二次): %v", err)
	}
	if len(second.Failed) != 0 {
		t.Fatalf("第二次 Failed = %v：说明等长档还在读消息表（指纹没生效）", second.Failed)
	}
	if len(second.Conflicts) != 0 || second.Imported != 0 {
		t.Fatalf("第二次 Conflicts=%v Imported=%d，want 空/0", second.Conflicts, second.Imported)
	}
	if len(second.Skipped) != 1 {
		t.Fatalf("第二次 Skipped = %v，want 1 条", second.Skipped)
	}
}

// digestRowCount 直接查 rig_import_state 的行数（用独立连接，绕过 Store），
// 用来断言"试运行没有写指纹"。
func digestRowCount(t *testing.T, dbPath string) int {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer func() { _ = raw.Close() }()
	var rows int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM rig_import_state`).Scan(&rows); err != nil {
		t.Fatalf("count rig_import_state: %v", err)
	}
	return rows
}
