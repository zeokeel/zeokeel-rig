package memory

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/internal/memwiki"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// 本文件实现模型可见的 `memory` 工具：一个**渐进式**的 wiki 记忆。
//
// 渐进披露是整个设计的重点：`index` 只回 "name — summary (多久以前)"，正文只能用
// `read` 按页取（并且有 8 KiB 上限）。20 页记忆的索引只有一两 KB，而全量正文可能
// 是几百 KB——模型先看目录、再决定读哪页，context 花在"真正需要的那一页"上。

const (
	// ToolName 是模型可见的工具名。
	ToolName = "memory"
	// DefaultMaxOutputBytes 限制单次工具响应的字节数。
	DefaultMaxOutputBytes = 32 << 10
	// DefaultMaxReadBytes 限制 `read` 返回的正文长度。一页超过它就截断并提示。
	DefaultMaxReadBytes = 8 << 10
)

// 类型别名：wiki 的数据模型定义在 internal/memwiki（存储实现与工具共用一套语义）。
type (
	// Page 是一页记忆。
	Page = memwiki.Page
	// PageMeta 是索引/搜索返回的元数据（不含正文）。
	PageMeta = memwiki.PageMeta
	// Wiki 是 wiki 记忆的存储接口。宿主可以自己实现（例如落到远程）。
	Wiki = memwiki.Wiki
)

// NewFileWiki 打开/创建一个目录作为 wiki 存储（0600/0700，跨进程持久）。
func NewFileWiki(dir string) (Wiki, error) { return memwiki.NewFileWiki(dir) }

// NewMemoryWiki 返回进程内的 wiki（测试与无盘环境）。
func NewMemoryWiki() Wiki { return memwiki.NewMemoryWiki() }

// NewWorkspaceWiki 返回"按工作区分区"的文件 wiki：目录是
// `<stateDir>/memory/<workspaceID>`（stateDir 为空时用用户级默认状态目录）。
//
// 记忆按工作区隔离：不同仓库互不可见；同一仓库经不同路径（符号链接、相对路径）打开
// 会落到同一份记忆。
func NewWorkspaceWiki(stateDir, workspaceRoot string) (Wiki, error) {
	dir, err := memwiki.WikiDir(stateDir, workspaceRoot)
	if err != nil {
		return nil, err
	}
	return memwiki.NewFileWiki(dir)
}

// WorkspaceID 返回工作区根的稳定标识（`<base>-<sha256(canonical)[:8]>`）。
func WorkspaceID(workspaceRoot string) (string, error) { return memwiki.WorkspaceID(workspaceRoot) }

// Option 配置 memory 工具。
type Option func(*config)

type config struct {
	maxOutput int
	maxRead   int
}

// WithMaxOutputBytes 覆盖单次响应上限（默认 32 KiB）。
func WithMaxOutputBytes(limit int) Option {
	return func(cfg *config) {
		if limit > 0 {
			cfg.maxOutput = limit
		}
	}
}

// WithMaxReadBytes 覆盖 `read` 的正文上限（默认 8 KiB）。
func WithMaxReadBytes(limit int) Option {
	return func(cfg *config) {
		if limit > 0 {
			cfg.maxRead = limit
		}
	}
}

// Tools 返回绑定到 wiki 的 memory 工具。wiki 为 nil 时使用进程内实现。
func Tools(wiki Wiki, opts ...Option) []rig.Tool {
	if wiki == nil {
		wiki = NewMemoryWiki()
	}
	cfg := config{maxOutput: DefaultMaxOutputBytes, maxRead: DefaultMaxReadBytes}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return []rig.Tool{&tool{wiki: wiki, cfg: cfg}}
}

// WikiFromStore 把宿主的扁平 rig.MemoryStore 投影成 wiki（兼容旧宿主）。
//
// 投影规则：一条记忆 = 一页；页名由记忆 ID 生成（非 slug 字符转成 '-'），摘要取
// 正文首行（截断到 200 字节），更新时间为 Created。写入方向不回写 store（MemoryStore
// 没有"覆盖"语义）——这只是一个只读迁移通道，宿主应该改用 Wiki。
func WikiFromStore(store rig.MemoryStore) Wiki {
	return &storeWiki{store: store}
}

// storeWiki 是 rig.MemoryStore → Wiki 的适配器。
type storeWiki struct{ store rig.MemoryStore }

var _ Wiki = (*storeWiki)(nil)

// Index 实现 Wiki。
func (w *storeWiki) Index(ctx context.Context) ([]PageMeta, error) {
	memories, err := w.store.List(ctx)
	if err != nil {
		return nil, err
	}
	metas := make([]PageMeta, 0, len(memories))
	for _, entry := range memories {
		metas = append(metas, metaFromMemory(entry))
	}
	sort.SliceStable(metas, func(i, j int) bool { return metas[i].Name < metas[j].Name })
	return metas, nil
}

