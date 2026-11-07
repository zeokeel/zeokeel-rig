package full

import (
	"github.com/zeokeel/zeokeel-rig"
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
	"github.com/zeokeel/zeokeel-rig/tools/team"
	"github.com/zeokeel/zeokeel-rig/tools/terminal"
	"github.com/zeokeel/zeokeel-rig/tools/web"
	"github.com/zeokeel/zeokeel-rig/tools/workflow"
	"github.com/zeokeel/zeokeel-rig/tools/worktree"
)

// AllOptions 配置完整工具集。Memory、Scheduler 与 TeamTasks 会回退到全新的
// 内存实现。其他依赖是可选的但不会自动兜底：缺少 Host、Settings、SessionLog、
// Searcher、Runner 或 Reviewer 时，对应工具仍可构造，但调用时会返回
// “未配置”工具错误。服务端绑定工具（MCP、LSP）只有配置了对应客户端才会
// 包含。
type AllOptions struct {
	// Policy 是文件/shell/terminal/任务工具使用的沙箱策略。为 nil 时表示一个
	// 以 Root 为根、不含 shell 与网络的空策略。
	Policy *sandbox.Policy
	// Root 是 working_directory/inspect/report 报告的 workspace 根。
	Root string
	// Settings 为 settings_get/settings_set 提供存储。
	Settings rig.Settings
	// Host 为 ask_user/present/plan_review/exit_plan_mode/goal 提供宿主
	// 实现。
	Host hosttools.Options
	// Memory 为 memory 工具提供存储；nil 表示全新的内存存储。
	Memory rig.MemoryStore
	// Scheduler 为 schedule_* 提供调度器；nil 表示全新的内存调度器。
	Scheduler rig.Scheduler
	// SessionLog 为 session_query/inspect/report 提供会话日志；nil 会降级
	// 这些工具。
	SessionLog rig.SessionLog
	// Searcher 为 web_search 提供搜索后端；nil 时工具报告配置错误。
	Searcher web.Searcher
	// LSP 非 nil 时加入 lsp_* 工具与遗留 lsp 工具。
	LSP          *lsp.Client
	LSPPathToURI func(string) (string, error)
	// LSPPathPolicy 在 LSP 工具读取模型提供的路径前进行校验。为 nil 时：
	// 若设置了 Policy 则由 Policy.CheckPath 推导，否则不加限制。
	LSPPathPolicy lsp.PathPolicy
	// MCPClients 加入每个服务器的 MCP 工具/资源/提示。
	MCPClients []*mcp.Client
	// MCPPrefix 为 MCP 工具名加前缀（默认 "mcp__"）。
	MCPPrefix string
	// Runner 为 subagent/workflow/ralph/team 提供执行器；nil 时仍会生成
	// 工具（调用时报告配置错误）。
	Runner rig.SubagentRunner
	// Models 是遗留 list_subagent_models 工具报告的模型列表。
	Models []string
	// Skill 为 skill 工具提供存储；nil 表示默认文件系统存储
	// （$ZEOKEEL_SKILLS、<root>/.zeokeel/skills、~/.zeokeel/skills）。
	Skill rig.SkillStore
	// TeamTasks 为 team_task_* 提供存储；nil 表示全新的内存任务板。
	TeamTasks rig.TeamTaskManager
	// Reviewer 为 code_review/auto_review 提供评审器；nil 时这些工具报告
	// 配置错误（SubagentReviewer 可把它们接到子 agent）。
	Reviewer rig.Reviewer
}

// All 返回给定选项下的全部静态 Rig 工具。它是 catalog 覆盖检查背后的唯一
// 聚合点，也是宿主构建完整 registry 的推荐方式：
//
//	reg := tools.NewRegistry(full.All(full.AllOptions{Policy: policy, Root: root})...)
//
// 只想要子集的宿主可以直接调用各个工具包。
func All(opts AllOptions) []rig.Tool {
	policy := opts.Policy
	if policy == nil {
		policy = sandbox.New(sandbox.WithRoots(opts.Root))
	}

	background := jobs.NewManager(jobs.WithPolicy(policy))
	var out []rig.Tool
	out = append(out, builtin.NewWithPolicyOptions(policy, builtin.WithBackgroundJobs(background))...)
	out = append(out,
		builtin.NewPwshWithPolicyOptions(policy, builtin.WithBackgroundJobs(background)),
		editor.New(policy),
		image.New(policy),
	)

	terminals := terminal.NewManager(terminal.WithPolicy(policy), terminal.WithBackgroundJobs(background))
	out = append(out, terminals.ToolsWithList()...)
	jobTools := background.Tools()
	notices := jobNoticeText(background)
	for _, tool := range jobTools {
		if tool.Name() == "job_output" {
			out = append(out, noticeTool{Tool: tool, drain: notices})
			continue
		}
		out = append(out, tool)
	}

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
		pathPolicy := opts.LSPPathPolicy
		if pathPolicy == nil && opts.Policy != nil {
			policy := opts.Policy
			pathPolicy = policy.CheckPath
		}
		var lspOpts []lsp.ToolOption
		if pathPolicy != nil {
			lspOpts = append(lspOpts, lsp.WithPathPolicy(pathPolicy))
		}
		out = append(out, lsp.Tools(opts.LSP, opts.LSPPathToURI, lspOpts...)...)
		out = append(out, lsp.LegacyTool(opts.LSP, opts.LSPPathToURI, lspOpts...))
	}
	for _, client := range opts.MCPClients {
		if client == nil {
			continue
		}
		out = append(out, mcp.Tools(client, opts.MCPPrefix)...)
		out = append(out, mcp.ResourceTools(client, opts.MCPPrefix)...)
	}

	manager := team.NewManager(opts.Runner)
	out = append(out, team.DSHTools(manager)...)
	out = append(out,
		workflow.New(opts.Runner),
		ralph.New(opts.Runner),
	)
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
