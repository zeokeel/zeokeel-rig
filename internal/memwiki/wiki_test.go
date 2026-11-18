package memwiki_test

// memwiki 存储层的用例：front-matter 往返、slug 校验、覆盖/追加的 Log 语义、
// index.json 缓存与重建、按工作区分区、并发与权限。

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/internal/memwiki"
)

func newFileWiki(t *testing.T) *memwiki.FileWiki {
	t.Helper()
	wiki, err := memwiki.NewFileWiki(filepath.Join(t.TempDir(), "wiki"))
	if err != nil {
		t.Fatalf("NewFileWiki: %v", err)
	}
	return wiki
}

// TestFileWikiFrontMatterRoundTrip 断言 title/summary/content/updated 完整往返。
func TestFileWikiFrontMatterRoundTrip(t *testing.T) {
	wiki := newFileWiki(t)
	stamp := time.Date(2026, 11, 18, 8, 30, 0, 0, time.UTC)
	page := memwiki.Page{
		Name:    "build-flaky-tests",
		Title:   "Build: flaky tests",
		Summary: "记录偶发失败的测试与重试策略",
		Content: "# 结论\n\n`go test ./kit/` 偶发超时，重试即可。",
		Updated: stamp,
	}
	if err := wiki.Write(context.Background(), page); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := wiki.Read(context.Background(), page.Name)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Title != page.Title || got.Summary != page.Summary {
		t.Fatalf("front-matter 往返不一致: %+v", got)
	}
	if !strings.Contains(got.Content, "偶发超时") {
		t.Fatalf("正文丢了: %q", got.Content)
	}
	if !got.Updated.Equal(stamp) {
		t.Fatalf("Updated = %v, want %v", got.Updated, stamp)
	}
	// 磁盘上必须是 front-matter + Markdown。
	raw, err := os.ReadFile(filepath.Join(wiki.Dir(), "pages", "build-flaky-tests.md"))
	if err != nil {
		t.Fatalf("read raw: %v", err)
	}
	if !strings.HasPrefix(string(raw), "---\n") || !strings.Contains(string(raw), "summary: ") {
		t.Fatalf("磁盘格式不是 front-matter:\n%s", raw)
	}
}

// TestValidateName 断言 slug 规则（不做静默改写）。
func TestValidateName(t *testing.T) {
	valid := []string{"a", "build-flaky-tests", "v2-notes", "123"}
	for _, name := range valid {
		if err := memwiki.ValidateName(name); err != nil {
			t.Fatalf("%q 应当合法: %v", name, err)
		}
	}
	// 注意：`-a` / `a-` 是 spec 允许的（[a-z0-9-]），不额外限制。
	invalid := []string{"", "Build", "build notes", "../escape", "a/b", "中文", strings.Repeat("x", 65), "a.b"}
	for _, name := range invalid {
		if err := memwiki.ValidateName(name); err == nil {
			t.Fatalf("%q 应当被拒绝", name)
		} else if !errors.Is(err, memwiki.ErrInvalidName) {
			t.Fatalf("%q 的错误应当可 errors.Is(ErrInvalidName): %v", name, err)
		}
	}
}

