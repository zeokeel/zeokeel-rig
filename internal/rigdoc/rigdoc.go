// Package rigdoc 从真实工具构造器生成 AI 可读的 Rig 工具目录。
//
// 它是 cmd/rigdoc 的实现，也被文档一致性测试直接调用；生成结果是
// docs/ai/tools.json 与 docs/ai/tools.md。
package rigdoc

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/audit"
	"github.com/zeokeel/zeokeel-rig/tools/full"
	"github.com/zeokeel/zeokeel-rig/tools/ptc"
)

// ToolDoc 是一个工具在 AI 目录中的可序列化描述。
type ToolDoc struct {
	Name         string         `json:"name"`
	Category     string         `json:"category"`
	Availability string         `json:"availability"`
	DSHReference string         `json:"dsh_reference,omitempty"`
	Description  string         `json:"description"`
	Required     []string       `json:"required,omitempty"`
	Properties   map[string]any `json:"properties,omitempty"`
}

// JSONPath 与 MarkdownPath 是仓库中检入的目录文件。
const (
	JSONPath     = "docs/ai/tools.json"
	MarkdownPath = "docs/ai/tools.md"
)

// BuildCatalog 从 tools/full.All() 与 kit 侧 run_code 构造完整目录。
func BuildCatalog() []ToolDoc {
	policy := sandbox.New(sandbox.WithRoots("."))
	tools := full.All(full.AllOptions{Policy: policy, Root: "."})
	dsh := make(map[string]string, len(audit.Mappings))
	for _, mapping := range audit.Mappings {
		dsh[mapping.Rig] = mapping.Reference
	}
	docs := make([]ToolDoc, 0, len(tools)+1)
	for _, tool := range tools {
		docs = append(docs, describe(tool, "static", dsh[tool.Name()]))
	}
	// run_code 是 kit 的可选扩展，不在 full.All 的静态工具中。
	docs = append(docs, describe(ptc.New(ptc.Config{}), "kit.Config.PTC", dsh["run_code"]))
	sort.Slice(docs, func(i, j int) bool { return docs[i].Name < docs[j].Name })
	return docs
}

// JSON 生成稳定的缩进 JSON（末尾带换行）。
func JSON(docs []ToolDoc) []byte {
	data, err := json.MarshalIndent(docs, "", "  ")
	if err != nil {
		panic(fmt.Sprintf("rigdoc: marshal catalog: %v", err))
	}
	return append(data, '\n')
}

// Markdown 生成适合人/AI 快速浏览的目录表格。
func Markdown(docs []ToolDoc) string {
	var b strings.Builder
	b.WriteString("# Rig 工具目录（AI 可读，自动生成）\n\n")
	b.WriteString("> 由 `go run ./cmd/rigdoc` 从真实 `tools/full.All()` 生成，请勿手改。\n")
	b.WriteString("> 机器可读版本：`docs/ai/tools.json`；中文使用/组装指南：`docs/zh/AI-SDK.md`。\n\n")
	b.WriteString("| 工具 | 能力类别 | 可用性 | DSH 对应 | 作用 | 必填参数 |\n")
	b.WriteString("| --- | --- | --- | --- | --- | --- |\n")
	for _, doc := range docs {
		description := strings.ReplaceAll(doc.Description, "|", "\\|")
		description = strings.ReplaceAll(description, "\n", " ")
		if len(description) > 180 {
			description = description[:177] + "..."
		}
		required := "—"
		if len(doc.Required) > 0 {
			required = "`" + strings.Join(doc.Required, "`, `") + "`"
		}
		dsh := doc.DSHReference
		if dsh == "" {
			dsh = "—"
		}
		b.WriteString(fmt.Sprintf("| `%s` | %s | %s | %s | %s | %s |\n",
			doc.Name, doc.Category, doc.Availability, dsh, description, required))
	}
	b.WriteString("\n## 条件/服务端工具\n\n")
	b.WriteString("- `mcp__<server>__<tool>`：由 `full.AllOptions.MCPClients` 中每个 MCP server 动态生成。\n")
	b.WriteString("- `mcp_list_resources` / `mcp_read_resource` / `mcp_list_resource_templates` / `mcp_list_prompts` / `mcp_get_prompt` / `lsp` / `lsp_*`：仅配置 MCP/LSP client 时构造（server-bound）。\n")
	b.WriteString("- `structured_output`：仅注入到设置了 `SubagentSpec.OutputSchema` 的子 agent 上下文，不是静态工具。\n")
	return b.String()
}

// Write 重新生成两个目录文件。
func Write(jsonPath, mdPath string) error {
	docs := BuildCatalog()
	if err := os.WriteFile(jsonPath, JSON(docs), 0o644); err != nil {
		return err
	}
	return os.WriteFile(mdPath, []byte(Markdown(docs)), 0o644)
}

// Check 校验检入文件是否与当前构造器一致。
func Check(jsonPath, mdPath string) error {
	docs := BuildCatalog()
	for path, want := range map[string][]byte{
		jsonPath: JSON(docs),
		mdPath:   []byte(Markdown(docs)),
	} {
		got, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("%s is missing; run: go run ./cmd/rigdoc: %w", path, err)
		}
		if string(got) != string(want) {
			return fmt.Errorf("%s is stale; run: go run ./cmd/rigdoc", path)
		}
	}
	return nil
}

func describe(tool rig.Tool, availability, dsh string) ToolDoc {
	schema := tool.Schema()
	doc := ToolDoc{
		Name:         tool.Name(),
		Category:     categoryOf(tool.Name()),
		Availability: availability,
		DSHReference: dsh,
		Description:  tool.Description(),
	}
	if properties, ok := schema["properties"].(map[string]any); ok {
		doc.Properties = properties
	}
	if required, ok := schema["required"].([]string); ok {
		doc.Required = required
	} else if required, ok := schema["required"].([]any); ok {
		for _, item := range required {
			if name, ok := item.(string); ok {
				doc.Required = append(doc.Required, name)
			}
		}
	}
	return doc
}

func categoryOf(name string) string {
	switch name {
	case "read_file", "write_file", "edit_file", "str_replace_editor", "list_dir", "glob", "grep", "read_image":
		return "files"
	case "bash", "pwsh", "terminal_open", "terminal_send", "terminal_read", "terminal_signal", "terminal_close", "terminal_list", "job_start", "job_output", "job_kill", "job_list", "job_wait":
		return "shell"
	case "web_fetch", "web_search":
		return "web"
	case "ask_user", "present", "present_artifact", "plan_review", "exit_plan_mode", "goal", "get_goal", "create_goal", "update_goal", "settings_get", "settings_set":
		return "host"
	case "session_search", "session_trace", "session_event_read", "session_event_search", "session_event_trace", "inspect", "report":
		return "session"
	case "memory", "schedule_create", "schedule_list", "schedule_update", "schedule_delete", "enter_worktree", "exit_worktree", "list_worktrees", "working_directory", "todo", "todo_write", "skill":
		return "state"
	case "subagent", "subagent_fork", "spawn_teammate", "list_agents", "wait_agent", "team_spawn", "team_list", "team_send", "team_wait", "team_last", "team_close", "team_interrupt", "spawn_team", "list_teams", "team_status", "close_team", "wait_team", "send_message", "interrupt_agent", "list_subagent_models", "team_task_create", "team_task_list", "team_task_get", "team_task_update", "workflow", "ralph", "code_review", "auto_review":
		return "orchestration"
	case "run_code":
		return "programmatic"
	default:
		return "other"
	}
}
