package shelf

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// 本包把"渐进式加载"抽成一个可复用的工具原语。三个动作固定：
//
//	list           目录（只给元数据，便宜）
//	search query   在目录/正文里找，仍然只回目录行
//	load name      才返回正文（有单条上限，UTF-8 安全）
//
// 形态来自仓库里已有的两个消费者（`skill`：list/search/load；`memory`：
// index/search/read），共同点是**目录先行、正文按需、输出有界**。宿主只要实现
// `Source`（它自己的"条目从哪来"），就得到一个行为一致的渐进式工具。

// 默认上限。Options 里同名字段的零值表示"用默认"，负数表示"不限制"。
const (
	// DefaultMaxListed 是 list 最多显示多少条。
	DefaultMaxListed = 40
	// DefaultMaxSearch 是 search 最多返回多少条。
	DefaultMaxSearch = 20
	// DefaultMaxOutputBytes 是单次响应的默认上限（64 KiB）。这是**安全网**而不是
	// "单条上限"：完整正文由 agent 的 spill/截断边界负责，本库不叠第二层限制。
	DefaultMaxOutputBytes = 64 << 10
	// MaxSummaryBytes 是一句话摘要的上限（UTF-8 字节）。目录里显示的就是它。
	MaxSummaryBytes = 200
)

// 动作名（线格式契约的一部分）。
const (
	ActionList   = "list"
	ActionSearch = "search"
	ActionLoad   = "load"
)

// ErrUnknownItem 让 Source 表达"没有这个条目"。实现 Load 时返回它，工具会渲染成
// 可读的"用 list 查看现有条目"文案，而不是把内部错误原样抛给模型。
var ErrUnknownItem = errors.New("shelf: unknown item")

// Item 是一个可被渐进加载的条目。
type Item struct {
	// Name 是稳定标识（slug）；模型用它 load。
	Name string
	// Title 是人读标题；空时用 Name。
	Title string
	// Summary 是一句话摘要——目录里显示的就是它（必填，≤200 字节）。
	Summary string
	// Bytes 是正文大小（可选；>0 时目录行里会显示 "N.N KB"）。
	Bytes int
}

// Source 是宿主提供的内容来源。
//
// 契约：**只有 Load 会返回正文**。List 与 Search 必须便宜（不要为了排序去读所有
// 正文），因为它们服务于"我先看看有什么"这一步。
type Source interface {
	// List 返回全部条目（顺序由 Source 决定，工具不会重排）。
	List(ctx context.Context) ([]Item, error)
	// Search 返回匹配条目；**Source 负责排序**（工具不再重排，以免覆盖宿主的权重）。
	Search(ctx context.Context, query string) ([]Item, error)
	// Load 返回条目正文；不存在时返回 ErrUnknownItem。
	Load(ctx context.Context, name string) (string, error)
}

// TruncateMode 选择"输出超上限"时的截断方式。
type TruncateMode int

const (
	// TruncateLines（默认）按**整行**截断：不会切出半行，也不会切坏 UTF-8。
	TruncateLines TruncateMode = iota
	// TruncateBytes 是给既有工具留的兼容模式：按字节切（UTF-8 安全）并追加
	// `…\n[truncated: output exceeded N bytes]\n`，与 v0.20 之前的 `skill` 一致。
	TruncateBytes
)

// Texts 是模型可见文案的覆盖点。空字段使用默认值。
type Texts struct {
	// EmptyList 是"一条都没有"时的文案。
	EmptyList string
	// NoMatch 是"搜索无结果"的文案，必须包含一个 %q（查询）。
	NoMatch string
	// Unknown 是"没有这个条目"的文案，必须包含一个 %q（名字）。
	Unknown string
}

// 默认文案（模型可见）。刻意用英文短句 + 指向下一步动作。
const (
	defaultEmptyList = "no items yet"
	defaultNoMatch   = "no items matched %q"
	defaultUnknown   = `no item named %q (use action: list to see what is available)`
)

