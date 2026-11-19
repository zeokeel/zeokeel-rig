package sqlite_test

// ImportDir 的"半截续跑"：上一次迁移可能在 Create 成功后、AppendBatch 写了一半时
// 中断。续跑必须从库内已有的条数按 ordinal 接着写，而不是把整份历史再追加一遍。
//
// 旧行为：源 3 条、库内 2 条 → 导入后 5 条（m1 m2 m1 m2 m3），且再跑一次还会继续涨。

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	sqlitestore "github.com/zeokeel/zeokeel-rig/session/sqlite"
	"github.com/zeokeel/zeokeel-rig/session/store"
)

// importFixture 造一个目录存储源，返回根目录与三条消息。
func importFixture(t *testing.T, texts ...string) (string, store.Meta) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "sessions")
	legacy, err := store.NewDir(dir)
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	meta, err := legacy.Create(context.Background(), store.Meta{Title: "续跑"})
	if err != nil {
		t.Fatalf("legacy Create: %v", err)
	}
	for index, text := range texts {
		if index%2 == 0 {
			if _, err := legacy.Append(context.Background(), meta.ID, userMessage(text)); err != nil {
				t.Fatalf("legacy Append(%s): %v", text, err)
			}
		} else {
			if _, err := legacy.Append(context.Background(), meta.ID, assistantMessage(text)); err != nil {
				t.Fatalf("legacy Append(%s): %v", text, err)
			}
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("legacy Close: %v", err)
	}
	return dir, meta
}

// textsOfMessages 把消息拍平成文本，便于断言顺序。
func textsOfMessages(messages []rig.Message) string {
	parts := make([]string, 0, len(messages))
	for _, message := range messages {
		parts = append(parts, message.Text())
	}
	return strings.Join(parts, ",")
}

// TestImportDirResumesPartialHistoryWithoutDuplicating 是本次修复的主用例。
func TestImportDirResumesPartialHistoryWithoutDuplicating(t *testing.T) {
	ctx := context.Background()
	dir, meta := importFixture(t, "m1", "m2", "m3")

	db, _ := openTemp(t, sqlitestore.Options{})
	// 模拟上次中断：会话行已建，只写进了前两条。
	if _, err := db.Create(ctx, sqlitestore.Meta{ID: meta.ID, Title: "续跑"}); err != nil {
		t.Fatalf("db.Create: %v", err)
	}
	if _, err := db.AppendBatch(ctx, meta.ID, userMessage("m1"), assistantMessage("m2")); err != nil {
		t.Fatalf("db.AppendBatch(前两条): %v", err)
	}

	report, err := sqlitestore.ImportDir(ctx, db, sqlitestore.ImportOptions{Dir: dir})
	if err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	if report.Imported != 1 {
		t.Fatalf("ImportDir.Imported = %d，want 1（续跑一个会话）", report.Imported)
	}
	importedMeta, messages, err := db.Load(ctx, meta.ID)
	if err != nil {
		t.Fatalf("db.Load: %v", err)
	}
	if got := textsOfMessages(messages); got != "m1,m2,m3" {
		t.Fatalf("导入后消息 = %q（共 %d 条），want \"m1,m2,m3\"：续跑不得重放已有消息",
			got, len(messages))
	}
	if importedMeta.Messages != 3 {
		t.Fatalf("元数据 Messages = %d，want 3", importedMeta.Messages)
	}

	// 完整重跑幂等：一条都不再追加。
	again, err := sqlitestore.ImportDir(ctx, db, sqlitestore.ImportOptions{Dir: dir})
	if err != nil {
		t.Fatalf("ImportDir(第二次): %v", err)
	}
	if again.Imported != 0 {
		t.Fatalf("第二次 ImportDir.Imported = %d，want 0", again.Imported)
	}
	if _, messages, err := db.Load(ctx, meta.ID); err != nil {
		t.Fatalf("第二次之后 db.Load: %v", err)
	} else if got := textsOfMessages(messages); got != "m1,m2,m3" {
		t.Fatalf("第二次导入后消息 = %q，want 幂等", got)
	}
}

// TestImportDirDryRunDoesNotTouchPartialSession：试运行对半截会话也必须零写入。
func TestImportDirDryRunDoesNotTouchPartialSession(t *testing.T) {
	ctx := context.Background()
	dir, meta := importFixture(t, "m1", "m2", "m3")

	db, _ := openTemp(t, sqlitestore.Options{})
	if _, err := db.Create(ctx, sqlitestore.Meta{ID: meta.ID, Title: "续跑"}); err != nil {
		t.Fatalf("db.Create: %v", err)
	}
	if _, err := db.AppendBatch(ctx, meta.ID, userMessage("m1"), assistantMessage("m2")); err != nil {
		t.Fatalf("db.AppendBatch: %v", err)
	}
	report, err := sqlitestore.ImportDir(ctx, db, sqlitestore.ImportOptions{Dir: dir, DryRun: true})
	if err != nil {
		t.Fatalf("ImportDir(DryRun): %v", err)
	}
	if report.Imported != 0 {
		t.Fatalf("DryRun.Imported = %d，want 0", report.Imported)
	}
	if report.Messages != 3 {
		t.Fatalf("DryRun.Messages = %d，want 3（预览要数出源里的消息）", report.Messages)
	}
	if _, messages, err := db.Load(ctx, meta.ID); err != nil {
		t.Fatalf("DryRun 之后 db.Load: %v", err)
	} else if got := textsOfMessages(messages); got != "m1,m2" {
		t.Fatalf("DryRun 写库了：消息 = %q，want 仍是 m1,m2", got)
	}
}

// TestImportDirReportsDatabaseAheadOfSource：库内比源还多（源被截短/不是同一个源）时
// 不追加、不静默当作"已完整"，而是记一条可诊断的冲突。
func TestImportDirReportsDatabaseAheadOfSource(t *testing.T) {
	ctx := context.Background()
	dir, meta := importFixture(t, "m1", "m2")

	db, _ := openTemp(t, sqlitestore.Options{})
	if _, err := db.Create(ctx, sqlitestore.Meta{ID: meta.ID, Title: "更长的库"}); err != nil {
		t.Fatalf("db.Create: %v", err)
	}
	if _, err := db.AppendBatch(ctx, meta.ID,
		userMessage("m1"), assistantMessage("m2"), userMessage("m3")); err != nil {
		t.Fatalf("db.AppendBatch: %v", err)
	}
	report, err := sqlitestore.ImportDir(ctx, db, sqlitestore.ImportOptions{Dir: dir})
	if err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	if report.Imported != 0 {
		t.Fatalf("ImportDir.Imported = %d，want 0（不得把 3 条再追加成 5 条）", report.Imported)
	}
	if len(report.Conflicts) != 1 {
		t.Fatalf("ImportDir.Conflicts = %v，want 恰好 1 条可诊断记录", report.Conflicts)
	}
	if !strings.Contains(report.Conflicts[0], meta.ID) {
		t.Fatalf("冲突记录没有点出会话 ID：%q", report.Conflicts[0])
	}
	if _, messages, err := db.Load(ctx, meta.ID); err != nil {
		t.Fatalf("db.Load: %v", err)
	} else if got := textsOfMessages(messages); got != "m1,m2,m3" {
		t.Fatalf("库内消息被改动：%q，want 原样 m1,m2,m3", got)
	}
}
