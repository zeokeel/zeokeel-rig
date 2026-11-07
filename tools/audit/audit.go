// Package audit is ZeoKeel Rig's reproducible "modern agent tool surface"
// coverage audit.
//
// It records the model-facing tool catalog of the reference modern agent
// (DeepSeek Harness, @deepseek-ai/dsh 0.2.1-rc.2 — the installation this audit
// was performed against), maps each reference tool to the Rig tool that covers
// it, and documents the capabilities that are deliberately out of scope for a
// pure toolset SDK (DocumentedGaps, grouped as kernel-bound or host-bound).
//
// The audit is enforced by modern_test.go: it builds the complete static Rig
// tool set through tools/full.All and fails when a mapped modern tool is
// missing. Adding a reference tool or a Rig tool means updating the tables here
// (and, for new Rig tools, the catalog ledger), so the coverage claim is
// reproducible instead of prose.
package audit

import (
	"fmt"
	"maps"
	"slices"

	"strings"
)

// Builtin is one model-facing tool of the reference implementation.
type Builtin struct {
	// Name is the tool name the reference agent exposes to the model.
	Name string
	// Package is the upstream package (or configuration row) that registers it.
	Package string
}

// Reference is the reference agent's built-in model-facing tool catalog,
// extracted from the installed @deepseek-ai/dsh 0.2.1-rc.2 packages (one
// defineTool registration per entry; subagent_fork comes from the
// dsh-base/cordis.patch.yml toolName override). Optional profile overlays
// (plugin_manager, plugin_market) are included because they are documented
// built-ins of the same distribution, not third-party extensions.
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

// Mapping links one reference tool to the Rig tool(s) that cover it.
type Mapping struct {
	// Reference is the reference tool name.
	Reference string
	// Rig is the Rig tool name that covers it (the static name asserted by
	// modern_test.go), or a server-bound name for MCP tools.
	Rig string
	// Note records semantic differences the model/host should know.
	Note string
}

// Mappings is the audit's coverage table. Every reference tool appears either
// here or in DocumentedGaps, never both; Unmapped enforces that.
var Mappings = []Mapping{
	{Reference: "ask_user_question", Rig: "ask_user", Note: "partial: single question/options only (no multi-question or multi-select); Rig returns a readable error when no interactive UI is connected"},
	{Reference: "bash", Rig: "bash", Note: "sandbox.Policy command allowlist + approval gate"},
	{Reference: "create_goal", Rig: "goal", Note: "goal action=set"},
	{Reference: "get_goal", Rig: "goal", Note: "goal action=get"},
	{Reference: "update_goal", Rig: "goal", Note: "goal action=update (complete/blocked/pause/resume/edit) when the GoalStore implements rig.GoalUpdater"},
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
	{Reference: "subagent", Rig: "subagent", Note: "fresh child context; DSH `subagent` (spawn provider) semantics"},
	{Reference: "structured_output", Rig: "subagent", Note: "v0.8 child-context tool: agent.NewRunner exposes structured_output while a SubagentSpec.OutputSchema is set and returns the captured object in SubagentResult.Structured; it is not a standalone static tool, so it does not add its own Modern name"},
	{Reference: "subagent_fork", Rig: "subagent_fork", Note: "v0.8 fork semantics: SubagentSpec.Fork seeds the child with a copy of the parent session's completed history (runner configured with RunnerOptions.ParentSessionID)"},
	{Reference: "workflow", Rig: "workflow"},
	{Reference: "list_subagent_models", Rig: "list_subagent_models", Note: "legacy-name tool returning the configured model routes"},
	{Reference: "interrupt_agent", Rig: "interrupt_agent", Note: "legacy team alias for team_interrupt"},
	{Reference: "send_message", Rig: "send_message", Note: "DSH-compatible non-blocking send: accepts the message and returns {sent:true}; read the reply with team_last/team_wait"},
	{Reference: "list_agents", Rig: "list_teams", Note: "also team_list; legacy list_teams stays registered"},
	{Reference: "wait_agent", Rig: "wait_team", Note: "also team_wait; legacy wait_team stays registered"},
	{Reference: "spawn_teammate", Rig: "spawn_team", Note: "also team_spawn; legacy spawn_team stays registered"},
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

// Gap is a reference capability with no Rig equivalent.
type Gap struct {
	// Reference is the reference tool name.
	Reference string
	// Category says where the capability belongs outside the pure toolset:
	// "kernel-bound" (a product that owns a module runtime) or "host-bound"
	// (a process/product concern: boot policy, plugin management, a code
	// execution runtime). The category is stable audit vocabulary.
	Category string
	// Reason states why Rig does not provide it.
	Reason string
	// Alternative is what a model or host should use instead.
	Alternative string
}

// Gap categories.
const (
	// CategoryKernelBound marks capabilities that need a module runtime the SDK
	// does not contain; products that assemble a kernel provide them.
	CategoryKernelBound = "kernel-bound"
	// CategoryHostBound marks capabilities owned by the process/product host
	// (boot policy, runtime plugin management, code execution).
	CategoryHostBound = "host-bound"
)

// DocumentedGaps are the deliberate omissions of the audit. Each one is a
// capability of a full agent host/product rather than of a pure toolset SDK.
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
		Alternative: "workflow for multi-step composition (templates with {{prev}}/{{steps.name}}) and bash/terminal for scripting; a product can add a run_code tool for its own trusted runtime.",
	},
}

// ServerBound lists Rig tools that exist only next to an external server or
// client (MCP, legacy LSP), so they are not part of the static gate. They are
// still legitimate coverage of their reference counterparts when configured.
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

// Dynamic documents reference tools whose names are produced at runtime rather
// than registered statically. Rig matches the naming scheme, so no static name
// is asserted for them.
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

// ProductRequired lists modern-surface names that do not come from the DSH
// reference but are required by the product's compatibility surface (legacy
// ZeoKeel tool names kept for existing agents/prompts). They are gated exactly
// like mapped names.
var ProductRequired = []string{
	"auto_review",
	"code_review",
}

// Modern is the sorted, unique set of static Rig tool names this audit requires
// (mapped reference tools plus ProductRequired, minus server-bound names). It
// is asserted against tools/full.All by modern_test.go.
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

// Missing returns the Modern tool names not present in available, sorted.
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

// Unmapped returns reference tools that appear in neither Mappings nor
// DocumentedGaps. A non-empty result means the audit tables are incomplete.
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

// Coverage summarizes one audit run against an available tool-name set.
type Coverage struct {
	// Reference is the number of reference tools.
	Reference int
	// Mapped is the number of reference tools with a Rig equivalent.
	Mapped int
	// Gaps is the number of documented gaps.
	Gaps int
	// Modern is the number of static Rig names the audit requires.
	Modern int
	// Missing lists the required static names that are absent.
	Missing []string
}

// OK reports whether every required modern tool is available.
func (c Coverage) OK() bool { return len(c.Missing) == 0 }

// String renders one line for test logs and release checks.
func (c Coverage) String() string {
	return fmt.Sprintf("reference=%d mapped=%d gaps=%d modern=%d missing=%d",
		c.Reference, c.Mapped, c.Gaps, c.Modern, len(c.Missing))
}

// Check compares available tool names with the audit tables.
func Check(available []string) Coverage {
	return Coverage{
		Reference: len(Reference),
		Mapped:    len(Mappings),
		Gaps:      len(DocumentedGaps),
		Modern:    len(Modern),
		Missing:   Missing(available),
	}
}