// Options 配置渐进式工具。零值 = 全部默认。
type Options struct {
	// ToolName 是模型可见的工具名（必填，`New` 会校验非空与非重名风险）。
	ToolName string
	// Description 是模型可见描述；空时给一句通用英文说明。
	Description string
	// Schema 整体覆盖默认 schema（给既有工具保持逐字节兼容用）。nil 用默认。
	Schema map[string]any
	// MaxListed 是 list 的条数上限；0 = 默认 40，负数 = 不限制。
	MaxListed int
	// MaxSearch 是 search 的条数上限；0 = 默认 20，负数 = 不限制。
	MaxSearch int
	// MaxOutputBytes 是单次输出上限；0 = 默认 32 KiB。
	MaxOutputBytes int
	// FormatItem 覆盖目录行渲染；nil = `- name — summary (N.N KB)`。
	FormatItem func(Item) string
	// TrailingNewline 为真时目录输出以换行结尾（既有工具的兼容开关）。
	TrailingNewline bool
	// Truncate 选择截断模式（默认按整行）。
	Truncate TruncateMode
	// Texts 覆盖默认文案。
	Texts Texts
	// AllowEmptySummary 允许条目没有摘要。
	//
	// 默认 false（fail closed）：目录里显示的就是摘要，没有摘要的条目对模型毫无信息
	// 量。既有工具（`skill`）允许没有 description 的技能，由 FormatItem 自行处理空
	// 摘要，因此它显式打开这个开关。
	AllowEmptySummary bool
}

// tool 是渐进式工具的 rig.Tool 实现。
type tool struct {
	source            Source
	name              string
	desc              string
	schema            map[string]any
	listed            int
	search            int
	maxOut            int
	format            func(Item) string
	allowEmptySummary bool
	trail             bool
	mode              TruncateMode
	texts             Texts
}

var _ rig.Tool = (*tool)(nil)

// New 构造一个渐进式工具。参数不合法时返回错误（fail closed）——宁可让宿主在装配
// 期就发现，也不要让模型在运行期收到一条行为奇怪的工具。
func New(source Source, opts Options) (rig.Tool, error) {
	if source == nil {
		return nil, errors.New("shelf: source is nil")
	}
	name := strings.TrimSpace(opts.ToolName)
	if name == "" {
		return nil, errors.New("shelf: ToolName is required (a model-visible tool name cannot be empty)")
	}
	listed := opts.MaxListed
	switch {
	case listed == 0:
		listed = DefaultMaxListed
	case listed < 0:
		listed = 0 // 不限制
	}
	search := opts.MaxSearch
	switch {
	case search == 0:
		search = DefaultMaxSearch
	case search < 0:
		search = 0
	}
	maxOut := opts.MaxOutputBytes
	if maxOut <= 0 {
		maxOut = DefaultMaxOutputBytes
	}
	desc := strings.TrimSpace(opts.Description)
	if desc == "" {
		desc = defaultDescription(name)
	}
	schema := opts.Schema
	if schema == nil {
		schema = defaultSchema()
	}
	format := opts.FormatItem
	if format == nil {
		format = defaultItemLine
	}
	texts := opts.Texts
	if strings.TrimSpace(texts.EmptyList) == "" {
		texts.EmptyList = defaultEmptyList
	}
	if strings.TrimSpace(texts.NoMatch) == "" {
		texts.NoMatch = defaultNoMatch
	}
	if strings.TrimSpace(texts.Unknown) == "" {
		texts.Unknown = defaultUnknown
	}
	return &tool{
		source: source, name: name, desc: desc, schema: schema,
		listed: listed, search: search, maxOut: maxOut,
		format: format, trail: opts.TrailingNewline, mode: opts.Truncate, texts: texts,
		allowEmptySummary: opts.AllowEmptySummary,
	}, nil
}

// defaultDescription 是通用描述：讲清"先 list 再 load"这个用法，因为模型对这个
// 工具名是第一次见。
func defaultDescription(name string) string {
	return fmt.Sprintf("`%s` browses a catalog of items and loads one at a time. "+
		"action=%s returns the catalog (names and summaries only — bodies are never included), "+
		"action=%s finds items by query and still returns only catalog lines, and "+
		"action=%s returns one item's full body. Start with %s, then load only what you need.",
		name, ActionList, ActionSearch, ActionLoad, ActionList)
}

// defaultSchema 是默认的模型可见 schema。
func defaultSchema() map[string]any {
	return toolutil.ObjectSchema(map[string]any{
		"action": map[string]any{
			"type":        "string",
			"description": "list the catalog, search it, or load one item by name.",
			"enum":        []string{ActionList, ActionSearch, ActionLoad},
		},
		"query": toolutil.StringProp("Search query for action=search (matched by the source, case-insensitively)."),
		"name":  toolutil.StringProp("Item name to load (as returned by action=list)."),
	}, "action")
}

// name 实现 rig.Tool。
func (t *tool) Name() string { return t.name }

// Description 实现 rig.Tool。
func (t *tool) Description() string { return t.desc }

