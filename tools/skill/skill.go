package skill

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
	"github.com/zeokeel/zeokeel-rig/tools/shelf"
)

// ToolName 是模型可见的工具名。
const ToolName = "skill"

// skill 工具支持的 action。
const (
	ActionList   = "list"
	ActionSearch = "search"
	ActionLoad   = "load"
)

// MaxOutputBytes 限制单次 skill 工具响应的字节数。
const MaxOutputBytes = 32 << 10

// Tools 返回绑定到 store 的 skill 工具，默认库里的 <root> 取**进程工作目录**。
//
// 它只适合"工作区就是进程 cwd"的简单宿主。守护进程 / GUI 这类 cwd 与工作区
// 无关的场景必须用 ToolsWithRoot（tools/full 就是这么接的），否则
// <Root>/.zeokeel/skills 会被静默跳过。
//
// 实现走的是书架原语 `tools/shelf`：本文件只提供"技能从哪来"（storeSource）
// 与"技能怎么渲染"（formatSkillItem / formatSkillContent），目录/正文的分离、上限、
// 截断、action 解析、错误包装都由 `tools/shelf`（书架原语）负责。模型可见行为与原实现逐字节一致
// （schema、描述、`- name: description` 目录行、`…\n[truncated: …]` 截断后缀）。
func Tools(store rig.SkillStore) []rig.Tool {
	return ToolsWithRoot(currentRoot(), store)
}

// ToolsWithRoot 返回绑定到 store 的 skill 工具；store 为 nil（或带类型 nil）时
// 回退到 DefaultDirs(root) 上的文件系统存储（$ZEOKEEL_SKILLS、<root>/.zeokeel/skills、
// ~/.zeokeel/skills）。
//
// root 是宿主的**工作区根**（tools/full 的 AllOptions.Root），与进程 cwd 无关：
// 守护进程从 / 启动、GUI 从家目录启动时，项目级技能仍然能被发现。
func ToolsWithRoot(root string, store rig.SkillStore) []rig.Tool {
	if isNil(store) {
		store = NewDefaultFileStore(root)
	}
	tool, err := shelf.New(&storeSource{store: store}, skillOptions())
	if err != nil {
		// 选项是包内常量，装配期不可能失败；真发生了也要给模型一个可读错误，
		// 而不是 panic（工具构造不应该让宿主进程崩）。
		return []rig.Tool{brokenTool{err: err}}
	}
	return []rig.Tool{tool}
}

// skillOptions 把"skill 的历史行为"翻译成书架原语的选项。这里每一项都是刻意保留旧语义的
// 兼容开关：协议层要逐字节不变，新能力由原语的默认值提供。
func skillOptions() shelf.Options {
	return shelf.Options{
		ToolName:    ToolName,
		Description: legacyDescription,
		Schema: toolutil.ObjectSchema(map[string]any{
			"action": map[string]any{
				"type":        "string",
				"description": "list all skills, search for matching skills, or load one skill by name.",
				"enum":        []string{ActionList, ActionSearch, ActionLoad},
			},
			"query": toolutil.StringProp("Search query for action=search (matched case-insensitively against name, description and body)."),
			"name":  toolutil.StringProp("Skill name to load (DSH-compatible name-only call; as returned by action=list)."),
		}),
		// 旧实现没有条数上限（靠 32 KiB 总输出截断兜底）。
		MaxListed: -1,
		MaxSearch: -1,
		// 只有全局输出上限（旧实现就是 32 KiB）；`load` 不做单条截断。
		MaxOutputBytes: MaxOutputBytes,
		FormatItem:     formatSkillItem,
		// 旧实现的目录每行以换行结尾，整段也以换行结尾。
		TrailingNewline: true,
		// 旧实现的截断文案是 `…\n[truncated: output exceeded N bytes]\n`。
		Truncate: shelf.TruncateBytes,
		Texts: shelf.Texts{
			EmptyList: "no skills found",
			NoMatch:   "no skills found matching %q",
		},
		// 技能允许没有 description（社区技能常见），目录行会退化成 `- name`。
		AllowEmptySummary: true,
	}
}