// Read 实现 Wiki。
func (w *storeWiki) Read(ctx context.Context, name string) (Page, error) {
	memories, err := w.store.List(ctx)
	if err != nil {
		return Page{}, err
	}
	for _, entry := range memories {
		if metaFromMemory(entry).Name == name {
			return pageFromMemory(entry), nil
		}
	}
	return Page{}, fmt.Errorf("memory: no page named %q", name)
}

// Write 实现 Wiki：扁平 store 不支持覆盖，改为追加一条新记忆。
func (w *storeWiki) Write(ctx context.Context, page Page) error {
	if _, err := w.store.Add(ctx, rig.Memory{
		Text:    page.Content,
		Scope:   page.Name,
		Created: page.Updated,
	}); err != nil {
		return err
	}
	return nil
}

// Append 实现 Wiki：追加成一条新记忆（带日期前缀）。
func (w *storeWiki) Append(ctx context.Context, name, text string) error {
	if _, err := w.store.Add(ctx, rig.Memory{
		Text:  time.Now().UTC().Format("2006-01-02") + " " + text,
		Scope: name,
	}); err != nil {
		return err
	}
	return nil
}

// Delete 实现 Wiki。
func (w *storeWiki) Delete(ctx context.Context, name string) error {
	memories, err := w.store.List(ctx)
	if err != nil {
		return err
	}
	for _, entry := range memories {
		if metaFromMemory(entry).Name == name {
			if !w.store.Remove(ctx, entry.ID) {
				return fmt.Errorf("memory: could not remove %q", name)
			}
			return nil
		}
	}
	return fmt.Errorf("memory: no page named %q", name)
}

// Search 实现 Wiki。
func (w *storeWiki) Search(ctx context.Context, query string) ([]PageMeta, error) {
	memories, err := w.store.Search(ctx, query)
	if err != nil {
		return nil, err
	}
	metas := make([]PageMeta, 0, len(memories))
	for _, entry := range memories {
		metas = append(metas, metaFromMemory(entry))
	}
	if len(metas) > memwiki.MaxSearchResults {
		metas = metas[:memwiki.MaxSearchResults]
	}
	return metas, nil
}

// metaFromMemory 把一条扁平记忆投影成页面元数据。
func metaFromMemory(entry rig.Memory) PageMeta {
	name := slugify(entry.ID)
	if name == "" {
		name = "memory"
	}
	summary, _ := memwiki.ClampText(firstLine(entry.Text), memwiki.MaxSummaryBytes)
	if summary == "" {
		summary = "(no summary)"
	}
	return PageMeta{
		Name:    name,
		Title:   name,
		Summary: summary,
		Updated: entry.Created.UTC(),
		Bytes:   len(entry.Text),
	}
}

// pageFromMemory 把一条扁平记忆投影成完整页面。
func pageFromMemory(entry rig.Memory) Page {
	meta := metaFromMemory(entry)
	return Page{
		Name: meta.Name, Title: meta.Title, Summary: meta.Summary,
		Content: entry.Text, Updated: meta.Updated,
	}
}

// slugify 把任意字符串收敛成 [a-z0-9-]。
func slugify(text string) string {
	var builder strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(text)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			builder.WriteRune(r)
		default:
			builder.WriteByte('-')
		}
	}
	out := strings.Trim(builder.String(), "-")
	if len(out) > memwiki.MaxNameLength {
		out = out[:memwiki.MaxNameLength]
	}
	return out
}

// firstLine 返回第一行非空文本。
func firstLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// tool 是 `memory` 工具的实现。
type tool struct {
	wiki Wiki
	cfg  config
}

var _ rig.Tool = (*tool)(nil)

// Name 实现 rig.Tool。
func (t *tool) Name() string { return ToolName }

// Description 实现 rig.Tool。
func (t *tool) Description() string {
	// 常驻工具的描述是**每个回合都付的钱**：这里只给"一句话总述 + 一行 action
	// 列表"，细节（summary 必填/≤200 字节、read 的 8 KiB 截断、append 落在 ## Log）
	// 全部交给**运行时错误与输出**去教——模型用错的那一刻再教，比常驻描述里教便宜得多。
	return "Persistent per-workspace wiki memory; list the index first, then read one page.\n" +
		"Actions: index (list name + summary), read (one page body), write (create or replace), " +
		"append (add a dated line to the page's ## Log), search (find pages), delete.\n" +
		"Keep pages small and summaries precise; only the summary is visible in the index."
}

