// Package catalog 是 ZeoKeel Rig 的工具覆盖账本（coverage ledger）。
//
// 它记录旧版 ZeoKeel agent 暴露过的每个工具名，并对那些只有在配置了外部依赖
// （MCP server 或语言服务器）时才存在的工具进行分类。tools/full 包中的覆盖率
// 门禁（TestToolCatalogCoverage）断言下面每个静态名字都能由 tools/full.All 构造，
// 因此“新增工具却没有接线进 SDK”或“误删工具”都会让 CI 失败。
//
// 请保持本列表与 docs/TOOLS.md 同步：它是每次发布都会执行的“有没有漏掉什么？”
// 检查清单。
//
// # 典型用法
//
// 发布门禁与测试把一次构建产出的工具名交给账本检查：
//
//	// names 是构建产出的 []rig.Tool 的工具名列表。
//	report := catalog.Check(names)
//	if !report.OK() {
//		log.Fatalf("缺少静态工具: %v", report.Missing)
//	}
//	if missing := catalog.AuditModern(names); len(missing) > 0 {
//		log.Fatalf("缺少现代工具: %v", missing)
//	}
//	if missing := catalog.AuditLegacy(names); len(missing) > 0 {
//		log.Fatalf("缺少旧版工具: %v", missing)
//	}
package catalog

import (
	"fmt"
	"slices"

	"strings"
)

// Static 是无头构建必须能在不依赖外部服务器或 UI 的情况下构造出的全部工具名：
// 旧版工具面加上现代新增（skills、团队任务板、评审）。这里的每个名字都必须由
// tools/full.All 产出，而它产出的每个工具也都必须列在这里。
var Static = []string{
	// 文件系统 / 编辑器 / 图片
	"read_file", "write_file", "edit_file", "str_replace_editor",
	"list_dir", "glob", "grep", "read_image",
	// shell / 进程
	"bash", "pwsh",
	"terminal_open", "terminal_send", "terminal_read", "terminal_signal",
	"terminal_close", "terminal_list",
	"job_start", "job_output", "job_kill", "job_list", "job_wait",
	// web
	"web_fetch", "web_search",
	// 会话 / 诊断
	"session_search", "session_trace",
	"session_event_read", "session_event_search", "session_event_trace",
	"inspect", "report",
	// 宿主交互 / 计划 / 目标 / 设置
	"ask_user", "present", "present_artifact", "plan_review", "exit_plan_mode", "goal",
	"get_goal", "create_goal", "update_goal",
	"settings_get", "settings_set",
	// 记忆 / 调度 / worktree
	"memory",
	"schedule_create", "schedule_list", "schedule_update", "schedule_delete",
	"enter_worktree", "exit_worktree", "list_worktrees",
	// 编排
	"subagent", "subagent_fork", "spawn_teammate", "list_agents", "wait_agent", "workflow", "ralph",
	"team_spawn", "team_list", "team_send", "team_wait", "team_last",
	"team_close", "team_interrupt",
	"spawn_team", "list_teams", "team_status", "close_team", "wait_team",
	"send_message", "interrupt_agent", "list_subagent_models",
	// 杂项
	"working_directory", "todo", "todo_write",
	// 现代工具面新增（v0.7/v0.8）：skills、团队任务板、评审
	"skill", "code_review", "auto_review",
	"team_task_create", "team_task_list", "team_task_get", "team_task_update",
}

// ServerBound 工具只有在配置了外部 server/client 时才存在，因此无法被静态覆盖率
// 检查构造出来。它们仍然属于账本：对应的构造函数与测试覆盖了它们。
var ServerBound = []string{
	// MCP（stdio 或 streamable HTTP）：每个 server 一个 client
	"mcp_list_resources", "mcp_read_resource", "mcp_list_resource_templates",
	"mcp_list_prompts", "mcp_get_prompt",
	// 旧的单一 LSP 工具（lsp_* 工具是静态构造函数）
	"lsp",
}

// Modern 是当代编码 agent 应当暴露的工具面（派生自 DeepSeek Harness / ZeoKeel
// agent 的工具集）。它是超集式审计：名字可能与 Legacy 重叠，来自现代工具面的
// 新增项也放在这里。AuditModern 会报告构建中缺失的名字。
var Modern = []string{
	// 交互 / 文件 / shell
	"ask_user", "present", "read_file", "write_file", "edit_file",
	"read_image", "bash", "pwsh", "glob", "grep", "working_directory",
	"todo_write", "todo",
	// 目标 / 计划
	"goal", "get_goal", "create_goal", "update_goal", "plan_review", "exit_plan_mode",
	// 技能
	"skill",
	// 编排
	"subagent", "subagent_fork", "workflow", "ralph",
	"team_spawn", "team_list", "team_send", "team_wait", "team_close",
	"team_interrupt",
	"team_task_create", "team_task_list", "team_task_get", "team_task_update",
	// 评审
	"code_review", "auto_review",
	// 调度 / worktree
	"schedule_create", "schedule_list", "schedule_update", "schedule_delete",
	"enter_worktree", "exit_worktree", "list_worktrees",
	// 会话 / 诊断
	"session_search", "session_trace", "session_event_read",
	"session_event_search", "session_event_trace", "inspect", "report",
	// web / MCP / LSP
	"web_fetch", "web_search",
	// 进程
	"terminal_open", "terminal_send", "terminal_read", "terminal_signal",
	"terminal_close", "terminal_list",
	"job_start", "job_output", "job_kill", "job_list",
	// 记忆 / 设置
	"memory", "settings_get", "settings_set",
}

