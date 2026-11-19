package sqlite_test

// 评审缺陷（第 3/4/6/7/9/10/11 条）的回归用例。
//
// 这些用例的共同点是：**都不依赖机器状态**——库文件建在 t.TempDir() 里，用户输入
// 自己造，平台差异不参与断言。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	rig "github.com/zeokeel/zeokeel-rig"
	sqlitestore "github.com/zeokeel/zeokeel-rig/session/sqlite"
	"github.com/zeokeel/zeokeel-rig/session/store"
)

func openStore(t *testing.T) *sqlitestore.Store {
	t.Helper()
	dir := t.TempDir()
	db, err := sqlitestore.Open(sqlitestore.Options{Path: filepath.Join(dir, "sessions.db")})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func textMessage(role rig.Role, text string) rig.Message {
	return rig.Message{Role: role, Content: []rig.ContentPart{{Type: rig.PartText, Text: text}}}
}

// 第 3 条：Close 与其余方法并发不得触发 DATA RACE / panic；关闭后统一 ErrClosed。
func TestCloseIsRaceFreeAndReportsClosed(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	meta, err := db.Create(ctx, sqlitestore.Meta{Title: "t"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_, _, _ = db.Load(ctx, meta.ID)
				_, _ = db.Append(ctx, meta.ID, textMessage(rig.RoleUser, "x"))
				_, _ = db.List(ctx, sqlitestore.Filter{})
				_ = db.SaveCost(ctx, meta.ID, sqlitestore.CostEntry{Model: "m"})
				_, _ = db.CostTotals(ctx, sqlitestore.CostFilter{})
				_, _ = db.Schema(ctx)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(time.Millisecond)
		_ = db.Close()
	}()
	wg.Wait()

	// 关闭之后：所有入口都返回 ErrClosed，且都不 panic。
	assertClosed := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, store.ErrClosed) {
			t.Fatalf("Close 之后 %s 的 err = %v，want ErrClosed", name, err)
		}
	}
	_, _, err = db.Load(ctx, meta.ID)
	assertClosed("Load", err)
	_, err = db.List(ctx, sqlitestore.Filter{})
	assertClosed("List", err)
	_, err = db.Create(ctx, sqlitestore.Meta{Title: "t2"})
	assertClosed("Create", err)
	_, err = db.Append(ctx, meta.ID, textMessage(rig.RoleUser, "y"))
	assertClosed("Append", err)
	_, err = db.AppendBatch(ctx, meta.ID, textMessage(rig.RoleUser, "y"))
	assertClosed("AppendBatch", err)
	_, err = db.Replace(ctx, meta.ID, nil)
	assertClosed("Replace", err)
	err = db.Delete(ctx, meta.ID)
	assertClosed("Delete", err)
	_, err = db.SearchMessages(ctx, sqlitestore.SearchQuery{Text: "x"})
	assertClosed("SearchMessages", err)
	_, err = db.SearchEvents(ctx, sqlitestore.EventQuery{Text: "x"})
	assertClosed("SearchEvents", err)
	err = db.SaveCost(ctx, meta.ID, sqlitestore.CostEntry{})
	assertClosed("SaveCost", err)
	_, err = db.CostTotals(ctx, sqlitestore.CostFilter{})
	assertClosed("CostTotals", err)
	_, err = db.CostByModel(ctx, sqlitestore.CostFilter{})
	assertClosed("CostByModel", err)
	_, err = db.Schema(ctx)
	assertClosed("Schema", err)
	_, err = db.History().Load(ctx, meta.ID)
	assertClosed("History.Load", err)
	err = db.History().Append(ctx, meta.ID, textMessage(rig.RoleUser, "z"))
	assertClosed("History.Append", err)
	_, err = db.Checkpoints().Load(ctx, meta.ID)
	assertClosed("Checkpoints.Load", err)
	err = db.SessionLog().AppendEvent(ctx, rig.SessionEvent{SessionID: meta.ID, Type: "t"})
	assertClosed("SessionLog.AppendEvent", err)
	// Close 幂等。
	if err := db.Close(); err != nil {
		t.Fatalf("第二次 Close = %v，want nil", err)
	}
}

