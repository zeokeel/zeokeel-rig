package memwiki_test

// The exported wrappers exist so a storage backend outside this package can
// implement the same rules. These tests pin what they promise: the semantics the
// file store applies.

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/internal/memwiki"
)

// TestValidateNameIsASlug covers both halves of the rule.
func TestValidateNameIsASlug(t *testing.T) {
	if err := memwiki.ValidateName("build-flaky-tests"); err != nil {
		t.Fatalf("valid name rejected: %v", err)
	}
	for _, bad := range []string{"", "Build Notes", "有中文", strings.Repeat("a", memwiki.MaxNameLength+1)} {
		if err := memwiki.ValidateName(bad); !errors.Is(err, memwiki.ErrInvalidName) {
			t.Fatalf("ValidateName(%q) = %v, want ErrInvalidName", bad, err)
		}
	}
}

// TestNormalizeSummaryCollapsesAndBounds covers the one-line rule.
func TestNormalizeSummaryCollapsesAndBounds(t *testing.T) {
	got, err := memwiki.NormalizeSummary("  第一行\n第二行  ")
	if err != nil {
		t.Fatalf("NormalizeSummary: %v", err)
	}
	if got != "第一行 第二行" {
		t.Fatalf("normalised = %q", got)
	}
	if _, err := memwiki.NormalizeSummary("   "); !errors.Is(err, memwiki.ErrSummaryRequired) {
		t.Fatalf("blank summary = %v, want ErrSummaryRequired", err)
	}
	long := strings.Repeat("长", memwiki.MaxSummaryBytes)
	if _, err := memwiki.NormalizeSummary(long); !errors.Is(err, memwiki.ErrSummaryTooLong) {
		t.Fatalf("long summary = %v, want ErrSummaryTooLong", err)
	}
}

// TestNormalizeTitleFallsBackToTheName keeps a page readable when it has no title.
func TestNormalizeTitleFallsBackToTheName(t *testing.T) {
	if got := memwiki.NormalizeTitle("build-flaky-tests", "  "); got != "build-flaky-tests" {
		t.Fatalf("empty title = %q", got)
	}
	if got := memwiki.NormalizeTitle("n", "  构建 偶发失败 "); got != "构建 偶发失败" {
		t.Fatalf("title = %q", got)
	}
}

// TestAppendLogWritesIntoTheLogSection covers the append rule a second backend
// must reproduce exactly.
func TestAppendLogWritesIntoTheLogSection(t *testing.T) {
	got := memwiki.AppendLog("# 正文", "第一次补充")
	if !strings.Contains(got, memwiki.LogHeading) {
		t.Fatalf("no Log heading: %q", got)
	}
	if !strings.Contains(got, "第一次补充") || !strings.Contains(got, "# 正文") {
		t.Fatalf("append lost content: %q", got)
	}
	// Appending to a page that already has a Log section adds to it rather than
	// opening a second one.
	again := memwiki.AppendLog(got, "第二次补充")
	if strings.Count(again, memwiki.LogHeading) != 1 {
		t.Fatalf("duplicate Log heading: %q", again)
	}
	if !strings.Contains(again, "第二次补充") {
		t.Fatalf("second append missing: %q", again)
	}
	// Empty text changes nothing.
	if memwiki.AppendLog(got, "   ") != got {
		t.Fatal("blank append changed the page")
	}
}

// TestRewriteLogRecordsThePreviousSize covers what a Write over an existing page
// leaves behind.
func TestRewriteLogRecordsThePreviousSize(t *testing.T) {
	got := memwiki.RewriteLog("# 新正文", 128)
	if !strings.Contains(got, memwiki.LogHeading) {
		t.Fatalf("no Log heading: %q", got)
	}
	if !strings.Contains(got, "128") {
		t.Fatalf("previous size missing: %q", got)
	}
}

// TestSortMetasOrdersByUpdateThenName pins the order Index and Search promise.
func TestSortMetasOrdersByUpdateThenName(t *testing.T) {
	now := time.Now().UTC()
	metas := []memwiki.PageMeta{
		{Name: "b", Updated: now},
		{Name: "a", Updated: now},
		{Name: "older", Updated: now.Add(-time.Hour)},
	}
	memwiki.SortMetas(metas)
	if metas[0].Name != "a" || metas[1].Name != "b" || metas[2].Name != "older" {
		t.Fatalf("order = %q, %q, %q", metas[0].Name, metas[1].Name, metas[2].Name)
	}
}
