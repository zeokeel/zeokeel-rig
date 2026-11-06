// Package full aggregates every static ZeoKeel Rig tool into one slice, for
// hosts that want the complete registry and for the catalog coverage check.
// It is a separate package so that the tools registry package does not import
// the tool implementations (which would create import cycles in their tests).
package full

import (
	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/builtin"
	"github.com/zeokeel/zeokeel-rig/tools/editor"
	"github.com/zeokeel/zeokeel-rig/tools/hosttools"
	"github.com/zeokeel/zeokeel-rig/tools/image"
	"github.com/zeokeel/zeokeel-rig/tools/inspect"
	"github.com/zeokeel/zeokeel-rig/tools/jobs"
	"github.com/zeokeel/zeokeel-rig/tools/lsp"
	"github.com/zeokeel/zeokeel-rig/tools/mcp"
	"github.com/zeokeel/zeokeel-rig/tools/memory"
	"github.com/zeokeel/zeokeel-rig/tools/misc"
	"github.com/zeokeel/zeokeel-rig/tools/ralph"
	"github.com/zeokeel/zeokeel-rig/tools/report"
	"github.com/zeokeel/zeokeel-rig/tools/review"
	"github.com/zeokeel/zeokeel-rig/tools/schedule"
	"github.com/zeokeel/zeokeel-rig/tools/session"
	"github.com/zeokeel/zeokeel-rig/tools/settings"
	"github.com/zeokeel/zeokeel-rig/tools/skill"
	"github.com/zeokeel/zeokeel-rig/tools/subagent"
	"github.com/zeokeel/zeokeel-rig/tools/team"
	"github.com/zeokeel/zeokeel-rig/tools/terminal"
	"github.com/zeokeel/zeokeel-rig/tools/web"
	"github.com/zeokeel/zeokeel-rig/tools/workflow"
	"github.com/zeokeel/zeokeel-rig/tools/worktree"
)

// AllOptions configures the complete tool set. Every dependency is optional:
// missing host interfaces or stores fall back to in-memory/default
// implementations, and a nil Runner produces orchestration tools that report a
// configuration error when called. Server-bound tools (MCP, LSP) are included
// only when their clients are configured.
type AllOptions struct {
	// Policy is the sandbox policy for filesystem/shell/terminal/job tools.
	// Nil means an empty policy rooted at Root without shell or network.
	Policy *sandbox.Policy
	// Root is the workspace root reported by working_directory/inspect/report.
	Root string
	// Settings backs settings_get/settings_set.
	Settings v1.Settings
	// Host backs ask_user/present/plan_review/exit_plan_mode/goal.
	Host hosttools.Options
	// Memory backs the memory tool; nil means a fresh in-memory store.
	Memory v1.MemoryStore
	// Scheduler backs schedule_*; nil means a fresh in-memory scheduler.
	Scheduler v1.Scheduler
	// SessionLog backs session_query/inspect/report; nil degrades them.
	SessionLog v1.SessionLog
	// Searcher backs web_search; nil means the tool reports a configuration error.
	Searcher web.Searcher
	// LSP adds the lsp_* tools and the legacy lsp tool when non-nil.
	LSP          *lsp.Client
	LSPPathToURI func(string) (string, error)
	// MCPClients add the MCP tools/resources/prompts of each server.
	MCPClients []*mcp.Client
	// MCPPrefix prefixes MCP tool names (default "mcp__").
	MCPPrefix string
	// Runner backs subagent/workflow/ralph/team; nil still yields the tools
	// (they report a configuration error when called).
	Runner v1.SubagentRunner
	// Models is the list reported by the legacy list_subagent_models tool.
	Models []string
	// Skill backs the skill tool; nil means the default filesystem store
	// ($ZEOKEEL_SKILLS, <root>/.zeokeel/skills, ~/.zeokeel/skills).
	Skill v1.SkillStore
	// TeamTasks backs the team_task_* tools; nil means a fresh in-memory board.
	TeamTasks v1.TeamTaskManager
	// Reviewer backs code_review/auto_review; nil means tools that report a
	// configuration error (SubagentReviewer wires them to a child agent).
	Reviewer v1.Reviewer
}

// All returns every static Rig tool for the given options. It is the single
// aggregation point behind the catalog coverage check (tools/catalog) and the
// recommended way for a host to build a complete registry:
//
//	reg := tools.NewRegistry(full.All(full.AllOptions{Policy: policy, Root: root})...)
//
// Hosts that want a subset can call the individual tool packages instead.
func All(opts AllOptions) []v1.Tool {
	policy := opts.Policy
	if policy == nil {
		policy = sandbox.New(sandbox.WithRoots(opts.Root))
	}

	var out []v1.Tool
	out = append(out, builtin.NewWithPolicy(policy)...)
	out = append(out,
		builtin.NewPwshWithPolicy(policy),
		editor.New(policy),
		image.New(policy),
	)

	terminals := terminal.NewManager(terminal.WithPolicy(policy))
	out = append(out, terminals.ToolsWithList()...)
	background := jobs.NewManager(jobs.WithPolicy(policy))
	out = append(out, background.Tools()...)

	todos := misc.NewTodoStore()
	out = append(out,
		misc.NewWorkingDirectory(opts.Root),
		misc.NewTodoTool(todos),
		misc.NewTodoAliasTool(todos),
	)
	out = append(out, memory.Tools(opts.Memory)...)
	out = append(out, schedule.Tools(opts.Scheduler)...)
	out = append(out, web.NewFetcher(policy), web.NewSearcher(opts.Searcher))
	out = append(out, hosttools.Tools(opts.Host)...)
	out = append(out, settings.Tools(opts.Settings)...)
	out = append(out, session.Tools(opts.SessionLog)...)
	out = append(out,
		inspect.New(inspect.Options{Root: opts.Root, Log: opts.SessionLog}),
		report.New(report.Options{Log: opts.SessionLog, Policy: policy}),
	)
	out = append(out, worktree.Tools(policy)...)

	if opts.LSP != nil {
		out = append(out, lsp.Tools(opts.LSP, opts.LSPPathToURI)...)
		out = append(out, lsp.LegacyTool(opts.LSP, opts.LSPPathToURI))
	}
	for _, client := range opts.MCPClients {
		if client == nil {
			continue
		}
		out = append(out, mcp.Tools(client, opts.MCPPrefix)...)
		out = append(out, mcp.ResourceTools(client, opts.MCPPrefix)...)
	}

	out = append(out, subagent.Tools(opts.Runner)...)
	out = append(out,
		workflow.New(opts.Runner),
		ralph.New(opts.Runner),
	)
	manager := team.NewManager(opts.Runner)
	out = append(out, manager.Tools()...)
	out = append(out, manager.LegacyTools(opts.Models)...)

	taskBoard := opts.TeamTasks
	if taskBoard == nil {
		taskBoard = team.NewTaskManager()
	}
	out = append(out, team.TaskTools(taskBoard)...)
	out = append(out, skill.Tools(opts.Skill)...)
	out = append(out, review.Tools(opts.Reviewer)...)

	return out
}