// 第 4 条：三个漏了 usable() 的方法在关闭后返回 ErrClosed 而不是 panic。
func TestClosedMethodsDoNotPanic(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	_ = db.Close()
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("关闭之后调用仍然 panic：%v", recovered)
		}
	}()
	if _, err := db.CostTotals(ctx, sqlitestore.CostFilter{}); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("CostTotals err = %v, want ErrClosed", err)
	}
	if _, err := db.CostByModel(ctx, sqlitestore.CostFilter{}); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("CostByModel err = %v, want ErrClosed", err)
	}
	if _, err := db.Schema(ctx); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("Schema err = %v, want ErrClosed", err)
	}
}

// 第 9 条：Create 对已存在的 ID 返回 ErrExists（与目录存储一致）。
func TestCreateExistingIDReturnsErrExists(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	if _, err := db.Create(ctx, sqlitestore.Meta{ID: "dup", Title: "first"}); err != nil {
		t.Fatalf("首次 Create: %v", err)
	}
	_, err := db.Create(ctx, sqlitestore.Meta{ID: "dup", Title: "second"})
	if !errors.Is(err, store.ErrExists) {
		t.Fatalf("重复 Create 的 err = %v，want store.ErrExists", err)
	}
	// 原来的会话不能被覆盖。
	meta, _, err := db.Load(ctx, "dup")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if meta.Title != "first" {
		t.Fatalf("重复 Create 覆盖了既有会话：title = %q，want %q", meta.Title, "first")
	}
}

