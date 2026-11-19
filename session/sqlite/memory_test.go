package sqlite_test

// The memory backend: the same wiki contract the file store implements, with the
// pages living beside the sessions they were learned in.

import (
	"context"
	"errors"
	"strings"
	"testing"

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
