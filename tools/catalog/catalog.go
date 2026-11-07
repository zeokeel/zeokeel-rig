// Package catalog is the ZeoKeel Rig tool coverage ledger.
//
// It records every tool name the legacy ZeoKeel agent exposed and classifies the
// ones that only exist when an external dependency is configured (an MCP server
// or a language server). The coverage test in the tools package asserts that
// every static name below is produced by tools.All, so adding a tool without
// wiring it into the SDK, or dropping one, fails CI.
//
// Keep this list in sync with docs/TOOLS.md: it is the "is anything missing?"
// checklist that every release runs.
package catalog

import (
	"fmt"
	"slices"

	"strings"
)

// Static is every tool name a headless build must be able to construct with no
// external servers or UI: the legacy surface plus the modern additions (skills,
// team task board, review). Every name here must be produced by
// tools/full.All, and every tool it produces must be listed here.
var Static = []string{
	// filesystem / editor / image
	"read_file", "write_file", "edit_file", "str_replace_editor",
	"list_dir", "glob", "grep", "read_image",
	// shell / processes
	"bash", "pwsh",
	"terminal_open", "terminal_send", "terminal_read", "terminal_signal",
	"terminal_close", "terminal_list",
	"job_start", "job_output", "job_kill", "job_list", "job_wait",
	// web
	"web_fetch", "web_search",
	// session / diagnostics
	"session_search", "session_trace",
	"session_event_read", "session_event_search", "session_event_trace",
	"inspect", "report",
	// host interaction / planning / goals / settings
	"ask_user", "present", "present_artifact", "plan_review", "exit_plan_mode", "goal",
	"get_goal", "create_goal", "update_goal",
	"settings_get", "settings_set",
	// memory / scheduling / worktrees
	"memory",
	"schedule_create", "schedule_list", "schedule_update", "schedule_delete",
	"enter_worktree", "exit_worktree", "list_worktrees",
	// orchestration
	"subagent", "subagent_fork", "spawn_teammate", "list_agents", "wait_agent", "workflow", "ralph",
	"team_spawn", "team_list", "team_send", "team_wait", "team_last",
	"team_close", "team_interrupt",
	"spawn_team", "list_teams", "team_status", "close_team", "wait_team",
	"send_message", "interrupt_agent", "list_subagent_models",
	// misc
	"working_directory", "todo", "todo_write",
	// modern-surface additions (v0.7/v0.8): skills, team task board, review
	"skill", "code_review", "auto_review",
	"team_task_create", "team_task_list", "team_task_get", "team_task_update",
}

// ServerBound tools exist only while an external server/client is configured and
// therefore cannot be constructed by the static coverage check. They are still
// part of the catalog: the constructors and their tests cover them.
var ServerBound = []string{
	// MCP (stdio or streamable HTTP): one client per server
	"mcp_list_resources", "mcp_read_resource", "mcp_list_resource_templates",
	"mcp_list_prompts", "mcp_get_prompt",
	// legacy single LSP tool (the lsp_* tools are static constructors)
	"lsp",
}

// Modern is the tool surface a current-generation coding agent is expected to
// expose (derived from the DeepSeek Harness / ZeoKeel agent tool set). It is a
// superset-style audit: names may overlap Legacy, and additions that came from
// the modern surface live here. AuditModern reports any that a build is missng.
var Modern = []string{
	// interaction / files / shell
	"ask_user", "present", "read_file", "write_file", "edit_file",
	"read_image", "bash", "pwsh", "glob", "grep", "working_directory",
	"todo_write", "todo",
	// goals / planning
	"goal", "get_goal", "create_goal", "update_goal", "plan_review", "exit_plan_mode",
	// skills
	"skill",
	// orchestration
	"subagent", "subagent_fork", "workflow", "ralph",
	"team_spawn", "team_list", "team_send", "team_wait", "team_close",
	"team_interrupt",
	"team_task_create", "team_task_list", "team_task_get", "team_task_update",
	// review
	"code_review", "auto_review",
	// scheduling / worktrees
	"schedule_create", "schedule_list", "schedule_update", "schedule_delete",
	"enter_worktree", "exit_worktree", "list_worktrees",
	// session / diagnostics
	"session_search", "session_trace", "session_event_read",
	"session_event_search", "session_event_trace", "inspect", "report",
	// web / mcp / lsp
	"web_fetch", "web_search",
	// processes
	"terminal_open", "terminal_send", "terminal_read", "terminal_signal",
	"terminal_close", "terminal_list",
	"job_start", "job_output", "job_kill", "job_list",
	// memory / settings
	"memory", "settings_get", "settings_set",
}

// DocumentedGaps are modern-surface capabilities intentionally not exposed as a
// static tool name yet, with the reason tracked in docs/COVERAGE.md. The
// subagent-context tool `structured_output` is a child-agent capability
// (SubagentSpec.OutputSchema) rather than a standalone static name.
var DocumentedGaps []string

// AuditModern reports which modern tool names a build is missing.
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

// Dynamic are name patterns produced at runtime from a server's own tool list.
var Dynamic = []string{
	"mcp__<server>__<tool>",
}

// Legacy is the exact tool surface of the previous ZeoKeel agent (extracted
// from old/internal/tools and runtime settings tools). AuditLegacy verifies that
// nothing the product used to expose went missing while Rig was extracted; the
// release checklist runs it together with Check.
//
// Deprecated: the legacy tool-name aliases in this list (for example
// "spawn_team", "list_teams", "wait_team", "close_team", "send_message",
// "interrupt_agent", "todo") are kept for compatibility with pre-v0.7 prompts
// and integrations, and are deprecated as of v0.9.1 in favor of the modern
// names (team_spawn, team_list, team_wait, team_close, team_send,
// team_interrupt, todo_write, ...). They keep working through v0.9.x and are
// planned for removal in v0.10.0; see CHANGELOG.md and docs/VERSIONING.md.
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

// AuditLegacy reports which legacy tool names are still missing from a build
// (server-bound names are reported separately by the caller).
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

// Report is the result of comparing a tool set with the catalog.
type Report struct {
	Missing []string
	Extra   []string
}

// OK reports whether every static tool is present.
func (r Report) OK() bool { return len(r.Missing) == 0 }

// String renders a compact summary (used by tests and release checks).
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

// Check compares the tool names a build produced with Static.
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
