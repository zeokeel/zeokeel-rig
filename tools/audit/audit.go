// Package audit 是 ZeoKeel Rig 可复现的“现代 agent 工具面”覆盖审计。
//
// 它记录参考现代 agent（DeepSeek Harness，@deepseek-ai/dsh 0.2.1-rc.2 —— 本次
// 审计所针对的安装版本）面向模型的工具目录，把每个参考工具映射到覆盖它的 Rig
// 工具，并记录对一个纯工具集 SDK 而言刻意不在范围内的能力（DocumentedGaps，
// 按 kernel-bound 或 host-bound 分组）。
//
// 该审计由 modern_test.go 强制执行：它通过 tools/full.All 构建完整的静态 Rig
// 工具集，并在某个已映射的现代工具缺失时失败。新增参考工具或 Rig 工具时，必须
// 同步更新这里的表格（对新增的 Rig 工具还要更新 catalog 账本），使覆盖结论可
// 复现，而不是只停留在文字说明。
package audit

import (
	"fmt"
	"maps"
	"slices"

	"strings"
)

// Builtin 是参考实现中的一个面向模型的工具。
type Builtin struct {
	// Name 是参考 agent 暴露给模型的工具名。
	Name string
	// Package 是注册该工具的上游包（或配置行）。
	Package string
}

// Reference 是参考 agent 内置的面向模型工具目录，从已安装的
// @deepseek-ai/dsh 0.2.1-rc.2 包中提取（每个条目对应一次 defineTool 注册；
// subagent_fork 来自 dsh-base/cordis.patch.yml 的 toolName 覆盖）。可选的
// profile 叠加项（plugin_manager、plugin_market）也包含在内，因为它们是同一
// 发行版有文档的内置项，而不是第三方扩展。
var Reference = []Builtin{
	{Name: "ask_user_question", Package: "dsh-tool-ask-user"},
	{Name: "bash", Package: "dsh-tool-bash"},
	{Name: "cordis_inspect_list", Package: "dsh-tool-cordis"},
	{Name: "cordis_inspect_query", Package: "dsh-tool-cordis"},
	{Name: "create_goal", Package: "dsh-tool-goal"},
	{Name: "create_worktree", Package: "dsh-experimental-tool-worktree"},
	{Name: "edit", Package: "dsh-tool-fs"},
	{Name: "exit_plan_mode", Package: "dsh-plan-mode"},
	{Name: "get_goal", Package: "dsh-tool-goal"},
	{Name: "glob", Package: "dsh-tool-fs-search"},
	{Name: "grep", Package: "dsh-tool-fs-search"},
	{Name: "interrupt_agent", Package: "dsh-tool-subagent-control"},
	{Name: "job_kill", Package: "dsh-tool-jobs"},
	{Name: "job_list", Package: "dsh-tool-jobs"},
	{Name: "job_output", Package: "dsh-tool-jobs"},
	{Name: "list_agents", Package: "dsh-tool-subagent-control"},
	{Name: "list_mcp_resource_templates", Package: "dsh-mcp-resources"},
	{Name: "list_mcp_resources", Package: "dsh-mcp-resources"},
	{Name: "list_subagent_models", Package: "dsh-tool-subagent"},
	{Name: "load_workspace_dependencies", Package: "dsh-tool-workspace-dependencies"},
	{Name: "plugin_manager", Package: "dsh-plugin-manager"},
	{Name: "plugin_market", Package: "dsh-plugin-market"},
	{Name: "present", Package: "dsh-tool-present"},
	{Name: "pwsh", Package: "dsh-tool-pwsh"},
	{Name: "ralph", Package: "dsh-tool-ralph"},
	{Name: "read", Package: "dsh-tool-fs"},
	{Name: "read_image", Package: "dsh-tool-fs"},
	{Name: "read_mcp_resource", Package: "dsh-mcp-resources"},
	{Name: "schedule_create", Package: "dsh-tool-schedule"},
	{Name: "schedule_delete", Package: "dsh-tool-schedule"},
	{Name: "schedule_list", Package: "dsh-tool-schedule"},
	{Name: "run_code", Package: "dsh-ptc-runtime-node (PTC preset)"},
	{Name: "schedule_update", Package: "dsh-tool-schedule"},
	{Name: "send_message", Package: "dsh-tool-subagent-control"},
	{Name: "session_event_read", Package: "dsh-tool-session-query"},
	{Name: "session_event_search", Package: "dsh-tool-session-query"},
	{Name: "session_event_trace", Package: "dsh-tool-session-query"},
	{Name: "session_search", Package: "dsh-tool-session-query"},
	{Name: "session_trace", Package: "dsh-tool-session-query"},
	{Name: "skill", Package: "dsh-tool-skill"},
	{Name: "spawn_teammate", Package: "dsh-experimental-tool-agent-team"},
	{Name: "str_replace_editor", Package: "dsh-tool-str-replace-editor"},
	{Name: "structured_output", Package: "dsh-subagent (structured children)"},
	{Name: "subagent", Package: "dsh-tool-subagent"},
	{Name: "subagent_fork", Package: "dsh-tool-subagent (dsh-base toolName override)"},
	{Name: "team_task_create", Package: "dsh-experimental-tool-agent-team"},
	{Name: "team_task_get", Package: "dsh-experimental-tool-agent-team"},
	{Name: "team_task_list", Package: "dsh-experimental-tool-agent-team"},
	{Name: "team_task_update", Package: "dsh-experimental-tool-agent-team"},
	{Name: "terminal_close", Package: "dsh-tool-terminal"},
	{Name: "terminal_list", Package: "dsh-tool-terminal"},
	{Name: "terminal_open", Package: "dsh-tool-terminal"},
	{Name: "terminal_read", Package: "dsh-tool-terminal"},
	{Name: "terminal_send", Package: "dsh-tool-terminal"},
	{Name: "terminal_signal", Package: "dsh-tool-terminal"},
	{Name: "todo_write", Package: "dsh-tool-todo"},
	{Name: "update_goal", Package: "dsh-tool-goal"},
	{Name: "wait_agent", Package: "dsh-experimental-tool-agent-team"},
	{Name: "web_fetch", Package: "dsh-tool-web"},
	{Name: "web_search", Package: "dsh-tool-web"},
	{Name: "workflow", Package: "dsh-tool-workflow"},
	{Name: "working_directory", Package: "dsh-tool-working-directory"},
	{Name: "write", Package: "dsh-tool-fs"},
}