// DocumentedGaps 是刻意暂不暴露为静态工具名的现代能力，原因记录在
// docs/COVERAGE.md。子 agent 上下文工具 `structured_output` 属于子 agent 能力
// （SubagentSpec.OutputSchema），而不是独立的静态名字。
var DocumentedGaps []string

// AuditModern 报告构建缺失了哪些现代工具名。
func AuditModern(have []string) []string {
	present := make(map[string]bool, len(have))
	for _, name := range have {
		present[name] = true
	}
	var missing []string
	for _, name := range Modern {
		if !present[name] {
			missing = append(missing, name)
		}
	}
	slices.Sort(missing)
	return missing
}

// Dynamic 是运行时根据 server 自己的工具列表生成的工具名模式。
var Dynamic = []string{
	"mcp__<server>__<tool>",
}

// Legacy 是上一代 ZeoKeel agent 的确切工具面（提取自 old/internal/tools 以及
// runtime settings 工具）。AuditLegacy 校验：在抽出 Rig 的过程中，产品过去暴露的
// 工具没有缺失；发布检查清单会把它与 Check 一起运行。
//
// Deprecated: 本列表中的旧版工具名别名（例如 "spawn_team"、"list_teams"、
// "wait_team"、"close_team"、"send_message"、"interrupt_agent"、"todo"）是为了
// 兼容 v0.7 之前的提示词与集成而保留的，自 v0.9.1 起已被现代名字
// （team_spawn、team_list、team_wait、team_close、team_send、team_interrupt、
// todo_write 等）取代。它们在整个 v0.9.x 期间保持可用，并计划在 v0.10.0 移除；
// 参见 CHANGELOG.md 与 docs/VERSIONING.md。
var Legacy = []string{
	"ask_user", "bash", "close_team", "edit_file", "enter_worktree",
	"exit_plan_mode", "exit_worktree", "glob", "goal", "grep", "inspect",
	"interrupt_agent", "job_kill", "job_list", "job_output", "job_start",
	"list_subagent_models", "list_teams", "list_worktrees", "lsp",
	"mcp_get_prompt", "mcp_list_prompts", "mcp_list_resources", "mcp_read_resource",
	"memory",
	"plan_review", "present", "pwsh", "ralph", "read_file", "read_image",
	"report", "schedule_create", "schedule_delete", "schedule_list",
	"schedule_update", "send_message", "session_event_read",
	"session_event_search", "session_event_trace", "session_search",
	"session_trace", "settings_get", "settings_set", "spawn_team",
	"str_replace_editor", "subagent", "team_status", "terminal_close",
	"terminal_list", "terminal_open", "terminal_read", "terminal_send",
	"terminal_signal", "todo", "wait_team", "web_fetch", "web_search",
	"workflow", "working_directory", "write_file",
}

// AuditLegacy 报告构建中仍然缺失哪些旧版工具名（server-bound 名字由调用方单独
// 报告）。
func AuditLegacy(have []string) []string {
	present := make(map[string]bool, len(have))
	for _, name := range have {
		present[name] = true
	}
	var missing []string
	for _, name := range Legacy {
		if !present[name] {
			missing = append(missing, name)
		}
	}
	slices.Sort(missing)
	return missing
}

// Report 是把一组工具与账本比较后的结果。
type Report struct {
	Missing []string
	Extra   []string
}

// OK 报告每个静态工具是否都存在。
func (r Report) OK() bool { return len(r.Missing) == 0 }

// String 渲染一段紧凑摘要（测试与发布检查使用）。
func (r Report) String() string {
	switch {
	case r.OK() && len(r.Extra) == 0:
		return "tool catalog: complete"
	case r.OK():
		return fmt.Sprintf("tool catalog: complete (+%d extra: %s)", len(r.Extra), strings.Join(r.Extra, ", "))
	default:
		return fmt.Sprintf("tool catalog: missing %d (%s)", len(r.Missing), strings.Join(r.Missing, ", "))
	}
}

// Check 把构建产出的工具名与 Static 比较。
func Check(have []string) Report {
	present := make(map[string]bool, len(have))
	for _, name := range have {
		present[name] = true
	}
	expected := make(map[string]bool, len(Static))
	for _, name := range Static {
		expected[name] = true
	}
	report := Report{}
	for _, name := range Static {
		if !present[name] {
			report.Missing = append(report.Missing, name)
		}
	}
	for _, name := range have {
		if !expected[name] {
			report.Extra = append(report.Extra, name)
		}
	}
	slices.Sort(report.Missing)
	slices.Sort(report.Extra)
	return report
}