// Schema 实现 rig.Tool。
func (t *tool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"description": "index, read, write, append, search or delete.",
				"enum":        []string{"index", "read", "write", "append", "search", "delete"},
			},
			"name": map[string]any{
				"type":        "string",
				"description": "Page slug (a-z, 0-9, dashes).",
			},
			"title": map[string]any{
				"type":        "string",
				"description": "Human title; defaults to name.",
			},
			"summary": map[string]any{
				"type":        "string",
				"description": "One-line summary shown in the index; required.",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "Markdown body (replaces existing).",
			},
			"text": map[string]any{
				"type":        "string",
				"description": "Text appended to the page's ## Log.",
			},
			"query": map[string]any{
				"type":        "string",
				"description": "Search query (titles, summaries, bodies).",
			},
		},
		"required":             []string{"action"},
		"additionalProperties": false,
	}
}

// args 是解码后的 memory 工具参数对象。
type args struct {
	Action  string `json:"action"`
	Name    string `json:"name"`
	Title   string `json:"title"`
	Summary string `json:"summary"`
	Content string `json:"content"`
	Text    string `json:"text"`
	Query   string `json:"query"`
}

// Invoke 实现 rig.Tool。
func (t *tool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return rig.ToolResult{}, err
	}
	var a args
	if err := toolutil.DecodeArgs(call, &a); err != nil {
		return rig.ToolResult{}, err
	}
	switch strings.ToLower(strings.TrimSpace(a.Action)) {
	case "index":
		return t.index(ctx), nil
	case "read":
		return t.read(ctx, a), nil
	case "write":
		return t.write(ctx, a), nil
	case "append":
		return t.appendLog(ctx, a), nil
	case "search":
		return t.search(ctx, a), nil
	case "delete":
		return t.remove(ctx, a), nil
	case "":
		return toolutil.ErrorResultf("memory: action is required (index, read, write, append, search, delete)"), nil
	default:
		// 这里就是"用错的那一刻再教"：常驻描述里不再列全细节，改成错误里给全清单。
		return toolutil.ErrorResultf(`memory: unknown action %q; use one of index/read/search/write/append/delete`, a.Action), nil
	}
}

// index 实现 index 动作：只回元数据行，最多 MaxIndexResults 条。
func (t *tool) index(ctx context.Context) rig.ToolResult {
	metas, err := t.wiki.Index(ctx)
	if err != nil {
		return toolutil.ErrorResultf("memory: index: %v", err)
	}
	if len(metas) == 0 {
		return toolutil.TextResult("还没有记忆（no pages yet）。用 action: write 建第一页。")
	}
	total := len(metas)
	shown := metas
	if len(shown) > memwiki.MaxIndexResults {
		shown = shown[:memwiki.MaxIndexResults]
	}
	lines := make([]string, 0, len(shown)+1)
	for _, meta := range shown {
		lines = append(lines, formatMetaLine(meta))
	}
	if remaining := total - len(shown); remaining > 0 {
		lines = append(lines, fmt.Sprintf("… and %d more pages (use search to narrow)", remaining))
	}
	return toolutil.TextResult(t.limitOutput(strings.Join(lines, "\n"), len(shown)))
}

// read 实现 read 动作：正文上限 maxRead（默认 8 KiB）。
func (t *tool) read(ctx context.Context, a args) rig.ToolResult {
	name := strings.TrimSpace(a.Name)
	if name == "" {
		return toolutil.ErrorResultf("memory: name is required for action: read (use action: index to list pages)")
	}
	page, err := t.wiki.Read(ctx, name)
	if err != nil {
		return toolutil.ErrorResultf("memory: %s", describeWikiError(err, name))
	}
	body := page.Content
	if len(body) > t.cfg.maxRead {
		body, _ = memwiki.ClampText(body, t.cfg.maxRead)
		body += fmt.Sprintf("\n[truncated: page is %d bytes; use append or read a narrower page]", len(page.Content))
	}
	header := fmt.Sprintf("%s — %s (%s)\n\n", page.Name, page.Summary, humanAge(page.Updated))
	return toolutil.TextResult(t.limitOutput(header+body, 1))
}

