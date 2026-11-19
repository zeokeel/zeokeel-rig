package sqlite_test

// The memory backend: the same wiki contract the file store implements, with the
// pages living beside the sessions they were learned in.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	rig "github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/memory"
	sqlitestore "github.com/zeokeel/zeokeel-rig/session/sqlite"
)

// TestMemoryWriteThenRead covers the write/read pair the model uses most.
func TestMemoryWriteThenRead(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	wiki := db.Memory()
	if err := wiki.Write(ctx, memory.Page{
		Name: "build-flaky-tests", Title: "构建偶发失败", Summary: "记录偶发失败的测试与重试策略", Content: "# 正文",
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	page, err := wiki.Read(ctx, "build-flaky-tests")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if page.Summary != "记录偶发失败的测试与重试策略" || !strings.Contains(page.Content, "正文") {
		t.Fatalf("page = %+v", page)
	}
	if page.Updated.IsZero() {
		t.Fatal("Updated was not filled in")
	}
}

// TestMemoryValidationMatchesTheContract keeps the database from accepting what
// the file store rejects: a bad name or a missing summary.
func TestMemoryValidationMatchesTheContract(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	wiki := db.Memory()
	if err := wiki.Write(ctx, memory.Page{Name: "Build Notes", Summary: "x"}); !errors.Is(err, memory.ErrInvalidName) {
		t.Fatalf("bad name = %v, want ErrInvalidName", err)
	}
	if err := wiki.Write(ctx, memory.Page{Name: "ok-name"}); !errors.Is(err, memory.ErrSummaryRequired) {
		t.Fatalf("missing summary = %v, want ErrSummaryRequired", err)
	}
	long := strings.Repeat("长", memory.MaxSummaryBytes)
	if err := wiki.Write(ctx, memory.Page{Name: "too-long", Summary: long}); !errors.Is(err, memory.ErrSummaryTooLong) {
		t.Fatalf("long summary = %v, want ErrSummaryTooLong", err)
	}
	if _, err := wiki.Read(ctx, "nope"); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("missing page = %v, want ErrNotFound", err)
	}
	if err := wiki.Delete(ctx, "nope"); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("delete missing = %v, want ErrNotFound", err)
	}
}

// TestMemoryIndexAndSearchReturnMetadataOnly pins the promise that makes the
// wiki usable in a small context window.
func TestMemoryIndexAndSearchReturnMetadataOnly(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	wiki := db.Memory()
	for _, name := range []string{"alpha", "beta"} {
		if err := wiki.Write(ctx, memory.Page{Name: name, Summary: "summary-" + name, Content: "body-" + name}); err != nil {
			t.Fatalf("Write %s: %v", name, err)
		}
	}
	index, err := wiki.Index(ctx)
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if len(index) != 2 {
		t.Fatalf("index = %d pages", len(index))
	}
	for _, meta := range index {
		if meta.Bytes == 0 {
			t.Fatalf("meta without a size: %+v", meta)
		}
	}
	hits, err := wiki.Search(ctx, "body-beta")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].Name != "beta" {
		t.Fatalf("search = %+v", hits)
	}
}

