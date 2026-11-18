package memwiki

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// MemoryWiki 是进程内的 Wiki 实现（测试与无盘环境用）。并发安全。
type MemoryWiki struct {
	mu    sync.RWMutex
	pages map[string]Page
	now   func() time.Time
}

var _ Wiki = (*MemoryWiki)(nil)

// NewMemoryWiki 返回一个空的进程内 wiki。
func NewMemoryWiki() *MemoryWiki {
	return &MemoryWiki{pages: make(map[string]Page), now: func() time.Time { return time.Now().UTC() }}
}

// Index 实现 Wiki。
func (w *MemoryWiki) Index(ctx context.Context) ([]PageMeta, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	metas := make([]PageMeta, 0, len(w.pages))
	for _, page := range w.pages {
		metas = append(metas, metaOf(page))
	}
	sortMetas(metas)
	return metas, nil
}

// Read 实现 Wiki。
func (w *MemoryWiki) Read(ctx context.Context, name string) (Page, error) {
	if err := ctxErr(ctx); err != nil {
		return Page{}, err
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	page, ok := w.pages[name]
	if !ok {
		return Page{}, notFound(name)
	}
	return page, nil
}

// Write 实现 Wiki。
func (w *MemoryWiki) Write(ctx context.Context, page Page) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if err := ValidateName(page.Name); err != nil {
		return err
	}
	summary, err := validateSummary(page.Summary)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	previous, existed := w.pages[page.Name]
	content := page.Content
	if existed {
		content = prependRewriteLog(content, previous, page.Content)
	}
	w.pages[page.Name] = Page{
		Name:    page.Name,
		Title:   normalizeTitle(page.Name, page.Title),
		Summary: summary,
		Content: content,
		Updated: w.stamp(page.Updated),
	}
	return nil
}

// Append 实现 Wiki。
func (w *MemoryWiki) Append(ctx context.Context, name, text string) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if err := ValidateName(name); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	page, ok := w.pages[name]
	if !ok {
		return notFound(name)
	}
	page.Content = appendLogLine(page.Content, text)
	page.Updated = w.stamp(time.Time{})
	w.pages[name] = page
	return nil
}

// Delete 实现 Wiki。
func (w *MemoryWiki) Delete(ctx context.Context, name string) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.pages[name]; !ok {
		return notFound(name)
	}
	delete(w.pages, name)
	return nil
}

// Search 实现 Wiki：标题命中 > 摘要命中 > 正文命中，最多 MaxSearchResults 条。
func (w *MemoryWiki) Search(ctx context.Context, query string) ([]PageMeta, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	metas := make([]PageMeta, 0, len(w.pages))
	for _, page := range w.pages {
		metas = append(metas, metaOf(page))
	}
	return searchMetas(metas, func(name string) Page { return w.pages[name] }, query), nil
}

// Now 覆盖时钟（测试用）。
func (w *MemoryWiki) Now(now func() time.Time) { w.now = now }

// stamp 返回要记录的时间：零值取当前时间，统一 UTC。
func (w *MemoryWiki) stamp(given time.Time) time.Time {
	if given.IsZero() {
		return w.now().UTC()
	}
	return given.UTC()
}

// notFound 构造"页面不存在"错误（含现有页面提示由工具层补）。
func notFound(name string) error {
	return &NotFoundError{Name: name}
}

// NotFoundError 是"页面不存在"的具体错误。
type NotFoundError struct{ Name string }

func (e *NotFoundError) Error() string { return "memwiki: page not found: " + e.Name }
func (e *NotFoundError) Is(target error) bool {
	return target == ErrNotFound
}

// prependRewriteLog 处理覆盖写：在新的正文里追加一行"曾有多少字节"的审计记录。
func prependRewriteLog(newContent string, previous Page, _ string) string {
	return appendLogLine(newContent, rewriteLogText(len(previous.Content)))
}

// searchMetas 在 meta + 正文上做大小写不敏感的子串搜索，按命中位置排序。
//
// 参数 readPage 用于取正文（Search 只返回 meta，但排序需要知道是否命中正文）。
func searchMetas(metas []PageMeta, readPage func(name string) Page, query string) []PageMeta {
	needle := strings.ToLower(strings.TrimSpace(query))
	if needle == "" {
		return nil
	}
	type scored struct {
		meta  PageMeta
		score int
	}
	matches := make([]scored, 0, len(metas))
	for _, meta := range metas {
		score := 0
		switch {
		case strings.Contains(strings.ToLower(meta.Title), needle):
			score = 0
		case strings.Contains(strings.ToLower(meta.Summary), needle):
			score = 1
		default:
			page := readPage(meta.Name)
			if !strings.Contains(strings.ToLower(page.Content), needle) {
				continue
			}
			score = 2
		}
		matches = append(matches, scored{meta: meta, score: score})
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score < matches[j].score
		}
		if !matches[i].meta.Updated.Equal(matches[j].meta.Updated) {
			return matches[i].meta.Updated.After(matches[j].meta.Updated)
		}
		return matches[i].meta.Name < matches[j].meta.Name
	})
	if len(matches) > MaxSearchResults {
		matches = matches[:MaxSearchResults]
	}
	out := make([]PageMeta, 0, len(matches))
	for _, match := range matches {
		out = append(out, match.meta)
	}
	return out
}

// ctxErr 把 ctx 取消转换成错误（wiki 操作可能很快，但仍要尊重取消）。
func ctxErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}
