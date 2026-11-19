package memory_test

// memory 包是"规则 + 契约"的公开门面：这里逐条钉住每个导出的 wrapper 与常量，
// 保证它们确实转发到 internal/memwiki 的那一份语义（而不是各自悄悄改行为）。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/memory"
)

func TestValidateNameAcceptsSlugsOnly(t *testing.T) {
	// 注意：连字符可以出现在任何位置（契约就是这么定的），所以 "-lead" 合法。
	for _, name := range []string{"a", "build-notes", "a1-b2", "-lead", "trail-", strings.Repeat("x", memory.MaxNameLength)} {
		if err := memory.ValidateName(name); err != nil {
			t.Fatalf("ValidateName(%q) = %v, want nil", name, err)
		}
	}
	for _, name := range []string{"", "Build Notes", "UPPER", "_under", "dot.name", strings.Repeat("x", memory.MaxNameLength+1), "有中文"} {
		err := memory.ValidateName(name)
		if !errors.Is(err, memory.ErrInvalidName) {
			t.Fatalf("ValidateName(%q) = %v, want ErrInvalidName", name, err)
		}
	}
}

func TestNormalizeSummaryValidatesAndCollapses(t *testing.T) {
	got, err := memory.NormalizeSummary("  两行\n\t摘要  ")
	if err != nil {
		t.Fatalf("NormalizeSummary: %v", err)
	}
	if got != "两行 摘要" {
		t.Fatalf("NormalizeSummary = %q, want 折叠空白后的单行", got)
	}
	if _, err := memory.NormalizeSummary("   "); !errors.Is(err, memory.ErrSummaryRequired) {
		t.Fatalf("空白摘要 = %v, want ErrSummaryRequired", err)
	}
	long := strings.Repeat("长", memory.MaxSummaryBytes)
	if _, err := memory.NormalizeSummary(long); !errors.Is(err, memory.ErrSummaryTooLong) {
		t.Fatalf("超长摘要 = %v, want ErrSummaryTooLong", err)
	}
}

func TestNormalizeTitleFallsBackToName(t *testing.T) {
	if got := memory.NormalizeTitle("build-notes", ""); got != "build-notes" {
		t.Fatalf("空标题 = %q, want 页名", got)
	}
	if got := memory.NormalizeTitle("build-notes", "  构建笔记 "); got != "构建笔记" {
		t.Fatalf("标题 = %q, want 去空白后的标题", got)
	}
}

func TestAppendLogWritesDatedEntriesUnderTheHeading(t *testing.T) {
	content := memory.AppendLog("# 页面", "第一条")
	if !strings.Contains(content, memory.LogHeading) {
		t.Fatalf("AppendLog 没有建 %q 段：\n%s", memory.LogHeading, content)
	}
	if !strings.Contains(content, "第一条") {
		t.Fatalf("AppendLog 丢了正文：\n%s", content)
	}
	today := time.Now().UTC().Format("2006-01-02")
	if !strings.Contains(content, today) {
		t.Fatalf("AppendLog 没带日期前缀（%s）：\n%s", today, content)
	}
	again := memory.AppendLog(content, "第二条")
	if strings.Count(again, memory.LogHeading) != 1 {
		t.Fatalf("Log 段被重复创建：\n%s", again)
	}
	if !strings.Contains(again, "第二条") {
		t.Fatalf("第二次追加丢了内容：\n%s", again)
	}
}

func TestRewriteLogRecordsThePreviousSize(t *testing.T) {
	content := memory.RewriteLog("# 页面", 1234)
	if !strings.Contains(content, "rewritten") || !strings.Contains(content, "1234") {
		t.Fatalf("RewriteLog 没记下上一个大小：\n%s", content)
	}
}

func TestSortMetasNewestFirstThenName(t *testing.T) {
	same := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	metas := []memory.PageMeta{
		{Name: "b", Updated: same},
		{Name: "a", Updated: same},
		{Name: "old", Updated: same.Add(-time.Hour)},
	}
	memory.SortMetas(metas)
	if metas[0].Name != "a" || metas[1].Name != "b" || metas[2].Name != "old" {
		t.Fatalf("SortMetas = %v, want [a b old]", []string{metas[0].Name, metas[1].Name, metas[2].Name})
	}
}

func TestClampTextKeepsUTF8Intact(t *testing.T) {
	// 「长」是 3 字节：上限 4 只能容下一个完整字符，不能切出半个。
	got, total := memory.ClampText("长长", 4)
	if got != "长" {
		t.Fatalf("ClampText = %q, want 一个完整字符", got)
	}
	if total != 6 {
		t.Fatalf("ClampText 返回的原始长度 = %d, want 6", total)
	}
	short, total := memory.ClampText("abc", 8)
	if short != "abc" || total != 3 {
		t.Fatalf("ClampText(短文本) = %q/%d", short, total)
	}
}

