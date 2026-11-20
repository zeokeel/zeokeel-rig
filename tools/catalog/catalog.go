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
	"read_file", "write_file", "edit_file", "str_replace_editor", "apply_patch",
	"list_dir", "glob", "grep", "search", "read_image", "undo",
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
	"inspect",
	// 宿主交互 / 计划 / 目标
	"ask_user", "present", "plan_review", "exit_plan_mode", "goal",
	"get_goal", "create_goal", "update_goal",
	// 动态工具加载
	"tool_search",
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
	// 沙箱可观测性与运行期授权：sandbox_status 是只读诊断工具；
	// request_sandbox_access 让模型为"某个工具自己的凭据/缓存目录"申请会话级
	// 可写授权（审批由宿主 gate 决定）。二者由 tools/full.All 在
	// AllOptions.SandboxGrants 下构造，覆盖门禁开启该选项后检查。
	"sandbox_status", "request_sandbox_access",
}

// OptionalTool 描述一个**条件构造**的工具：名字可能有条件地存在，而不是"只要
// SDK 被 import 就一定构造"。
//
// 它补上 Static 的一个盲区：Static 只回答"工具面里应该有这个名字"，无法区分
// "名字在账本里但永远构造不出来"与"配置了对应能力才会构造"。覆盖门禁据此把
// 工具面拆成两部分——"必现"的 Static − Optional，以及"允许缺席"的 Optional；
// 条件字段打开时对应工具必须出现。
type OptionalTool struct {
	// Name 是工具名（面向模型的稳定标识符）。
	Name string
	// Field 是控制它是否构造的字段。AllOptions 的字段写成 "AllOptions.X"；
	// 由 kit 追加的工具写它所属包的字段（例如 "kit.Config.PTC"）。
	Field string
	// Gate 用一句话说明构造条件（给人读，不参与判定）。
	Gate string
}

// Optional 是条件构造的工具清单。登记在这里的名字**允许缺席**：覆盖门禁只要求
// Static − Optional 必现，并要求对应条件字段打开时 Optional 里的工具出现。
//
// 注意两个不在 Static 里、但同样是条件构造的条目：
//
//   - spill_read：宿主没有注入 AllOptions.SpillStore 时不存在（同时登记在
//     ServerBound，归类是"宿主不提供外部组件就没有这个工具"）；
//   - run_code：由 kit 在 kit.Config.PTC=true 时追加，tools/full 不构造它。
//
// 新增条件构造的工具时，除了接线还要在这里说明控制字段，否则门禁无法分辨
// "条件构造"与"接线漏了"。
var Optional = []OptionalTool{
	{
		Name:  "apply_patch",
		Field: "AllOptions.Root",
		Gate:  "需要工作区根（AllOptions.Root，或 AllOptions.Policy 的 roots）；补丁工具必须同时拿到 Policy",
	},
	{
		Name:  "search",
		Field: "AllOptions.Root",
		Gate:  "需要工作区根（AllOptions.Root，或 AllOptions.Policy 的 roots）；它按根遍历文件",
	},
	{
		Name:  "undo",
		Field: "AllOptions.Root",
		Gate:  "需要工作区根（AllOptions.Root，或 AllOptions.Policy 的 roots）；UndoTracker 只换 tracker，不改变存在性",
	},
	{
		Name:  "sandbox_status",
		Field: "AllOptions.SandboxGrants",
		Gate:  "AllOptions.SandboxGrants=true 时追加；默认关闭",
	},
	{
		Name:  "request_sandbox_access",
		Field: "AllOptions.SandboxGrants",
		Gate:  "AllOptions.SandboxGrants=true 时追加；默认关闭（开启时还应配置审批门禁）",
	},
	{
		Name:  "spill_read",
		Field: "AllOptions.SpillStore",
		Gate:  "宿主注入 AllOptions.SpillStore 时追加；没有 Store 时模型看到的是文件路径，read_file 可读",
	},
	{
		Name:  "run_code",
		Field: "kit.Config.PTC",
		Gate:  "kit 在 PTC=true 时用 tools/ptc 追加；tools/full.All 不构造它",
	},
}

// OptionalByName 返回某个工具的可选构造说明。
func OptionalByName(name string) (OptionalTool, bool) {
	for _, entry := range Optional {
		if entry.Name == name {
			return entry, true
		}
	}
	return OptionalTool{}, false
}

// OptionalNames 返回 Optional 里登记的全部名字（升序、去重）。
func OptionalNames() []string {
	names := make([]string, 0, len(Optional))
	for _, entry := range Optional {
		names = append(names, entry.Name)
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// Required 返回不依赖额外配置就必须构造的工具名：Static − Optional，升序。
// 覆盖门禁用它断言"必现"的工具在默认配置下一个都不少。
func Required() []string {
	optional := make(map[string]bool, len(Optional))
	for _, entry := range Optional {
		optional[entry.Name] = true
	}
	required := make([]string, 0, len(Static))
	for _, name := range Static {
		if !optional[name] {
			required = append(required, name)
		}
	}
	slices.Sort(required)
	return required
}

// ServerBound 工具只有在配置了外部 server/client 时才存在，因此无法被静态覆盖率
// 检查构造出来。它们仍然属于账本：对应的构造函数与测试覆盖了它们。
var ServerBound = []string{
	// 溢出结果的读回入口：只在宿主注入 rig.SpillPolicy.Store 时才构造
	// （tools/full.All 据 AllOptions.SpillStore 决定）。与 MCP 同属一类：
	// "没有宿主提供的外部组件，这个工具就不存在"。
	"spill_read",
	// MCP 资源/提示工具（stdio 或 streamable HTTP）：每个 server 一个 client，模型可见名
	// 带 server 身份 —— <prefix><server>__<base>（默认 prefix "mcp__"，例如
	// `mcp__srv__list_resources`）。server 名在构造期才知道，所以账本记形态而不是固定名。
	"<prefix><server>__list_resources", "<prefix><server>__read_resource",
	"<prefix><server>__list_resource_templates", "<prefix><server>__list_prompts",
	"<prefix><server>__get_prompt",
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
	"session_event_search", "session_event_trace", "inspect",
	// web / MCP / LSP
	"web_fetch", "web_search",
	// 进程
	"terminal_open", "terminal_send", "terminal_read", "terminal_signal",
	"terminal_close", "terminal_list",
	"job_start", "job_output", "job_kill", "job_list",
	// 记忆
	"memory",
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
// todo_write 等）取代。截至 v0.23.x 它们仍然构造、仍然可用（presets 的
// standard/all 保留它们，personal 不含编排类）；移除任何一个都属于破坏性变更，
// 必须走 docs/VERSIONING.md 的弃用流程并在 CHANGELOG.md 说明。
var Legacy = []string{
	"ask_user", "bash", "close_team", "edit_file", "enter_worktree",
	"exit_plan_mode", "exit_worktree", "glob", "goal", "grep", "inspect",
	"interrupt_agent", "job_kill", "job_list", "job_output", "job_start",
	"list_subagent_models", "list_teams", "list_worktrees", "lsp",
	"<prefix><server>__get_prompt", "<prefix><server>__list_prompts",
	"<prefix><server>__list_resources", "<prefix><server>__read_resource",
	"memory",
	"plan_review", "present", "pwsh", "ralph", "read_file", "read_image",
	"schedule_create", "schedule_delete", "schedule_list",
	"schedule_update", "send_message", "session_event_read",
	"session_event_search", "session_event_trace", "session_search",
	"session_trace", "spawn_team",
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