// Schema 实现 rig.Tool。
func (t *tool) Schema() map[string]any { return t.schema }

// args 是解码后的参数。
type args struct {
	Action string `json:"action"`
	Name   string `json:"name"`
	Query  string `json:"query"`
}

// Invoke 实现 rig.Tool。
func (t *tool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	var a args
	if err := toolutil.DecodeArgs(call, &a); err != nil {
		return rig.ToolResult{}, err
	}
	action := strings.ToLower(strings.TrimSpace(a.Action))
	if action == "" {
		// 省略 action 时：给了 name 就当作 load，否则 list。这样"只传 name"的
		// 调用形态（DSH 兼容）不需要宿主自己再解析一遍。
		if strings.TrimSpace(a.Name) != "" {
			action = ActionLoad
		} else {
			action = ActionList
		}
	}
	switch action {
	case ActionList, ActionSearch, ActionLoad:
	default:
		return toolutil.ErrorResultf("%s: unknown action %q (want list, search or load)", t.name, a.Action), nil
	}
	if err := ctx.Err(); err != nil {
		return rig.ToolResult{}, err
	}
	switch action {
	case ActionList:
		return t.list(ctx), nil
	case ActionSearch:
		return t.searchAction(ctx, a), nil
	default:
		return t.load(ctx, a), nil
	}
}

// list 实现 list 动作。
func (t *tool) list(ctx context.Context) rig.ToolResult {
	items, err := t.source.List(ctx)
	if err != nil {
		return toolutil.ErrorResultf("%s: %v", t.name, err)
	}
	if err := t.validateItems(items); err != nil {
		return toolutil.ErrorResultf("%s: %v", t.name, err)
	}
	if len(items) == 0 {
		return toolutil.TextResult(t.texts.EmptyList)
	}
	return toolutil.TextResult(t.renderCatalog(items, t.listed))
}

// searchAction 实现 search 动作：只回目录行。
func (t *tool) searchAction(ctx context.Context, a args) rig.ToolResult {
	query := strings.TrimSpace(a.Query)
	if query == "" {
		return toolutil.ErrorResultf("%s: query is required for action=search", t.name)
	}
	items, err := t.source.Search(ctx, query)
	if err != nil {
		return toolutil.ErrorResultf("%s: %v", t.name, err)
	}
	if err := t.validateItems(items); err != nil {
		return toolutil.ErrorResultf("%s: %v", t.name, err)
	}
	if len(items) == 0 {
		return toolutil.TextResult(fmt.Sprintf(t.texts.NoMatch, query))
	}
	return toolutil.TextResult(t.renderCatalog(items, t.search))
}

// load 实现 load 动作：只有这里会读正文。
func (t *tool) load(ctx context.Context, a args) rig.ToolResult {
	name := strings.TrimSpace(a.Name)
	if name == "" {
		return toolutil.ErrorResultf("%s: name is required for action=load", t.name)
	}
	body, err := t.source.Load(ctx, name)
	if err != nil {
		if errors.Is(err, ErrUnknownItem) {
			return toolutil.ErrorResultf("%s: "+t.texts.Unknown, t.name, name)
		}
		return toolutil.ErrorResultf("%s: %v", t.name, err)
	}
	// 这里**不做单条截断**：正文原样返回，只受全局输出安全网约束。
	return toolutil.TextResult(t.truncateOutput(body))
}

// renderCatalog 渲染目录：逐条格式化，超过上限时用一行"… and N more"收尾。
func (t *tool) renderCatalog(items []Item, limit int) string {
	total := len(items)
	shown := items
	if limit > 0 && len(shown) > limit {
		shown = shown[:limit]
	}
	lines := make([]string, 0, len(shown)+1)
	for _, item := range shown {
		lines = append(lines, t.format(item))
	}
	if remaining := total - len(shown); remaining > 0 {
		lines = append(lines, fmt.Sprintf("… and %d more (use search to narrow)", remaining))
	}
	text := strings.Join(lines, "\n")
	if t.trail {
		text += "\n"
	}
	return t.truncateOutput(text)
}

// truncateOutput 处理"整段输出超上限"。
func (t *tool) truncateOutput(text string) string {
	if len(text) <= t.maxOut {
		return text
	}
	if t.mode == TruncateBytes {
		return toolutil.TruncateBytes(text, t.maxOut, t.byteSuffix(t.maxOut))
	}
	// 按整行截断：宁可少显示一条，也不要给模型半行。
	var builder strings.Builder
	for _, line := range strings.Split(text, "\n") {
		if builder.Len()+len(line)+1 > t.maxOut {
			break
		}
		builder.WriteString(line)
		builder.WriteString("\n")
	}
	trimmed := strings.TrimSuffix(builder.String(), "\n")
	if trimmed == "" {
		// 单行就超限：退化成 UTF-8 安全的前缀。
		cut, _ := toolutil.ClampUTF8(text, t.maxOut)
		trimmed = cut
	}
	return trimmed + t.truncatedNote()
}