// write 实现 write 动作：summary 必填。
func (t *tool) write(ctx context.Context, a args) rig.ToolResult {
	name := strings.TrimSpace(a.Name)
	if name == "" {
		return toolutil.ErrorResultf("memory: name is required for action: write (a slug like \"build-flaky-tests\")")
	}
	if strings.TrimSpace(a.Summary) == "" {
		return toolutil.ErrorResultf("memory: summary is required for action: write — the index only shows the summary, so every page needs a one-line summary")
	}
	page := Page{
		Name:    name,
		Title:   strings.TrimSpace(a.Title),
		Summary: a.Summary,
		Content: a.Content,
	}
	if err := t.wiki.Write(ctx, page); err != nil {
		return toolutil.ErrorResultf("memory: %s", describeWikiError(err, name))
	}
	return toolutil.TextResult(fmt.Sprintf("wrote page %s (%d bytes)", name, len(page.Content)))
}

// appendLog 实现 append 动作：追加到正文的 "## Log" 段。
func (t *tool) appendLog(ctx context.Context, a args) rig.ToolResult {
	name := strings.TrimSpace(a.Name)
	if name == "" {
		return toolutil.ErrorResultf("memory: name is required for action: append (use action: index to list pages)")
	}
	text := strings.TrimSpace(a.Text)
	if text == "" {
		return toolutil.ErrorResultf("memory: text is required for action: append")
	}
	if err := t.wiki.Append(ctx, name, text); err != nil {
		return toolutil.ErrorResultf("memory: %s", describeWikiError(err, name))
	}
	return toolutil.TextResult(fmt.Sprintf("appended to page %s", name))
}

// search 实现 search 动作：只回元数据行，最多 MaxSearchResults 条。
func (t *tool) search(ctx context.Context, a args) rig.ToolResult {
	query := strings.TrimSpace(a.Query)
	if query == "" {
		return toolutil.ErrorResultf("memory: query is required for action: search")
	}
	metas, err := t.wiki.Search(ctx, query)
	if err != nil {
		return toolutil.ErrorResultf("memory: search: %v", err)
	}
	if len(metas) == 0 {
		return toolutil.TextResult(fmt.Sprintf("没有匹配 %q 的页面（no pages matched; use action: index to see all pages）", query))
	}
	lines := make([]string, 0, len(metas))
	for _, meta := range metas {
		lines = append(lines, formatMetaLine(meta))
	}
	return toolutil.TextResult(t.limitOutput(strings.Join(lines, "\n"), len(metas)))
}

// remove 实现 delete 动作。
func (t *tool) remove(ctx context.Context, a args) rig.ToolResult {
	name := strings.TrimSpace(a.Name)
	if name == "" {
		return toolutil.ErrorResultf("memory: name is required for action: delete (use action: index to list pages)")
	}
	if err := t.wiki.Delete(ctx, name); err != nil {
		return toolutil.ErrorResultf("memory: %s", describeWikiError(err, name))
	}
	return toolutil.TextResult(fmt.Sprintf("deleted page %s", name))
}

// formatMetaLine 渲染一条元数据行：`name — summary (3天前)`。
func formatMetaLine(meta PageMeta) string {
	return fmt.Sprintf("%s — %s (%s)", meta.Name, meta.Summary, humanAge(meta.Updated))
}

// humanAge 渲染"多久以前"。
func humanAge(updated time.Time) string {
	if updated.IsZero() {
		return "未知时间"
	}
	delta := time.Since(updated.UTC())
	switch {
	case delta < time.Minute:
		return "刚刚"
	case delta < time.Hour:
		return fmt.Sprintf("%d分钟前", int(delta.Minutes()))
	case delta < 24*time.Hour:
		return fmt.Sprintf("%d小时前", int(delta.Hours()))
	default:
		return fmt.Sprintf("%d天前", int(delta.Hours()/24))
	}
}

// limitOutput 把输出限制在 maxOutput 字节内；**按整行截断**，不切断半行。
func (t *tool) limitOutput(text string, keptLines int) string {
	if len(text) <= t.cfg.maxOutput {
		return text
	}
	lines := strings.Split(text, "\n")
	var builder strings.Builder
	for _, line := range lines {
		if builder.Len()+len(line)+1 > t.cfg.maxOutput {
			break
		}
		builder.WriteString(line)
		builder.WriteString("\n")
	}
	truncated := strings.TrimSuffix(builder.String(), "\n")
	return truncated + fmt.Sprintf("\n[truncated: output exceeded %d bytes; use search to narrow]", t.cfg.maxOutput)
}

// describeWikiError 把存储层错误翻译成模型可读文案。
func describeWikiError(err error, name string) string {
	switch {
	case errors.Is(err, memwiki.ErrNotFound):
		return fmt.Sprintf("no page named %q (use action: index to see existing pages)", name)
	case errors.Is(err, memwiki.ErrInvalidName):
		return err.Error()
	case errors.Is(err, memwiki.ErrSummaryRequired), errors.Is(err, memwiki.ErrSummaryTooLong):
		return err.Error()
	default:
		return err.Error()
	}
}
