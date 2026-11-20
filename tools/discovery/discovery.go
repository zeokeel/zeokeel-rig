// Package discovery 提供 `tool_search`：让模型在**运行期**发现并激活那些没有
// 随初始请求发送的工具定义（lazy tool discovery）。
//
// # 为什么需要它
//
// 工具多了以后，每个工具的 JSON Schema 都会挤进每一次请求的 tools 数组：既花钱
// （输入 token）又稀释注意力。动态加载的思路是"默认只发常用的那十几个，其余的
// 注册但不发；模型需要时自己把它们加载进来"。
//
// # 为什么它对 prefix cache 友好
//
// 本 SDK 的 system prompt **不含工具描述**（见 kit 的 assembleSystemPrompt），
// 因此激活工具不需要重建 prompt；只有 `Request.Tools` 这一个字段变化。把激活
// 设计成**纯增量**（只在末尾追加）之后，已激活部分逐字节稳定，缓存前缀只在激活
// 的那一轮断开一次，之后恢复命中。
//
// # 模型可见工具
//
//	tool_search {}                        // 列出还没激活、且可发现的工具（默认最多 40 条）
//	tool_search {"query": "pdf"}          // 按名字/描述筛选
//	tool_search {"activate": ["a", "b"]}  // 激活；返回 activated / unknown
//
// schema 刻意只有两个可选字段，数组元素是字符串——它自己也要占 token，不能为了
// 发现工具再引入一份大 schema。
//
// # 组装
//
// 工具不 import agent / kit / tools/full，也不持有任何注册表：宿通过 Options 注入
// 两个回调（列清单、激活），因此这个包是叶子，可以出现在任何装配路径里。
//
//	tool := discovery.New(discovery.Options{
//		List:     func() []discovery.Item { ... },          // 未激活的工具
//		Activate: func(names []string) ([]string, []string) // 来自 agent.ActivateTools
//	})
//
// 没注入回调时工具仍然可调用：每次返回可读错误（不 panic），与 sandbox_status 的
// "不知道就不能假装知道"约定一致。
package discovery

import (
	"context"
	"fmt"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// ToolSearch 是工具面向模型的稳定名字。
const ToolSearch = "tool_search"

// DefaultLimit 是清单默认最多展示的条数。
const DefaultLimit = 40

// Item 是清单里的一条工具（只含模型挑选所需的最小信息）。
type Item struct {
	// Name 是工具名（模型用它激活）。
	Name string
	// Summary 是一句话描述（工具 Description 的首行/截断）。
	Summary string
}

// Options 注入宿主的注册表能力。
//
// 两个回调都带 ctx：激活是**按会话**的（同一个 agent 可以被多个会话共享），而
// 工具调用只能从 ctx 里拿到会话标识（agent 在派发工具时注入，见
// agent.ToolSessionID）。
type Options struct {
	// List 返回**尚未激活且可发现**的工具（按工具的注册顺序）。返回 nil 表示没有
	// 可发现工具。
	//
	// 清单的**覆盖面**由宿主负责：它必须是**全部已注册工具**（不是某个静态名字
	// 表），并排除两类工具——已在目标会话 active 的（清单只回答"还能加载什么"），
	// 以及自己声明不可发现的（rig.DiscoverableTool 返回 false；不可发现只是搜不到，
	// 仍可用 ActivateTools 按名字激活）。kit 的接线就是这么做的。
	List func(ctx context.Context) []Item
	// Activate 激活给定名字，返回生效与未知的名字（语义同
	// rig.ToolActivator.ActivateTools）。
	Activate func(ctx context.Context, names []string) (activated, unknown []string)
	// Limit 覆盖清单条数上限（<=0 用 DefaultLimit）。
	Limit int
}

// tool 是 tool_search 的 rig.Tool 实现。
type tool struct {
	opts Options
}

// New 返回 tool_search 工具。opts 的两个回调可为 nil（此时调用返回可读错误）。
func New(opts Options) rig.Tool {
	return &tool{opts: opts}
}

// Name 实现 rig.Tool。
func (t *tool) Name() string { return ToolSearch }

// Description 实现 rig.Tool（英文，简短：它每个请求都要占 token）。
func (t *tool) Description() string {
	return "Discover and activate tools that are registered but not loaded yet. " +
		"Call with no arguments to list available tools, with query to filter, " +
		"or with activate to load tools by name."
}

// Schema 实现 rig.Tool：只有两个可选字段，元素类型都是字符串。
func (t *tool) Schema() map[string]any {
	return toolutil.ObjectSchema(map[string]any{
		"query":    toolutil.StringProp("Optional substring to filter tool names and descriptions."),
		"activate": toolutil.StringArrayProp("Optional tool names to activate. Activation is additive: tools already active are ignored."),
	})
}

// args 是 tool_search 的参数。
type args struct {
	Query    string   `json:"query"`
	Activate []string `json:"activate"`
}

// Invoke 实现 rig.Tool。
func (t *tool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return rig.ToolResult{}, err
	}
	var decoded args
	if err := toolutil.DecodeArgs(call, &decoded); err != nil {
		return toolutil.ErrorResultf("%s: %v", ToolSearch, err), nil
	}
	if len(decoded.Activate) > 0 {
		return t.activate(ctx, decoded.Activate), nil
	}
	if t.opts.List == nil {
		return toolutil.ErrorResultf("%s: no discovery registry is configured by the host", ToolSearch), nil
	}
	items := t.opts.List(ctx)
	query := strings.ToLower(strings.TrimSpace(decoded.Query))
	if query != "" {
		items = filterItems(items, query)
	}
	if len(items) == 0 {
		if query == "" {
			return toolutil.TextResult("no tools are available to discover; every registered tool is already active"), nil
		}
		return toolutil.TextResult(fmt.Sprintf("no tool matches %q", decoded.Query)), nil
	}
	return toolutil.TextResult(renderList(items, t.limit())), nil
}

