package skill

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
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

// Tools 返回绑定到 store 的 skill 工具。store 为 nil（或带类型 nil）时回退到
// DefaultDirs 上的文件系统存储。
func Tools(store rig.SkillStore) []rig.Tool {
	if isNil(store) {
		store = NewDefaultFileStore(currentRoot())
	}
	return []rig.Tool{&skillTool{store: store}}
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

// skillTool 是 skill 工具的 rig.Tool 实现。
type skillTool struct {
	store rig.SkillStore
}

// Name 实现 rig.Tool。
func (t *skillTool) Name() string { return ToolName }

// Description 实现 rig.Tool。
func (t *skillTool) Description() string {
	return "Load agent skills (Markdown documents with reusable instructions) by name. Calling with only " +
		"`name` loads that skill; action=list returns names and descriptions, action=search matches a query " +
		"against name, description and body. Use it before a specialized task to follow the skill's workflow."
}

// Schema 实现 rig.Tool。
func (t *skillTool) Schema() map[string]any {
	return toolutil.ObjectSchema(map[string]any{
		"action": map[string]any{
			"type":        "string",
			"description": "list all skills, search for matching skills, or load one skill by name.",
			"enum":        []string{ActionList, ActionSearch, ActionLoad},
		},
		"query": toolutil.StringProp("Search query for action=search (matched case-insensitively against name, description and body)."),
		"name":  toolutil.StringProp("Skill name to load (DSH-compatible name-only call; as returned by action=list)."),
	})
}

type skillArgs struct {
	Action string `json:"action"`
	Query  string `json:"query"`
	Name   string `json:"name"`
}

// Invoke 实现 rig.Tool。
func (t *skillTool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	var args skillArgs
	if err := toolutil.DecodeArgs(call, &args); err != nil {
		return rig.ToolResult{}, err
	}
	action := strings.ToLower(strings.TrimSpace(args.Action))
	if action == "" {
		// 兼容 DSH：模型可见的 schema 是 name-only 形式；省略 action 时，给出 name 则加载
		// 对应技能，未给出 name 则列出技能。
		if strings.TrimSpace(args.Name) != "" {
			action = ActionLoad
		} else {
			action = ActionList
		}
	}
	switch action {
	case ActionList, ActionSearch, ActionLoad:
	default:
		return toolutil.ErrorResultf("skill: unknown action %q (want list, search or load)", args.Action), nil
	}
	if err := ctx.Err(); err != nil {
		return rig.ToolResult{}, err
	}
	switch action {
	case ActionList:
		skills, err := t.store.List(ctx)
		if err != nil {
			return toolutil.ErrorResultf("skill: %v", err), nil
		}
		if len(skills) == 0 {
			return toolutil.TextResult("no skills found"), nil
		}
		return toolutil.TextResult(toolutil.TruncateBytes(formatSkillList(skills), MaxOutputBytes, fmt.Sprintf("…\n[truncated: output exceeded %d bytes]\n", MaxOutputBytes))), nil
	case ActionSearch:
		query := strings.TrimSpace(args.Query)
		if query == "" {
			return toolutil.ErrorResultf("skill: query is required for action=search"), nil
		}
		skills, err := t.store.List(ctx)
		if err != nil {
			return toolutil.ErrorResultf("skill: %v", err), nil
		}
		matches := searchSkills(skills, query)
		if len(matches) == 0 {
			return toolutil.TextResult(fmt.Sprintf("no skills found matching %q", query)), nil
		}
		return toolutil.TextResult(toolutil.TruncateBytes(formatSkillList(matches), MaxOutputBytes, fmt.Sprintf("…\n[truncated: output exceeded %d bytes]\n", MaxOutputBytes))), nil
	default: // ActionLoad 动作
		name := strings.TrimSpace(args.Name)
		if name == "" {
			return toolutil.ErrorResultf("skill: name is required for action=load"), nil
		}
		loaded, err := t.store.Read(ctx, name)
		if err != nil {
			return toolutil.ErrorResultf("skill: %v", err), nil
		}
		return toolutil.TextResult(toolutil.TruncateBytes(formatSkillContent(loaded), MaxOutputBytes, fmt.Sprintf("…\n[truncated: output exceeded %d bytes]\n", MaxOutputBytes))), nil
	}
}

// formatSkillList 把技能渲染为 "- name: description" 行。
func formatSkillList(skills []rig.Skill) string {
	var builder strings.Builder
	for _, entry := range skills {
		builder.WriteString("- ")
		builder.WriteString(entry.Name)
		if description := strings.TrimSpace(entry.Description); description != "" {
			builder.WriteString(": ")
			builder.WriteString(description)
		}
		builder.WriteString("\n")
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

// --- 共享辅助函数 ---------------------------------------------------------

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