// Mapping 把一个参考工具关联到覆盖它的 Rig 工具。
type Mapping struct {
	// Reference 是参考工具名。
	Reference string
	// Rig 是覆盖它的 Rig 工具名（modern_test.go 断言的静态名字），MCP 工具则是
	// server-bound 名字。
	Rig string
	// Note 记录模型/宿主应当知道的语义差异。
	Note string
}

// Mappings 是审计的覆盖表。每个参考工具要么出现在这里，要么出现在
// DocumentedGaps 中，绝不会两者都有；Unmapped 会强制这一点。
var Mappings = []Mapping{
	{Reference: "ask_user_question", Rig: "ask_user", Note: "partial: single question/options only (no multi-question or multi-select); Rig returns a readable error when no interactive UI is connected"},
	{Reference: "bash", Rig: "bash", Note: "sandbox.Policy command allowlist + approval gate"},
	{Reference: "create_goal", Rig: "create_goal", Note: "DSH three-tool goal surface; revision CAS via optional hosttools.GoalRevisionStore; legacy goal remains"},
	{Reference: "get_goal", Rig: "get_goal", Note: "DSH three-tool goal surface; legacy goal action=get remains"},
	{Reference: "update_goal", Rig: "update_goal", Note: "DSH revision-CAS update (edit/pause/resume/complete/blocked); legacy goal action=update remains"},
	{Reference: "create_worktree", Rig: "enter_worktree", Note: "Rig additionally exposes exit_worktree and list_worktrees (superset of the DSH model-facing worktree surface)"},
	{Reference: "edit", Rig: "edit_file", Note: "Rig also exposes str_replace_editor for structured edits"},
	{Reference: "read", Rig: "read_file"},
	{Reference: "write", Rig: "write_file"},
	{Reference: "exit_plan_mode", Rig: "exit_plan_mode", Note: "Rig's plan_review submits the plan under review; exit_plan_mode applies approve/cancel/status"},
	{Reference: "glob", Rig: "glob", Note: "accepts root and DSH-compatible path aliases"},
	{Reference: "grep", Rig: "grep"},
	{Reference: "read_image", Rig: "read_image", Note: "multimodal rig.ToolResult.Parts"},
	{Reference: "present", Rig: "present", Note: "DSH-compatible file deliverables (rig.FilePresenter); the former artifact tool is now present_artifact"},
	{Reference: "pwsh", Rig: "pwsh"},
	{Reference: "ralph", Rig: "ralph"},
	{Reference: "skill", Rig: "skill", Note: "name-only calls load the skill (DSH-compatible); action=list/search remain as extensions"},
	{Reference: "str_replace_editor", Rig: "str_replace_editor"},
	{Reference: "subagent", Rig: "subagent", Note: "DSH async semantics: returns a target id immediately; completion notices via rig.NoticeSource; send_message/list_agents/wait_agent control it"},
	{Reference: "structured_output", Rig: "subagent", Note: "v0.8 child-context tool: agent.NewRunner exposes structured_output while a SubagentSpec.OutputSchema is set and returns the captured object in SubagentResult.Structured; it is not a standalone static tool, so it does not add its own Modern name"},
	{Reference: "subagent_fork", Rig: "subagent_fork", Note: "DSH async fork instance: SubagentSpec.Fork seeds the child with a copy of the parent session's completed history; completion notices via rig.NoticeSource"},
	{Reference: "workflow", Rig: "workflow", Note: "Rig implementation is Go-native declarative steps (sequential/parallel, {{prev}}/{{steps.name}} templating); no embedded JavaScript runtime"},
	{Reference: "list_subagent_models", Rig: "list_subagent_models", Note: "legacy-name tool returning the configured model routes"},
	{Reference: "interrupt_agent", Rig: "interrupt_agent", Note: "legacy team alias for team_interrupt"},
	{Reference: "send_message", Rig: "send_message", Note: "DSH-compatible non-blocking send: accepts the message and returns {sent:true}; read the reply with team_last/team_wait"},
	{Reference: "list_agents", Rig: "list_agents", Note: "DSH-compatible name added; team_list/list_teams remain aliases"},
	{Reference: "wait_agent", Rig: "wait_agent", Note: "DSH-compatible next-change waiter added; team_wait waits on one target"},
	{Reference: "spawn_teammate", Rig: "spawn_teammate", Note: "DSH-compatible name added; team_spawn/spawn_team remain aliases"},
	{Reference: "team_task_create", Rig: "team_task_create"},
	{Reference: "team_task_get", Rig: "team_task_get"},
	{Reference: "team_task_list", Rig: "team_task_list"},
	{Reference: "team_task_update", Rig: "team_task_update"},
	{Reference: "job_output", Rig: "job_output"},
	{Reference: "job_kill", Rig: "job_kill"},
	{Reference: "job_list", Rig: "job_list"},
	{Reference: "terminal_open", Rig: "terminal_open"},
	{Reference: "terminal_send", Rig: "terminal_send"},
	{Reference: "terminal_read", Rig: "terminal_read"},
	{Reference: "terminal_signal", Rig: "terminal_signal"},
	{Reference: "terminal_close", Rig: "terminal_close"},
	{Reference: "terminal_list", Rig: "terminal_list"},
	{Reference: "todo_write", Rig: "todo_write", Note: "DSH-compatible items accept {content,status} without id (ids are generated); legacy todo alias registered too"},
	{Reference: "schedule_create", Rig: "schedule_create"},
	{Reference: "schedule_list", Rig: "schedule_list"},
	{Reference: "schedule_update", Rig: "schedule_update"},
	{Reference: "schedule_delete", Rig: "schedule_delete"},
	{Reference: "session_search", Rig: "session_search"},
	{Reference: "session_trace", Rig: "session_trace"},
	{Reference: "session_event_read", Rig: "session_event_read"},
	{Reference: "session_event_search", Rig: "session_event_search"},
	{Reference: "session_event_trace", Rig: "session_event_trace"},
	{Reference: "web_search", Rig: "web_search", Note: "accepts singular query and DSH-compatible queries[] (1-4)"},
	{Reference: "web_fetch", Rig: "web_fetch"},
	{Reference: "working_directory", Rig: "working_directory"},
	{Reference: "list_mcp_resource_templates", Rig: "mcp_list_resource_templates", Note: "v0.8 server-bound: present only when an MCP client is configured, so excluded from the static Modern gate"},
	{Reference: "list_mcp_resources", Rig: "mcp_list_resources", Note: "server-bound: present only when an MCP client is configured, so excluded from the static Modern gate"},
	{Reference: "read_mcp_resource", Rig: "mcp_read_resource", Note: "server-bound, like mcp_list_resources"},
}