// truncatedNote 是全局输出安全网触发时的提示。
//
// 措辞刻意说清"这是安全网、不是单条限制"：完整内容请用宿主自己的读取工具或翻页，
// 别在这里反复 load 同一个条目。
func (t *tool) truncatedNote() string {
	return fmt.Sprintf("\n[truncated: content exceeds %.1f KiB; use the host's read tool or paging for the full content]",
		float64(t.maxOut)/1024)
}

// byteSuffix 是兼容模式的截断后缀（与 v0.20 的 skill 逐字节一致）。
func (t *tool) byteSuffix(limit int) string {
	return fmt.Sprintf("…\n[truncated: output exceeded %d bytes]\n", limit)
}

// validateItems 校验 Source 返回的条目：名字非空、摘要非空且不超上限。
//
// AllowEmptySummary 为真时只跳过"摘要必须非空"这一条（摘要超长仍然报错）。
//
// 为什么在这里校验而不是在 New 里：条目是运行期由 Source 产出的，装配期并不知道
// 会有哪些条目。校验失败直接让工具报错（而不是渲染一条空摘要），因为空摘要的目录
// 行对模型毫无信息量，等于把这条记忆藏起来。
func (t *tool) validateItems(items []Item) error {
	for index, item := range items {
		name := strings.TrimSpace(item.Name)
		if name == "" {
			return fmt.Errorf("source returned item %d without a name; every item needs a stable slug", index)
		}
		summary := strings.TrimSpace(item.Summary)
		if summary == "" && !t.allowEmptySummary {
			return fmt.Errorf("item %q has no summary; the catalog only shows the summary, so every item needs one", name)
		}
		if len(summary) > MaxSummaryBytes {
			return fmt.Errorf("item %q has a %d byte summary (limit %d); keep it to one short line", name, len(summary), MaxSummaryBytes)
		}
	}
	return nil
}

// defaultItemLine 渲染默认目录行：`- name — summary (N.N KB)`。
func defaultItemLine(item Item) string {
	name := strings.TrimSpace(item.Name)
	summary := strings.TrimSpace(item.Summary)
	if item.Bytes > 0 {
		return fmt.Sprintf("- %s — %s (%.1f KB)", name, summary, float64(item.Bytes)/1024)
	}
	return fmt.Sprintf("- %s — %s", name, summary)
}

// ItemsFromMap 是一个便利助手：把"名字 → 摘要/正文"的静态映射包成 Source。
//
// 没有搜索排序的智能：按名字升序匹配子串。它服务的是"宿主有几条固定的说明文档"
// 这类最简单的场景；需要权重/分页就自己实现 Source。
func ItemsFromMap(entries map[string]Item) Source {
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	items := make([]Item, 0, len(names))
	for _, name := range names {
		item := entries[name]
		if strings.TrimSpace(item.Name) == "" {
			item.Name = name
		}
		if strings.TrimSpace(item.Title) == "" {
			item.Title = item.Name
		}
		if item.Bytes == 0 {
			item.Bytes = len(entries[name].Summary)
		}
		items = append(items, item)
	}
	return &mapSource{items: items}
}

// mapSource 实现 Source（静态条目、按名字升序、子串搜索）。
type mapSource struct{ items []Item }

func (m *mapSource) List(context.Context) ([]Item, error) {
	return append([]Item(nil), m.items...), nil
}

func (m *mapSource) Search(_ context.Context, query string) ([]Item, error) {
	needle := strings.ToLower(strings.TrimSpace(query))
	if needle == "" {
		return nil, nil
	}
	matches := make([]Item, 0, len(m.items))
	for _, item := range m.items {
		haystack := strings.ToLower(item.Name + "\n" + item.Title + "\n" + item.Summary)
		if strings.Contains(haystack, needle) {
			matches = append(matches, item)
		}
	}
	return matches, nil
}

func (m *mapSource) Load(_ context.Context, name string) (string, error) {
	for _, item := range m.items {
		if item.Name == name {
			return item.Summary, nil
		}
	}
	return "", fmt.Errorf("%w: %s", ErrUnknownItem, name)
}