// 第 7 条：用户输入里的 LIKE 通配符必须按字面量匹配。
func TestEventSearchEscapesLikeWildcards(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	log := db.SessionLog()
	for _, event := range []rig.SessionEvent{
		{SessionID: "s1", Type: "note", Payload: []byte(`{"text":"100% done"}`)},
		{SessionID: "s1", Type: "note", Payload: []byte(`{"text":"no percent here"}`)},
		{SessionID: "s1", Type: "note", Payload: []byte(`{"text":"a_b"}`)},
		{SessionID: "s1", Type: "note", Payload: []byte(`{"text":"axb"}`)},
	} {
		if err := log.AppendEvent(ctx, event); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}
	hits, err := log.Search(ctx, "%", 50)
	if err != nil {
		t.Fatalf("Search(%%): %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("Search(\"%%\") 命中 %d 条，want 1 条（只有含字面量 %% 的那条）：%+v", len(hits), hitPayloads(hits))
	}
	hits, err = log.Search(ctx, "_", 50)
	if err != nil {
		t.Fatalf("Search(_): %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("Search(\"_\") 命中 %d 条，want 1 条（只有含字面量 _ 的那条）：%+v", len(hits), hitPayloads(hits))
	}
}

func hitPayloads(events []rig.SessionEvent) []string {
	out := make([]string, 0, len(events))
	for _, event := range events {
		out = append(out, string(event.Payload))
	}
	return out
}

// 第 8 条：Delete 必须在单事务里删会话与事件。
func TestDeleteRemovesEventsAtomically(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	if err := db.SessionLog().AppendEvent(ctx, rig.SessionEvent{SessionID: "s1", Type: "t"}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if err := db.Delete(ctx, "s1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// 会话以同一 ID 重建之后，旧事件不得"复活"。
	if _, err := db.Create(ctx, sqlitestore.Meta{ID: "s1", Title: "again"}); err != nil {
		t.Fatalf("重建 Create: %v", err)
	}
	events, err := db.SessionLog().Search(ctx, "", 50)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, event := range events {
		if event.SessionID == "s1" {
			t.Fatalf("删除后重建的会话带回了旧事件：%+v", event)
		}
	}
}

// 第 6 条：上次导入在 Create 之后、AppendBatch 之前中断，留下的空壳必须能续上。
func TestImportResumesIntoShellSession(t *testing.T) {
	ctx := context.Background()
	db := openStore(t)
	source := t.TempDir()
	dirStore, err := store.NewDir(source)
	if err != nil {
		t.Fatalf("NewDir: %v", err)
	}
	meta, err := dirStore.Create(ctx, store.Meta{ID: "resume-1", Title: "hello"})
	if err != nil {
		t.Fatalf("source Create: %v", err)
	}
	if _, err := dirStore.AppendBatch(ctx, meta.ID, textMessage(rig.RoleUser, "hi"), textMessage(rig.RoleAssistant, "hello")); err != nil {
		t.Fatalf("source AppendBatch: %v", err)
	}
	_ = dirStore.Close()

	// 造出"空壳"：会话行已存在，但一条消息都没有。
	if _, err := db.Create(ctx, sqlitestore.Meta{ID: "resume-1", Title: "hello"}); err != nil {
		t.Fatalf("shell Create: %v", err)
	}

	report, err := sqlitestore.ImportDir(ctx, db, sqlitestore.ImportOptions{Dir: source})
	if err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	if report.Imported != 1 {
		t.Fatalf("空壳会话没有被续导：report = %+v", report)
	}
	_, messages, err := db.Load(ctx, "resume-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("续导之后消息数 = %d，want 2", len(messages))
	}
	// 再跑一次：已经完整，必须跳过。
	again, err := sqlitestore.ImportDir(ctx, db, sqlitestore.ImportOptions{Dir: source})
	if err != nil {
		t.Fatalf("第二次 ImportDir: %v", err)
	}
	if again.Imported != 0 || len(again.Skipped) != 1 {
		t.Fatalf("完整会话应当被跳过：report = %+v", again)
	}
	if _, messages, _ := db.Load(ctx, "resume-1"); len(messages) != 2 {
		t.Fatalf("重复导入把消息导了两遍：%d 条", len(messages))
	}
}

// 第 10 条：Meta.Bytes / Meta.Model 目前**恒为 0/空**——这条用例把"文档声明"钉住，
// 一旦有人开始维护它们，测试会提醒同步改文档。
func TestMetaBytesAndModelAreDocumentedAsUnset(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	meta, err := db.Create(ctx, sqlitestore.Meta{ID: "meta-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	meta, err = db.Append(ctx, meta.ID, rig.Message{
		Role:    rig.RoleUser,
		Content: []rig.ContentPart{{Type: rig.PartText, Text: strings.Repeat("x", 100)}},
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if meta.Bytes != 0 {
		t.Fatalf("Meta.Bytes = %d，want 0（当前实现不维护它，见 docs/sqlite.md）", meta.Bytes)
	}
	if meta.Model != "" {
		t.Fatalf("Meta.Model = %q，want 空（当前实现不维护它，见 docs/sqlite.md）", meta.Model)
	}
}

// 第 5 条：提交之后回读不能用会被取消的 ctx。
func TestCommitSurvivesCancelledContext(t *testing.T) {
	db := openStore(t)
	ctx := context.Background()
	meta, err := db.Create(ctx, sqlitestore.Meta{ID: "cancel-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := db.Append(cancelled, meta.ID, textMessage(rig.RoleUser, "x")); err == nil {
		t.Fatalf("用已取消的 ctx 追加应当失败")
	}
	// 数据不能落库（事务没提交），也不能出现"报错但写进去了"。
	_, messages, err := db.Load(ctx, meta.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(messages) != 0 {
		t.Fatalf("取消的追加仍然写入了 %d 条消息", len(messages))
	}
	// 正常路径下，提交后的回读不受 ctx 影响。
	shortLived, cancelShort := context.WithCancel(ctx)
	updated, err := db.Append(shortLived, meta.ID, textMessage(rig.RoleUser, "y"))
	cancelShort()
	if err != nil {
		t.Fatalf("正常追加失败：%v", err)
	}
	if updated.Messages != 1 {
		t.Fatalf("返回的 Meta.Messages = %d，want 1", updated.Messages)
	}
}

var _ = os.Remove