// Gap 是 Rig 没有对应实现的一项参考能力。
type Gap struct {
	// Reference 是参考工具名。
	Reference string
	// Category 说明该能力在纯工具集之外属于哪里："kernel-bound"（拥有模块
	// 运行时的产品）或 "host-bound"（进程/产品关注点：启动策略、插件管理、代码
	// 执行运行时）。类别是稳定的审计词汇。
	Category string
	// Reason 说明 Rig 为什么不提供它。
	Reason string
	// Alternative 是模型或宿主应当改用的方案。
	Alternative string
}

// Gap 类别。
const (
	// CategoryKernelBound 标记需要 SDK 不包含的模块运行时、由装配内核的产品
	// 提供的能力。
	CategoryKernelBound = "kernel-bound"
	// CategoryHostBound 标记由进程/产品宿主拥有的能力（启动策略、运行时插件
	// 管理、代码执行）。
	CategoryHostBound = "host-bound"
)

// DocumentedGaps 是审计刻意省略的能力。每一项都属于完整的 agent 宿主/产品，
// 而不是纯工具集 SDK。
var DocumentedGaps = []Gap{
	{
		Reference:   "cordis_inspect_list",
		Category:    CategoryKernelBound,
		Reason:      "Rig is a pure toolset SDK: it contains no module runtime or Cordis plugin tree, so there is no generic host registry to inspect. Module assembly and dependency injection belong to the product (ZeoKeel uses zeokeel-kernel).",
		Alternative: "Rig's inspect tool reports agent-visible state (sessions, event statistics, workspace); products that own a module runtime should expose their own host-inspection tool.",
	},
	{
		Reference:   "cordis_inspect_query",
		Category:    CategoryKernelBound,
		Reason:      "Same as cordis_inspect_list: no kernel/plugin registry exists inside the SDK to query.",
		Alternative: "inspect (sessions/stats/workspace) plus product-provided host tools.",
	},
	{
		Reference:   "load_workspace_dependencies",
		Category:    CategoryHostBound,
		Reason:      "Loading workspace dependency instructions is host/product boot policy (which files, which package manager, which trust boundary). Rig deliberately keeps boot policy out of the toolset.",
		Alternative: "read_file the dependency file(s) and bash to install; a product can wrap this as a task-specific tool.",
	},
	{
		Reference:   "plugin_manager",
		Category:    CategoryHostBound,
		Reason:      "DSH's runtime plugin manager is part of its host runtime. Rig composes tools at compile time; there is no runtime plugin installer/loader to manage.",
		Alternative: "Go modules compiled into the product (tools/full is the aggregation point) or MCP servers for process-external capabilities.",
	},
	{
		Reference:   "plugin_market",
		Category:    CategoryHostBound,
		Reason:      "A plugin marketplace is a product/UI feature backed by the runtime plugin manager; the SDK has neither.",
		Alternative: "Product-side feature; external capability listings can be fetched with web_fetch, and runnable capabilities are MCP servers or compiled-in modules.",
	},
	{
		Reference:   "run_code",
		Category:    CategoryHostBound,
		Reason:      "DSH's programmatic tool-calling (PTC) preset exposes a run_code tool that executes model-authored TypeScript against the tool API in an isolated runtime. Rig has no code-execution runtime and deliberately stays a plain Go toolset; adding one would be a new sandbox/runtime design decision, not a tool alias.",
		Alternative: "kit.Config.PTC enables an opt-in run_code tool that executes model-authored Python 3/Node.js against the tool API in a system-installed runtime, with every inner call checked by the configured rig.Gate (this is a kit extension, not a static full.All tool). workflow is Rig's Go-native declarative orchestration (no embedded JavaScript engine).",
	},
}