// activate 处理 activate 请求。签名保持与 rig.ToolActivator.ActivateTools 一致，
// 因此宿主可以直接把方法值传进来。
func (t *tool) activate(ctx context.Context, names []string) rig.ToolResult {
	if t.opts.Activate == nil {
		return toolutil.ErrorResultf("%s: no activator is configured by the host", ToolSearch)
	}
	activated, unknown := t.opts.Activate(ctx, names)
	var builder strings.Builder
	if len(activated) > 0 {
		fmt.Fprintf(&builder, "activated: %s", strings.Join(activated, ", "))
	} else {
		builder.WriteString("activated: (none)")
	}
	if len(unknown) > 0 {
		if builder.Len() > 0 {
			builder.WriteString("\n")
		}
		fmt.Fprintf(&builder, "unknown: %s", strings.Join(unknown, ", "))
	}
	if len(activated) == 0 && len(unknown) == 0 {
		builder.WriteString("\nhint: pass tool names in activate (for example {\"activate\":[\"web_fetch\"]})")
	} else if len(activated) > 0 {
		builder.WriteString("\nthe activated tools are available from your next message on")
	}
	return toolutil.TextResult(builder.String())
}

// limit 返回清单条数上限。
func (t *tool) limit() int {
	if t.opts.Limit > 0 {
		return t.opts.Limit
	}
	return DefaultLimit
}

// filterItems 按名字/摘要的子串匹配筛选（大小写不敏感）。名字命中排在摘要命中
// 之前：模型通常记得名字的一部分。
func filterItems(items []Item, query string) []Item {
	byName := make([]Item, 0, len(items))
	bySummary := make([]Item, 0, len(items))
	for _, item := range items {
		name := strings.ToLower(item.Name)
		switch {
		case strings.Contains(name, query):
			byName = append(byName, item)
		case strings.Contains(strings.ToLower(item.Summary), query):
			bySummary = append(bySummary, item)
		}
	}
	return append(byName, bySummary...)
}

// renderList 渲染清单：`name — summary`，超长时截断并说明还剩多少。
func renderList(items []Item, limit int) string {
	shown := items
	if limit > 0 && len(items) > limit {
		shown = items[:limit]
	}
	var builder strings.Builder
	for index, item := range shown {
		if index > 0 {
			builder.WriteString("\n")
		}
		builder.WriteString(item.Name)
		if summary := strings.TrimSpace(item.Summary); summary != "" {
			builder.WriteString(" — ")
			builder.WriteString(firstLine(summary))
		}
	}
	if remaining := len(items) - len(shown); remaining > 0 {
		// 模型可见文案：英文（仓库硬规则）。
		fmt.Fprintf(&builder, "\n…and %d more; use query to narrow the results", remaining)
	}
	return builder.String()
}

// firstLine 取第一行并限制长度（清单要短）。
func firstLine(text string) string {
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		text = text[:index]
	}
	text = strings.TrimSpace(text)
	const maxRunes = 120
	runes := []rune(text)
	if len(runes) > maxRunes {
		return string(runes[:maxRunes]) + "…"
	}
	return text
}