// legacyDescription 是 v0.20 起模型可见的技能描述，保持逐字节不变。
const legacyDescription = "Load agent skills (Markdown documents with reusable instructions) by name. Calling with only " +
	"`name` loads that skill; action=list returns names and descriptions, action=search matches a query " +
	"against name, description and body. Use it before a specialized task to follow the skill's workflow."

// storeSource 把 rig.SkillStore 适配成渐进式原语的内容来源。
type storeSource struct {
	store rig.SkillStore
}

// List 实现 shelf.Source：只读元数据（不读正文，正文在 store.Read 里）。
func (s *storeSource) List(ctx context.Context) ([]shelf.Item, error) {
	skills, err := s.store.List(ctx)
	if err != nil {
		return nil, err
	}
	return itemsFromSkills(skills), nil
}

// Search 实现 shelf.Source：匹配规则与原实现一致（name/description/body、
// 大小写不敏感、多词要求全部命中），并保持 store 的原始顺序。
func (s *storeSource) Search(ctx context.Context, query string) ([]shelf.Item, error) {
	skills, err := s.store.List(ctx)
	if err != nil {
		return nil, err
	}
	return itemsFromSkills(searchSkills(skills, query)), nil
}

// Load 实现 shelf.Source：只有这里会读出正文并渲染成模型看到的页面。
func (s *storeSource) Load(ctx context.Context, name string) (string, error) {
	loaded, err := s.store.Read(ctx, name)
	if err != nil {
		return "", err
	}
	return formatSkillContent(loaded), nil
}

// itemsFromSkills 把技能列表投影成原语的条目（目录里显示 description）。
func itemsFromSkills(skills []rig.Skill) []shelf.Item {
	items := make([]shelf.Item, 0, len(skills))
	for _, entry := range skills {
		items = append(items, shelf.Item{
			Name:    entry.Name,
			Title:   entry.Name,
			Summary: strings.TrimSpace(entry.Description),
			Bytes:   len(entry.Body),
		})
	}
	return items
}

// formatSkillItem 渲染目录行：`- name: description`（没有描述时只留名字）。
func formatSkillItem(item shelf.Item) string {
	var builder strings.Builder
	builder.WriteString("- ")
	builder.WriteString(item.Name)
	if description := strings.TrimSpace(item.Summary); description != "" {
		builder.WriteString(": ")
		builder.WriteString(description)
	}
	return builder.String()
}

// searchSkills 对 name、description 与 body 做大小写不敏感匹配。多词查询要求每个词
// 都出现在技能的某处。
func searchSkills(skills []rig.Skill, query string) []rig.Skill {
	terms := strings.Fields(strings.ToLower(query))
	var matches []rig.Skill
	for _, entry := range skills {
		haystack := strings.ToLower(entry.Name + "\n" + entry.Description + "\n" + entry.Body)
		matchedAll := true
		for _, term := range terms {
			if !strings.Contains(haystack, term) {
				matchedAll = false
				break
			}
		}
		if matchedAll {
			matches = append(matches, entry)
		}
	}
	return matches
}

// formatSkillContent 渲染一份完整技能供模型阅读。
func formatSkillContent(skill rig.Skill) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "name: %s\n", skill.Name)
	if description := strings.TrimSpace(skill.Description); description != "" {
		fmt.Fprintf(&builder, "description: %s\n", description)
	}
	fmt.Fprintf(&builder, "path: %s\n", skill.Path)
	builder.WriteString("---\n")
	builder.WriteString(skill.Body)
	if !strings.HasSuffix(skill.Body, "\n") {
		builder.WriteString("\n")
	}
	return builder.String()
}

// currentRoot 返回进程工作目录（不可用时为空字符串）；它是默认技能搜索路径中的
// <root> 部分。
func currentRoot() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return wd
}

// brokenTool 是"装配失败"时的占位工具：调用返回可读错误，而不会 panic。
type brokenTool struct{ err error }

func (t brokenTool) Name() string           { return ToolName }
func (t brokenTool) Description() string    { return legacyDescription }
func (t brokenTool) Schema() map[string]any { return skillOptions().Schema }
func (t brokenTool) Invoke(context.Context, rig.ToolCall) (rig.ToolResult, error) {
	return toolutil.ErrorResultf("%s: tool is misconfigured: %v", ToolName, t.err), nil
}

// isNil 报告接口值是否为 nil（含带类型的 nil）。
func isNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