// TestWriteRequiresSummary 断言 summary 必填、超长报错、空白被折叠。
func TestWriteRequiresSummary(t *testing.T) {
	ctx := context.Background()
	wiki := newFileWiki(t)

	err := wiki.Write(ctx, memwiki.Page{Name: "a", Content: "x"})
	if !errors.Is(err, memwiki.ErrSummaryRequired) {
		t.Fatalf("缺摘要应当 ErrSummaryRequired: %v", err)
	}
	err = wiki.Write(ctx, memwiki.Page{Name: "a", Summary: "  \n\t ", Content: "x"})
	if !errors.Is(err, memwiki.ErrSummaryRequired) {
		t.Fatalf("全空白摘要应当 ErrSummaryRequired: %v", err)
	}
	err = wiki.Write(ctx, memwiki.Page{Name: "a", Summary: strings.Repeat("长", 100), Content: "x"})
	if !errors.Is(err, memwiki.ErrSummaryTooLong) {
		t.Fatalf("超长摘要应当 ErrSummaryTooLong: %v", err)
	}
	if err := wiki.Write(ctx, memwiki.Page{Name: "a", Summary: "多行\n摘要  带空白", Content: "x"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	page, err := wiki.Read(ctx, "a")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if page.Summary != "多行 摘要 带空白" {
		t.Fatalf("摘要没有被折叠成一行: %q", page.Summary)
	}
}

// TestOverwriteAppendsRewriteLog 断言覆盖写会在 Log 段留下"曾有多少字节"。
func TestOverwriteAppendsRewriteLog(t *testing.T) {
	ctx := context.Background()
	wiki := newFileWiki(t)
	oldBody := strings.Repeat("旧内容", 10)
	if err := wiki.Write(ctx, memwiki.Page{Name: "p", Summary: "第一版", Content: oldBody}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := wiki.Write(ctx, memwiki.Page{Name: "p", Summary: "第二版", Content: "新内容"}); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	page, err := wiki.Read(ctx, "p")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if page.Summary != "第二版" {
		t.Fatalf("摘要没有更新: %q", page.Summary)
	}
	if !strings.Contains(page.Content, "## Log") {
		t.Fatalf("覆盖后缺少 Log 段:\n%s", page.Content)
	}
	if !strings.Contains(page.Content, "rewritten (was") {
		t.Fatalf("覆盖后缺少 rewritten 审计行:\n%s", page.Content)
	}
	if want := len(oldBody); !strings.Contains(page.Content, itoaTest(want)) {
		t.Fatalf("rewritten 行里没有旧字节数 %d:\n%s", want, page.Content)
	}
}

// TestAppendCreatesAndExtendsLog 断言 append 建 Log 段并带日期前缀。
func TestAppendCreatesAndExtendsLog(t *testing.T) {
	ctx := context.Background()
	wiki := newFileWiki(t)
	if err := wiki.Write(ctx, memwiki.Page{Name: "p", Summary: "s", Content: "正文"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := wiki.Append(ctx, "p", "第一次观察：偶发失败"); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := wiki.Append(ctx, "p", "第二次观察：\n只在高并发下出现"); err != nil {
		t.Fatalf("Append: %v", err)
	}
	page, err := wiki.Read(ctx, "p")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if strings.Count(page.Content, "## Log") != 1 {
		t.Fatalf("Log 段应当只有一个:\n%s", page.Content)
	}
	today := time.Now().UTC().Format("2006-01-02")
	if !strings.Contains(page.Content, "- "+today+" 第一次观察") {
		t.Fatalf("append 没有日期前缀:\n%s", page.Content)
	}
	if !strings.Contains(page.Content, "只在高并发下出现") {
		t.Fatalf("多行 append 丢了续行:\n%s", page.Content)
	}
	// 正文仍在 Log 之前。
	if strings.Index(page.Content, "正文") > strings.Index(page.Content, "## Log") {
		t.Fatalf("追加破坏了正文与 Log 的顺序:\n%s", page.Content)
	}
}

// TestAppendAndDeleteUnknownPage 断言未知页面返回 ErrNotFound。
func TestAppendAndDeleteUnknownPage(t *testing.T) {
	ctx := context.Background()
	wiki := newFileWiki(t)
	if err := wiki.Append(ctx, "nope", "x"); !errors.Is(err, memwiki.ErrNotFound) {
		t.Fatalf("Append 未知页面应当 ErrNotFound: %v", err)
	}
	if err := wiki.Delete(ctx, "nope"); !errors.Is(err, memwiki.ErrNotFound) {
		t.Fatalf("Delete 未知页面应当 ErrNotFound: %v", err)
	}
	if _, err := wiki.Read(ctx, "nope"); !errors.Is(err, memwiki.ErrNotFound) {
		t.Fatalf("Read 未知页面应当 ErrNotFound: %v", err)
	}
}

// TestIndexSortedAndBytes 断言 index 按最近修改降序、Bytes 是正文字节数。
func TestIndexSortedAndBytes(t *testing.T) {
	ctx := context.Background()
	wiki := newFileWiki(t)
	if err := wiki.Write(ctx, memwiki.Page{Name: "old", Summary: "旧", Content: "x", Updated: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	if err := wiki.Write(ctx, memwiki.Page{Name: "new", Summary: "新", Content: "yyyy", Updated: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	metas, err := wiki.Index(ctx)
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if len(metas) != 2 || metas[0].Name != "new" || metas[1].Name != "old" {
		t.Fatalf("排序不对: %+v", metas)
	}
	if metas[0].Bytes != 4 {
		t.Fatalf("Bytes = %d, want 4", metas[0].Bytes)
	}
	// 索引不含正文。
	for _, meta := range metas {
		if strings.Contains(meta.Summary, "yyyy") {
			t.Fatalf("索引里出现了正文: %+v", meta)
		}
	}
}

// TestIndexCacheIsCreatedAndRebuilt 断言 index.json 会生成；损坏后自动重建。
func TestIndexCacheIsCreatedAndRebuilt(t *testing.T) {
	ctx := context.Background()
	wiki := newFileWiki(t)
	if err := wiki.Write(ctx, memwiki.Page{Name: "a", Summary: "sum", Content: "body"}); err != nil {
		t.Fatal(err)
	}
	if _, err := wiki.Index(ctx); err != nil {
		t.Fatalf("Index: %v", err)
	}
	cachePath := filepath.Join(wiki.Dir(), "index.json")
	raw, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("index.json 没有生成: %v", err)
	}
	var cache map[string]any
	if err := json.Unmarshal(raw, &cache); err != nil {
		t.Fatalf("index.json 不是合法 JSON: %v", err)
	}
	// 破坏缓存：index 必须仍然正确（自动重建）。
	if err := os.WriteFile(cachePath, []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	metas, err := wiki.Index(ctx)
	if err != nil {
		t.Fatalf("损坏缓存后 Index 失败: %v", err)
	}
	if len(metas) != 1 || metas[0].Name != "a" {
		t.Fatalf("重建后的索引不对: %+v", metas)
	}
	// 版本不符同样重建。
	if err := os.WriteFile(cachePath, []byte(`{"version":99,"pages":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if metas, err = wiki.Index(ctx); err != nil || len(metas) != 1 {
		t.Fatalf("版本不符后应当重建: %v %+v", err, metas)
	}
}

// TestIndexCacheInvalidatedByExternalEdit 断言外部改文件后索引会更新。
func TestIndexCacheInvalidatedByExternalEdit(t *testing.T) {
	ctx := context.Background()
	wiki := newFileWiki(t)
	if err := wiki.Write(ctx, memwiki.Page{Name: "a", Summary: "旧摘要", Content: "body"}); err != nil {
		t.Fatal(err)
	}
	if _, err := wiki.Index(ctx); err != nil {
		t.Fatal(err)
	}
	// 直接改盘上文件（模拟用户用编辑器改）。
	path := filepath.Join(wiki.Dir(), "pages", "a.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// 换成长度不同的摘要：同一字节长度的改写在 mtime 精度较粗的文件系统上可能被
	// 缓存判定挡住（这是 index.json 的已知取舍，见 memo）。
	updated := strings.Replace(string(raw), "summary: 旧摘要", "summary: 新摘要（更长）", 1)
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
	metas, err := wiki.Index(ctx)
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if len(metas) != 1 || metas[0].Summary != "新摘要（更长）" {
		t.Fatalf("外部改动没有被索引看到: %+v", metas)
	}
}

// TestSearchOrderingAndLimit 断言"标题命中 > 摘要命中 > 正文命中"且最多 20 条。
func TestSearchOrderingAndLimit(t *testing.T) {
	ctx := context.Background()
	wiki := memwiki.NewMemoryWiki()
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	mustWrite := func(page memwiki.Page) {
		t.Helper()
		if err := wiki.Write(ctx, page); err != nil {
			t.Fatalf("Write %s: %v", page.Name, err)
		}
	}
	mustWrite(memwiki.Page{Name: "body-hit", Title: "无关", Summary: "无关的摘要", Content: "这里提到 flaky 一次", Updated: base.Add(3 * time.Hour)})
	mustWrite(memwiki.Page{Name: "summary-hit", Title: "也无关", Summary: "关于 flaky 的摘要", Content: "正文", Updated: base.Add(2 * time.Hour)})
	mustWrite(memwiki.Page{Name: "title-hit", Title: "Flaky tests", Summary: "无关", Content: "正文", Updated: base.Add(time.Hour)})
	for index := 0; index < 25; index++ {
		mustWrite(memwiki.Page{
			Name:    "filler-" + itoaTest(index),
			Title:   "filler",
			Summary: "大量正文命中的页面",
			Content: "flaky 出现在正文里",
			Updated: base,
		})
	}
	metas, err := wiki.Search(ctx, "flaky")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(metas) > memwiki.MaxSearchResults {
		t.Fatalf("返回 %d 条，超过上限 %d", len(metas), memwiki.MaxSearchResults)
	}
	if len(metas) == 0 || metas[0].Name != "title-hit" {
		t.Fatalf("标题命中应当排第一: %+v", metas)
	}
	if metas[1].Name != "summary-hit" {
		t.Fatalf("摘要命中应当排第二: %+v", metas)
	}
	if metas[2].Name != "body-hit" {
		t.Fatalf("正文命中应当排第三: %+v", metas)
	}
	// 空查询返回空。
	if metas, err := wiki.Search(ctx, "   "); err != nil || len(metas) != 0 {
		t.Fatalf("空查询: %v %+v", err, metas)
	}
}

// TestWorkspaceIDNormalizesPaths 断言同一仓库经不同路径得到同一 ID。
func TestWorkspaceIDNormalizesPaths(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "sub")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	variants := []string{
		nested,
		nested + string(filepath.Separator),
		filepath.Join(root, "sub", "..", "sub"),
	}
	first, err := memwiki.WorkspaceID(variants[0])
	if err != nil {
		t.Fatalf("WorkspaceID: %v", err)
	}
	for _, variant := range variants[1:] {
		id, err := memwiki.WorkspaceID(variant)
		if err != nil {
			t.Fatalf("WorkspaceID(%q): %v", variant, err)
		}
		if id != first {
			t.Fatalf("%q 的 ID = %q, want %q（同一仓库必须同一份记忆）", variant, id, first)
		}
	}
	// 符号链接（Windows 可能没权限，跳过）。
	link := filepath.Join(root, "link")
	if err := os.Symlink(nested, link); err == nil {
		linked, err := memwiki.WorkspaceID(link)
		if err != nil {
			t.Fatalf("WorkspaceID(link): %v", err)
		}
		if linked != first {
			t.Fatalf("符号链接路径的 ID = %q, want %q", linked, first)
		}
	}
	// 不同工作区 → 不同 ID。
	other := t.TempDir()
	otherID, err := memwiki.WorkspaceID(other)
	if err != nil {
		t.Fatal(err)
	}
	if otherID == first {
		t.Fatalf("不同工作区得到了相同 ID: %q", first)
	}
	if !strings.HasPrefix(first, "sub-") || len(first) != len("sub-")+8 {
		t.Fatalf("ID 形状不对: %q", first)
	}
}

// TestWorkspacesAreIsolated 断言不同工作区互不可见（关键性质）。
func TestWorkspacesAreIsolated(t *testing.T) {
	ctx := context.Background()
	stateDir := t.TempDir()
	rootA := filepath.Join(t.TempDir(), "alpha")
	rootB := filepath.Join(t.TempDir(), "beta")
	for _, dir := range []string{rootA, rootB} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	wikiA, err := memwiki.NewFileWiki(mustWikiDir(t, stateDir, rootA))
	if err != nil {
		t.Fatal(err)
	}
	wikiB, err := memwiki.NewFileWiki(mustWikiDir(t, stateDir, rootB))
	if err != nil {
		t.Fatal(err)
	}
	if err := wikiA.Write(ctx, memwiki.Page{Name: "only-a", Summary: "仅属于 A", Content: "A 的正文"}); err != nil {
		t.Fatal(err)
	}
	metasB, err := wikiB.Index(ctx)
	if err != nil {
		t.Fatalf("B Index: %v", err)
	}
	if len(metasB) != 0 {
		t.Fatalf("B 看到了 A 的记忆: %+v", metasB)
	}
	if _, err := wikiB.Read(ctx, "only-a"); !errors.Is(err, memwiki.ErrNotFound) {
		t.Fatalf("B 能读到 A 的页面: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wikiA.Dir(), "pages", "only-a.md")); err != nil {
		t.Fatalf("A 的页面没落盘: %v", err)
	}
}

// TestConcurrentWrites 并发写 20 个不同页面（-race 也跑）。
func TestConcurrentWrites(t *testing.T) {
	ctx := context.Background()
	wiki := newFileWiki(t)
	const workers = 20
	var wait sync.WaitGroup
	for index := 0; index < workers; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			name := "page-" + itoaTest(index)
			if err := wiki.Write(ctx, memwiki.Page{Name: name, Summary: "并发写 " + name, Content: "body " + name}); err != nil {
				t.Errorf("Write %s: %v", name, err)
			}
		}(index)
	}
	wait.Wait()
	metas, err := wiki.Index(ctx)
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if len(metas) != workers {
		t.Fatalf("页面数 = %d, want %d", len(metas), workers)
	}
	for index := 0; index < workers; index++ {
		name := "page-" + itoaTest(index)
		if _, err := wiki.Read(ctx, name); err != nil {
			t.Fatalf("Read %s: %v", name, err)
		}
	}
}

// TestFilePermissionsArePrivate 断言目录 0700、文件 0600。
func TestFilePermissionsArePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 没有 POSIX 权限位")
	}
	wiki := newFileWiki(t)
	if err := wiki.Write(context.Background(), memwiki.Page{Name: "p", Summary: "s", Content: "c"}); err != nil {
		t.Fatal(err)
	}
	dirInfo, err := os.Stat(filepath.Join(wiki.Dir(), "pages"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("pages 目录权限 = %o, want 700", perm)
	}
	fileInfo, err := os.Stat(filepath.Join(wiki.Dir(), "pages", "p.md"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fileInfo.Mode().Perm(); perm != 0o600 {
		t.Fatalf("页面文件权限 = %o, want 600", perm)
	}
}

// TestMalformedFrontMatterStillReadable 断言坏 front-matter 的页面仍可读/可删。
func TestMalformedFrontMatterStillReadable(t *testing.T) {
	ctx := context.Background()
	wiki := newFileWiki(t)
	path := filepath.Join(wiki.Dir(), "pages", "broken.md")
	if err := os.WriteFile(path, []byte("没有 front-matter 的正文\n第二行"), 0o600); err != nil {
		t.Fatal(err)
	}
	page, err := wiki.Read(ctx, "broken")
	if err != nil {
		t.Fatalf("坏页面应当仍可读: %v", err)
	}
	if page.Title != "broken" || !strings.Contains(page.Content, "第二行") {
		t.Fatalf("坏页面的回退不对: %+v", page)
	}
	metas, err := wiki.Index(ctx)
	if err != nil || len(metas) != 1 {
		t.Fatalf("坏页面应当出现在索引里: %v %+v", err, metas)
	}
	if err := wiki.Delete(ctx, "broken"); err != nil {
		t.Fatalf("坏页面应当可删除: %v", err)
	}
}

// TestMemoryWikiParity 断言进程内实现与文件实现语义一致（基本流程）。
func TestMemoryWikiParity(t *testing.T) {
	ctx := context.Background()
	implementations := map[string]memwiki.Wiki{
		"memory": memwiki.NewMemoryWiki(),
		"file":   newFileWiki(t),
	}
	for label, wiki := range implementations {
		if err := wiki.Write(ctx, memwiki.Page{Name: "p", Summary: "s1", Content: "body1"}); err != nil {
			t.Fatalf("%s Write: %v", label, err)
		}
		if err := wiki.Append(ctx, "p", "log line"); err != nil {
			t.Fatalf("%s Append: %v", label, err)
		}
		page, err := wiki.Read(ctx, "p")
		if err != nil {
			t.Fatalf("%s Read: %v", label, err)
		}
		if !strings.Contains(page.Content, "## Log") || !strings.Contains(page.Content, "log line") {
			t.Fatalf("%s Log 语义不一致:\n%s", label, page.Content)
		}
		if err := wiki.Write(ctx, memwiki.Page{Name: "p", Summary: "s2", Content: "body2"}); err != nil {
			t.Fatalf("%s rewrite: %v", label, err)
		}
		page, _ = wiki.Read(ctx, "p")
		if !strings.Contains(page.Content, "rewritten (was") {
			t.Fatalf("%s 覆盖语义不一致:\n%s", label, page.Content)
		}
		if err := wiki.Delete(ctx, "p"); err != nil {
			t.Fatalf("%s Delete: %v", label, err)
		}
		if metas, err := wiki.Index(ctx); err != nil || len(metas) != 0 {
			t.Fatalf("%s 删除后索引应为空: %v %+v", label, err, metas)
		}
	}
}

// mustWikiDir 计算某工作区的 wiki 目录。
func mustWikiDir(t *testing.T, stateDir, root string) string {
	t.Helper()
	dir, err := memwiki.WikiDir(stateDir, root)
	if err != nil {
		t.Fatalf("WikiDir: %v", err)
	}
	return dir
}

// itoaTest 是测试内的小工具（避免每处都 strconv）。
func itoaTest(value int) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}