// TestMemoryAppendWritesTheLogSection covers the append rule: one dated entry,
// in the Log section, without touching the rest.
func TestMemoryAppendWritesTheLogSection(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	wiki := db.Memory()
	if err := wiki.Write(ctx, memory.Page{Name: "log-page", Summary: "带日志", Content: "# 正文"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := wiki.Append(ctx, "log-page", "第一次补充"); err != nil {
		t.Fatalf("Append: %v", err)
	}
	page, err := wiki.Read(ctx, "log-page")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !strings.Contains(page.Content, memory.LogHeading) || !strings.Contains(page.Content, "第一次补充") {
		t.Fatalf("content = %q", page.Content)
	}
	if !strings.Contains(page.Content, "# 正文") {
		t.Fatalf("append dropped the body: %q", page.Content)
	}
}

// TestMemoryRewriteRecordsThePreviousSize covers Write over an existing page:
// the reader should see that it was replaced, and how big it was.
func TestMemoryRewriteRecordsThePreviousSize(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	wiki := db.Memory()
	if err := wiki.Write(ctx, memory.Page{Name: "rewritten", Summary: "第一次", Content: "旧正文"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := wiki.Write(ctx, memory.Page{Name: "rewritten", Summary: "第二次", Content: "新正文"}); err != nil {
		t.Fatalf("Write again: %v", err)
	}
	page, err := wiki.Read(ctx, "rewritten")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !strings.Contains(page.Content, "rewritten") {
		t.Fatalf("rewrite log missing: %q", page.Content)
	}
	if !strings.Contains(page.Content, "新正文") {
		t.Fatalf("new body missing: %q", page.Content)
	}
}

// TestMemoryIndexReturnsEveryPage 是"后端不截断"的回归：工具层自己会截断并报
// "… and N more"，后端先截到 60 就把尾巴吃掉了（65 页时数据库后端会静默丢掉 5 页，
// 而文件后端能诚实地说"还有 5 页"）。
func TestMemoryIndexReturnsEveryPage(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	wiki := db.Memory()
	const pages = 65
	for index := 0; index < pages; index++ {
		name := fmt.Sprintf("page-%02d", index)
		if err := wiki.Write(ctx, memory.Page{Name: name, Summary: "第 " + name + " 页"}); err != nil {
			t.Fatalf("Write %s: %v", name, err)
		}
	}
	metas, err := wiki.Index(ctx)
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if len(metas) != pages {
		t.Fatalf("Index 返回 %d 页，want %d（后端不该自己截断）", len(metas), pages)
	}
}

// TestMemoryAppendRequiresAnExistingPage 钉住与另外两个后端一致的语义：Append 到
// 不存在的页返回 ErrNotFound，而不是顺手建一页。
func TestMemoryAppendRequiresAnExistingPage(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	if err := db.Memory().Append(ctx, "missing-page", "第一条"); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("Append 到不存在的页 = %v, want ErrNotFound", err)
	}
	if _, err := db.Memory().Read(ctx, "missing-page"); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("Append 之后出现了页：%v", err)
	}
}

// TestMemoryAppendDoesNotFakeARewrite 断言 append 不会在正文里留下
// `rewritten (was N bytes)` 那条假账：Append 不是重写，模型读到的正文必须干净。
func TestMemoryAppendDoesNotFakeARewrite(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	wiki := db.Memory()
	if err := wiki.Write(ctx, memory.Page{Name: "log-page", Summary: "日志页", Content: "# 日志页"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	for index := 0; index < 3; index++ {
		if err := wiki.Append(ctx, "log-page", fmt.Sprintf("第 %d 条", index)); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	page, err := wiki.Read(ctx, "log-page")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if strings.Contains(page.Content, "rewritten") {
		t.Fatalf("append 的正文里出现了 rewritten：\n%s", page.Content)
	}
	if got := strings.Count(page.Content, "- "); got != 3 {
		t.Fatalf("Log 段应恰好 3 条，得到 %d 条：\n%s", got, page.Content)
	}
}

// TestMemoryAppendIsAtomicUnderConcurrency 是数据丢失的回归：12 个并发 append，
// 每一条都必须留下痕迹（旧实现是"读→改→写"三步无事务，实测只剩 4 行且 12 次全
// 返回 nil）。
func TestMemoryAppendIsAtomicUnderConcurrency(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	wiki := db.Memory()
	if err := wiki.Write(ctx, memory.Page{Name: "hot", Summary: "并发追加目标", Content: "# hot"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	const writers = 12
	var wait sync.WaitGroup
	errs := make([]error, writers)
	for index := 0; index < writers; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			errs[index] = wiki.Append(ctx, "hot", fmt.Sprintf("writer-%02d", index))
		}(index)
	}
	wait.Wait()
	for index, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", index, err)
		}
	}
	page, err := wiki.Read(ctx, "hot")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	for index := 0; index < writers; index++ {
		marker := fmt.Sprintf("writer-%02d", index)
		if !strings.Contains(page.Content, marker) {
			t.Fatalf("%s 的追加丢了（并发写覆盖）：\n%s", marker, page.Content)
		}
	}
}

// TestMemorySearchRanksTitleOverSummaryOverBody 钉住排序：标题命中 > 摘要命中 >
// 只在正文命中（同档再按最近修改）。只按 updated_at 排会让"就叫这个名字"的页面
// 排在很久以前正文里提过一次的页面后面。
func TestMemorySearchRanksTitleOverSummaryOverBody(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	wiki := db.Memory()
	// 三页都要命中 "deploy"，但命中位置不同；正文命中那页最新，用来证明排序不是
	// 按 updated_at。
	if err := wiki.Write(ctx, memory.Page{Name: "body-hit", Summary: "无关摘要", Content: "# 杂项\n这里顺便提到 deploy"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := wiki.Write(ctx, memory.Page{Name: "summary-hit", Summary: "deploy 的流程", Content: "# 流程"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := wiki.Write(ctx, memory.Page{Name: "deploy", Title: "Deploy", Summary: "发布手册", Content: "# 发布"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	metas, err := wiki.Search(ctx, "deploy")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	var order []string
	for _, meta := range metas {
		order = append(order, meta.Name)
	}
	if len(order) != 3 || order[0] != "deploy" || order[1] != "summary-hit" || order[2] != "body-hit" {
		t.Fatalf("Search 顺序 = %v, want [deploy summary-hit body-hit]", order)
	}
}

// TestMemoryUpgradeFromV1KeepsMemoryAndSpillUsable 是 coreVersion 的回归。
//
// 构造一个"v1 库"（只有 v1 时代的表、user_version=1），用当前构建打开：新增的
// rig_memory_pages / rig_spill 必须被建出来，否则 Memory()/Spill() 会一直
// `no such table`（而且没有任何错误提示——这正是它漏 bump 时发生的事）。
func TestMemoryUpgradeFromV1KeepsMemoryAndSpillUsable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.sqlite")
	// v1 的 schema：就是当前 coreSchema 里"v1 时代已有"的那几张表。
	v1 := []string{
		`CREATE TABLE rig_sessions (
			id TEXT PRIMARY KEY, title TEXT NOT NULL DEFAULT '', workspace_id TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
			message_count INTEGER NOT NULL DEFAULT 0, bytes INTEGER NOT NULL DEFAULT 0,
			revision INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE rig_messages (
			session_id TEXT NOT NULL REFERENCES rig_sessions(id) ON DELETE CASCADE,
			ordinal INTEGER NOT NULL, payload BLOB NOT NULL)`,
		`CREATE TABLE rig_events (
			session_id TEXT NOT NULL, seq INTEGER NOT NULL, kind TEXT NOT NULL,
			at INTEGER NOT NULL, payload BLOB NOT NULL)`,
		`CREATE TABLE rig_costs (
			session_id TEXT NOT NULL, model TEXT NOT NULL DEFAULT '',
			prompt_tokens INTEGER NOT NULL DEFAULT 0, completion_tokens INTEGER NOT NULL DEFAULT 0,
			cached_tokens INTEGER NOT NULL DEFAULT 0, cost_usd REAL NOT NULL DEFAULT 0,
			at INTEGER NOT NULL)`,
		`CREATE TABLE rig_checkpoints (
			session_id TEXT PRIMARY KEY REFERENCES rig_sessions(id) ON DELETE CASCADE,
			payload BLOB NOT NULL, updated_at INTEGER NOT NULL)`,
		`CREATE TABLE rig_schema_extensions (name TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)`,
		`PRAGMA user_version = 1`,
	}
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	for _, statement := range v1 {
		if _, err := raw.Exec(statement); err != nil {
			_ = raw.Close()
			t.Fatalf("v1 建库失败（%s）: %v", statement, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	store, err := sqlitestore.Open(sqlitestore.Options{Path: path})
	if err != nil {
		t.Fatalf("Open(升级): %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()

	// 记忆面：写一页、列索引。
	if err := store.Memory().Write(ctx, memory.Page{Name: "upgraded", Summary: "升级之后仍然能用"}); err != nil {
		t.Fatalf("升级后 Memory().Write = %v（coreVersion 没 bump 时这里就是 no such table）", err)
	}
	if metas, err := store.Memory().Index(ctx); err != nil || len(metas) != 1 {
		t.Fatalf("升级后 Memory().Index = %v / %d 页", err, len(metas))
	}
	// 溢出结果面：存一条、读回来。
	ref, err := store.Spill().Save(ctx, rig.ToolCall{Name: "bash"}, "很大的输出")
	if err != nil {
		t.Fatalf("升级后 Spill().Save = %v（coreVersion 没 bump 时这里就是 no such table）", err)
	}
	if content, err := store.Spill().Load(ctx, ref.ID); err != nil || content != "很大的输出" {
		t.Fatalf("升级后 Spill().Load = %q, %v", content, err)
	}
}