// ServerBound 列出只有在外部 server 或 client（MCP、旧版 LSP）存在时才存在的
// Rig 工具，因此它们不属于静态门禁。配置之后，它们仍是其参考对应物的正当覆盖。
var ServerBound = []string{
	"lsp",
	"lsp_hover",
	"lsp_definition",
	"lsp_diagnostics",
	"lsp_symbols",
	"mcp_get_prompt",
	"mcp_list_prompts",
	"mcp_list_resource_templates",
	"mcp_list_resources",
	"mcp_read_resource",
}

// Dynamic 记录名字在运行时生成、而不是静态注册的参考工具。Rig 匹配同一命名
// 方案，因此不为它们断言静态名字。
var Dynamic = []struct {
	Reference string
	Rig       string
	Note      string
}{
	{
		Reference: "mcp__<server>__<tool>",
		Rig:       "mcp__<server>__<tool>",
		Note:      "DSH registers one tool per MCP server tool; Rig's tools/mcp Tools/NewTools use the same mcp__<server>__<tool> scheme (tools/mcp.DefaultPrefix).",
	},
}

// ProductRequired 列出并不来自 DSH 参考、但产品兼容面要求存在的现代工具面
// 名字（为现有 agent/提示词保留的旧版 ZeoKeel 工具名）。它们与已映射名字一样
// 受门禁约束。
var ProductRequired = []string{
	"auto_review",
	"code_review",
}