func TestConstantsMatchTheContract(t *testing.T) {
	// 这些数字是公开契约的一部分：改了它们就是改了模型看到的行为。
	if memory.MaxSummaryBytes != 200 || memory.MaxNameLength != 64 {
		t.Fatalf("上限漂移：MaxSummaryBytes=%d MaxNameLength=%d", memory.MaxSummaryBytes, memory.MaxNameLength)
	}
	if memory.MaxIndexResults != 60 || memory.MaxSearchResults != 20 {
		t.Fatalf("结果上限漂移：index=%d search=%d", memory.MaxIndexResults, memory.MaxSearchResults)
	}
	if memory.LogHeading != "## Log" {
		t.Fatalf("LogHeading = %q", memory.LogHeading)
	}
}

// mapWiki 只用**公共 API** 实现 memory.Wiki。
//
// 它存在的意义是钉死"接口没有泄漏未导出类型"：如果 Wiki/Page/PageMeta 里的任何
// 字段来自 internal/memwiki 的未导出类型，这个外部包就编译不过。
type mapWiki struct {
	pages map[string]memory.Page
}

func (w *mapWiki) Index(context.Context) ([]memory.PageMeta, error) {
	metas := make([]memory.PageMeta, 0, len(w.pages))
	for _, page := range w.pages {
		metas = append(metas, memory.PageMeta{
			Name: page.Name, Title: memory.NormalizeTitle(page.Name, page.Title),
			Summary: page.Summary, Updated: page.Updated, Bytes: len(page.Content),
		})
	}
	memory.SortMetas(metas)
	return metas, nil
}

func (w *mapWiki) Read(_ context.Context, name string) (memory.Page, error) {
	page, ok := w.pages[name]
	if !ok {
		return memory.Page{}, memory.ErrNotFound
	}
	return page, nil
}

func (w *mapWiki) Write(_ context.Context, page memory.Page) error {
	if err := memory.ValidateName(page.Name); err != nil {
		return err
	}
	summary, err := memory.NormalizeSummary(page.Summary)
	if err != nil {
		return err
	}
	page.Summary = summary
	page.Title = memory.NormalizeTitle(page.Name, page.Title)
	if page.Updated.IsZero() {
		page.Updated = time.Now().UTC()
	}
	if previous, ok := w.pages[page.Name]; ok {
		page.Content = memory.RewriteLog(page.Content, len(previous.Content))
	}
	w.pages[page.Name] = page
	return nil
}

func (w *mapWiki) Append(_ context.Context, name, text string) error {
	page, ok := w.pages[name]
	if !ok {
		return memory.ErrNotFound
	}
	page.Content = memory.AppendLog(page.Content, text)
	page.Updated = time.Now().UTC()
	w.pages[name] = page
	return nil
}

func (w *mapWiki) Delete(_ context.Context, name string) error {
	if _, ok := w.pages[name]; !ok {
		return memory.ErrNotFound
	}
	delete(w.pages, name)
	return nil
}

func (w *mapWiki) Search(_ context.Context, query string) ([]memory.PageMeta, error) {
	all, err := w.Index(context.Background())
	if err != nil {
		return nil, err
	}
	needle := strings.TrimSpace(query)
	if needle == "" {
		return nil, nil
	}
	var out []memory.PageMeta
	for _, meta := range all {
		if strings.Contains(meta.Name, needle) || strings.Contains(meta.Summary, needle) ||
			strings.Contains(w.pages[meta.Name].Content, needle) {
			out = append(out, meta)
		}
	}
	return out, nil
}

// TestWikiIsImplementableFromOutside 用外部包实现一遍接口并跑一遍，证明契约只依赖
// 公共类型（这是"接口没泄漏 internal 类型"的编译期 + 运行期双重证据）。
func TestWikiIsImplementableFromOutside(t *testing.T) {
	var wiki memory.Wiki = &mapWiki{pages: map[string]memory.Page{}}
	ctx := context.Background()
	if err := wiki.Write(ctx, memory.Page{Name: "build-notes", Summary: "构建笔记", Content: "# 正文"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	page, err := wiki.Read(ctx, "build-notes")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if page.Summary != "构建笔记" {
		t.Fatalf("Read 回来 = %+v", page)
	}
	metas, err := wiki.Index(ctx)
	if err != nil || len(metas) != 1 {
		t.Fatalf("Index = %v / %d", err, len(metas))
	}
	hits, err := wiki.Search(ctx, "正文")
	if err != nil || len(hits) != 1 {
		t.Fatalf("Search = %v / %d", err, len(hits))
	}
	if err := wiki.Append(ctx, "build-notes", "追加一条"); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := wiki.Delete(ctx, "build-notes"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := wiki.Read(ctx, "build-notes"); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("删除后 Read = %v, want ErrNotFound", err)
	}
}