// Modern 是本审计所要求、已排序去重的静态 Rig 工具名集合（已映射的参考工具
// 加上 ProductRequired，再减去 server-bound 名字）。modern_test.go 会针对
// tools/full.All 断言它。
var Modern = modernNames()

func modernNames() []string {
	seen := make(map[string]bool, len(Mappings)+len(ProductRequired))
	for _, mapping := range Mappings {
		if mapping.Rig == "" || isServerBound(mapping.Rig) {
			continue
		}
		seen[mapping.Rig] = true
	}
	for _, name := range ProductRequired {
		seen[name] = true
	}
	return slices.Sorted(maps.Keys(seen))
}

func isServerBound(name string) bool {
	return slices.Contains(ServerBound, name)
}

// Missing 返回 available 中缺失的 Modern 工具名，已排序。
func Missing(available []string) []string {
	present := make(map[string]bool, len(available))
	for _, name := range available {
		present[strings.TrimSpace(name)] = true
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

// Unmapped 返回既不在 Mappings 也不在 DocumentedGaps 中的参考工具。结果非空
// 表示审计表格不完整。
func Unmapped() []string {
	accounted := make(map[string]bool, len(Mappings)+len(DocumentedGaps))
	for _, mapping := range Mappings {
		accounted[mapping.Reference] = true
	}
	for _, gap := range DocumentedGaps {
		accounted[gap.Reference] = true
	}
	var unmapped []string
	for _, builtin := range Reference {
		if !accounted[builtin.Name] {
			unmapped = append(unmapped, builtin.Name)
		}
	}
	slices.Sort(unmapped)
	return unmapped
}

// Coverage 汇总一次针对可用工具名集合的审计运行。
type Coverage struct {
	// Reference 是参考工具数量。
	Reference int
	// Mapped 是有 Rig 对应实现的参考工具数量。
	Mapped int
	// Gaps 是已记录缺口的数量。
	Gaps int
	// Modern 是审计要求的静态 Rig 名字数量。
	Modern int
	// Missing 列出缺失的必需静态名字。
	Missing []string
}

// OK 报告每个必需的现代工具是否都可用。
func (c Coverage) OK() bool { return len(c.Missing) == 0 }

// String 为测试日志与发布检查渲染一行摘要。
func (c Coverage) String() string {
	return fmt.Sprintf("reference=%d mapped=%d gaps=%d modern=%d missing=%d",
		c.Reference, c.Mapped, c.Gaps, c.Modern, len(c.Missing))
}

// Check 把可用工具名与审计表格进行比较。
func Check(available []string) Coverage {
	return Coverage{
		Reference: len(Reference),
		Mapped:    len(Mappings),
		Gaps:      len(DocumentedGaps),
		Modern:    len(Modern),
		Missing:   Missing(available),
	}
}
