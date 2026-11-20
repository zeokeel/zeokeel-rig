package full

import (
	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/memoryvfs"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/sandbox/osrunner"
	"github.com/zeokeel/zeokeel-rig/tools/builtin"
	"github.com/zeokeel/zeokeel-rig/tools/discovery"
	"github.com/zeokeel/zeokeel-rig/tools/editor"
	"github.com/zeokeel/zeokeel-rig/tools/hosttools"
	"github.com/zeokeel/zeokeel-rig/tools/image"
	"github.com/zeokeel/zeokeel-rig/tools/inspect"
	"github.com/zeokeel/zeokeel-rig/tools/jobs"
	"github.com/zeokeel/zeokeel-rig/tools/lsp"
	"github.com/zeokeel/zeokeel-rig/tools/mcp"
	"github.com/zeokeel/zeokeel-rig/tools/memory"
	"github.com/zeokeel/zeokeel-rig/tools/misc"
	"github.com/zeokeel/zeokeel-rig/tools/patch"
	"github.com/zeokeel/zeokeel-rig/tools/ralph"
	"github.com/zeokeel/zeokeel-rig/tools/review"
	"github.com/zeokeel/zeokeel-rig/tools/sandboxgrant"
	"github.com/zeokeel/zeokeel-rig/tools/sandboxstatus"
	"github.com/zeokeel/zeokeel-rig/tools/schedule"
	"github.com/zeokeel/zeokeel-rig/tools/search"
	"github.com/zeokeel/zeokeel-rig/tools/session"
	"github.com/zeokeel/zeokeel-rig/tools/skill"
	"github.com/zeokeel/zeokeel-rig/tools/team"
	"github.com/zeokeel/zeokeel-rig/tools/terminal"
	"github.com/zeokeel/zeokeel-rig/tools/undo"
	"github.com/zeokeel/zeokeel-rig/tools/web"
	"github.com/zeokeel/zeokeel-rig/tools/workflow"
	"github.com/zeokeel/zeokeel-rig/tools/worktree"
	"time"
)

// AllOptions 配置完整工具集。Memory、Scheduler 与 TeamTasks 会回退到全新的
// 内存实现。其他依赖是可选的但不会自动兜底：缺少 Host、SessionLog、
// Searcher、Runner 或 Reviewer 时，对应工具仍可构造，但调用时会返回
// “未配置”工具错误。服务端绑定工具（MCP、LSP）只有配置了对应客户端才会
// 包含。
type AllOptions struct {
	// Policy 是文件/shell/terminal/任务工具使用的沙箱策略。为 nil 时表示一个
	// 以 Root 为根、不含 shell 与网络的空策略。
	Policy *sandbox.Policy
	// SandboxRunner 是可选的 OS 沙箱 runner（见 sandbox/osrunner）。非 nil 时
	// 它被注入 bash/pwsh、jobs、terminal 与 worktree 工具：命令 spawn 前先经
	// Runner.Confine 包装，失败即拒绝执行（fail closed）。nil 保持默认行为
	// （进程内策略围栏）。注意它与下面给 subagent/workflow 用的 Runner 是不同
	// 概念。
	SandboxRunner sandbox.Runner
	// Escalator 是逐命令提权的审批通道（见 tools/builtin 的 Escalator）。非 nil 时
	// bash/pwsh 接受 sandbox_permissions:"require_escalated" + justification，并在
	// **人工批准**后让那一条命令跳过 OS 沙箱包装；策略、危险命令判定与审批门禁
	// 一律不变。nil 时该参数保持"composition 不支持"的兼容错误。
	Escalator builtin.Escalator
	// SpillStore 是溢出结果的存储（见 rig.SpillPolicy.Store）。非 nil 时追加
	// spill_read 工具：模型看到的 `ref:` 句柄必须有一条读回路径，否则"结果太大，
	// 已存起来"等于"结果永久丢失"。nil 时不注册该工具（工具集与之前逐字一致）。
	SpillStore rig.SpillStore
	// SandboxGrants 追加两个沙箱可观测性/授权工具：
	//
	//   - sandbox_status：只读诊断，报告当前模式、可写根、保护路径、已授权路径
	//     与剩余授权预算；
	//   - request_sandbox_access：让模型为"某个工具自己的凭据/缓存目录"申请
	//     会话级可写授权。它**不自行审批**——审批由 host 的 rig.Gate 在调用
	//     之前完成，默认 gate 会拒绝它，因此开启本选项必须同时配置审批。
	//
	// 默认 false：不开启时工具集与之前逐字一致。它与 kit.Config.SandboxGrants
	// 语义相同，二者同时开启时按名字去重。
	SandboxGrants bool
	// BackgroundJobs 让宿主注入**自己的**后台任务管理器（tools/jobs.Manager），
	// 而不是让 full 在内部新建一个。语义与 builtin.WithBackgroundJobs 一致：
	// bash 的后台任务、job_* 工具与 terminal 工具共享同一个任务池。
	//
	// 用途：宿主已经有一个共享的 jobs.Manager（或想让 Shutdown 关掉的那一个）。
	// 注入的管理器**保留它自己的 policy/runner**（full 不会改写别人的管理器）；
	// 为 nil 时 full 用 Policy/SandboxRunner 新建一个，并通过 Runtime 交出来
	// （见 NewRuntime）。用 All 的调用方仍然拿不到句柄（旧行为，见 All 的注释）。
	BackgroundJobs *jobs.Manager
	// Owner 是这次装配的调用方标识（Runtime 形态下每个 Agent 一个）。job_start
	// 与 terminal_open/terminal_send 用它去解析该调用方自己的沙箱围栏，从而让
	// 一份共享的 jobs.Manager/terminal.Manager 服务多个策略不同的 Agent。
	// 空表示使用管理器的默认 scope（`kit.New` 的单 Agent 路径）。
	Owner string
	// BackgroundTerminals 让宿主注入**自己的**终端会话管理器（tools/terminal），
	// 而不是让 full 在内部新建一个。语义与 BackgroundJobs 一致：PTY 会话句柄
	// 跨工具集/Agent 共享，注入的管理器保留它自己的 policy/runner，且需要由注入
	// 方负责 CloseAll（kit.Runtime 就是这么做的）。
	BackgroundTerminals *terminal.Manager
	// SandboxGrantObserver 是 request_sandbox_access 的审计钩子：每次申请（含
	// 参数错误、被策略拒绝、冗余）都会回调一次。nil 表示不审计。
	SandboxGrantObserver func(rig.ToolCall, sandbox.Grant, error)
	// UndoTracker 是回合快照跟踪器（tools/undo）。宿主在每回合开始前调用
	// Tracker.Begin() 之后，模型就能用 undo 回退该回合的文件改动；为 nil 时
	// full 会自建一个（工具存在但没有任何回合快照，调用会得到可读错误）。
	UndoTracker *undo.Tracker
	// SearchMaxResults / SearchTimeout 调整 search 工具的默认上限；零值用默认。
	SearchMaxResults int
	SearchTimeout    time.Duration
	// PatchMaxBytes 调整 apply_patch 接受的补丁文本上限；零值用默认（1 MiB）。
	PatchMaxBytes int
	// SandboxDenialSink 接收每一次被识别的沙箱拒绝（结构化 Denial：模式、
	// enforcement、被拒路径、可写面、退出码/信号）。它挂在 Policy 上，因此一次
	// 配置覆盖 bash/pwsh/jobs/terminal/worktree/run_code。nil 表示不审计；回调
	// 必须并发安全，且同一次拒绝可能因多个结果视图重复上报。
	SandboxDenialSink func(sandbox.Denial)
	// Root 是 working_directory/inspect 报告的 workspace 根。
	Root string
	// Host 为 ask_user/present/plan_review/exit_plan_mode/goal 提供宿主
	// 实现。
	Host hosttools.Options
	// Artifact 是 present 的**内容产物**入口：非 nil 时等价于
	// Host.Artifact=Artifact（模型给 title+body，宿主渲染成卡片/文档）。
	Artifact rig.Presenter
	// Presenter 是 present 的便捷入口：非 nil 时等价于 Host.FilePresenter=Presenter。
	// 为 nil 时 present 仍然注册，但每次调用返回"宿主没有提供 FilePresenter"。
	Presenter rig.FilePresenter
	// Asker 是 ask_user 的便捷入口：非 nil 时等价于
	// Host.Asker=Asker 且 Host.Interactive=true（"有人在看这个 run"）。
	//
	// 需要在无头环境里保持 ask_user 关闭时，请直接用 Host 并留 Interactive=false。
	Asker rig.Asker
	// Memory 是**历史字段**：为 memory 工具提供扁平存储。nil 表示全新的内存 wiki。
	// 现在推荐用 Wiki（渐进式 wiki 记忆）；两者都设置时 Wiki 优先，Memory 只作为
	// 只读迁移通道（一条记忆 = 一页）。
	Memory rig.MemoryStore
	// Wiki 为 memory 工具提供渐进式 wiki 存储（nil 且 Memory 为 nil 时用进程内实现）。
	Wiki memory.Wiki
	// MemoryStore 为 v2 memory 工具提供上下文后端；非 nil 时优先于 Wiki。
	MemoryStore memoryvfs.Store
	// Scheduler 为 schedule_* 提供调度器；nil 表示全新的内存调度器。
	Scheduler rig.Scheduler
	// SessionLog 为 session_* / inspect 提供会话日志；nil 时这些工具报告
	// 配置错误（kit 会直接把它们从默认工具集里摘掉）。
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
	// MCPPrefix 为 MCP 工具名加前缀（默认 "mcp__"）。mcp 包会先按
	// `mcp.NormalizePrefix` 规范化（去空白、空串用默认值、补齐 `__` 分隔符），
	// 所以调用方按前缀过滤工具名时也用同一个结果。
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

// All 返回给定选项下的全部静态 Rig 工具。它是 catalog 覆盖检查背后的聚合点；
// 由于保持旧签名（只返回 []rig.Tool），它**丢弃**后台任务句柄。
//
// 需要能关停后台任务的宿主请用 NewRuntime：
//
//	rt, err := full.NewRuntime(full.AllOptions{Policy: policy, Root: root})
//	defer rt.Close()
//	reg := tools.NewRegistry(rt.Tools...)
//
// 只想要子集的宿主可以直接调用各个工具包。
//
// 注意：All 保持原有签名（`[]rig.Tool`），因此它**仍然不交出后台句柄**——
// bash/terminal 的后台任务管理器创建后没有关闭路径。需要能关停后台任务的宿主请用
// NewRuntime（见 runtime.go）；kit 已经从 All 切到 NewRuntime。
func All(opts AllOptions) []rig.Tool {
	rt, err := NewRuntime(opts)
	if err != nil {
		// NewRuntime 目前不会失败；这里保持 All 的既有签名（没有 error 返回值），
		// 真出错时返回空集，让覆盖测试/调用方立刻看到"没有工具"，而不是半个工具集。
		return nil
	}
	return rt.Tools
}

// newRuntime 构造工具集与它背后的后台句柄。NewRuntime 是它的公开入口；
// 拆出来只是为了让 All 的注释能说清"句柄被丢弃"这件事。
func newRuntime(opts AllOptions) (Runtime, error) {
	policy := opts.Policy
	if policy == nil {
		policy = sandbox.New(sandbox.WithRoots(opts.Root))
	}
	if opts.SandboxDenialSink != nil {
		policy.SetDenialSink(opts.SandboxDenialSink)
	}
	// 这里曾经调用 sandbox.Policy.AddGitProtection / 保护路径 API 把 `.git`
	// 控制面登记成只读子路径。DSH 对齐后策略层不再有保护路径，`AddGitProtection`
	// 的唯一剩余效果（把 `.git` 声明成带只读子路径的可降级可写根）恰好是"半开"的
	// 那一半：`.git/config` 在进程内仍不可写，与 DSH 的"只做可写根围栏"不一致。
	// 因此这里不再登记任何保护/只读子路径，`.git` 完全遵循可写根语义。

	jobOpts := []jobs.Option{jobs.WithPolicy(policy)}
	if opts.SandboxRunner != nil {
		jobOpts = append(jobOpts, jobs.WithRunner(opts.SandboxRunner))
	}
	// 注入优先：宿主可能已经持有（或被多个工具集共享）一个后台任务管理器。
	background := opts.BackgroundJobs
	if background == nil {
		background = jobs.NewManager(jobOpts...)
	}
	var out []rig.Tool
	builtinOpts := []builtin.Option{builtin.WithBackgroundJobs(background)}
	if opts.SandboxRunner != nil {
		builtinOpts = append(builtinOpts, builtin.WithRunner(opts.SandboxRunner))
	}
	if opts.Escalator != nil {
		builtinOpts = append(builtinOpts, builtin.WithEscalator(opts.Escalator))
	}
	// 授权工具是否可用要告诉 shell 工具：沙箱拒绝的处置建议据此生成，绝不指向
	// 本组合里不存在的工具。
	builtinOpts = append(builtinOpts, builtin.WithGrantTool(opts.SandboxGrants))
	out = append(out, builtin.NewWithPolicyOptions(policy, builtinOpts...)...)
	out = append(out,
		builtin.NewPwshWithPolicyOptions(policy, builtinOpts...),
		editor.New(policy),
		image.New(policy),
	)

	// 工作区级工具：apply_patch（原子多文件补丁）、search（模糊+BM25）、undo（回合撤销）。
	// 三者都以"工作区根"为边界，root 缺失时退回策略主根。
	workspaceRoot := opts.Root
	if workspaceRoot == "" && len(policy.Roots()) > 0 {
		workspaceRoot = policy.Roots()[0]
	}
	if workspaceRoot != "" {
		// apply_patch 与其它写工具走**同一套策略**：只读模式、deny glob、保护区路径
		// 都必须在工具内生效（否则它就是沙箱旁路）。
		patchOpts := []patch.Option{patch.WithPolicy(policy)}
		if opts.PatchMaxBytes > 0 {
			patchOpts = append(patchOpts, patch.WithMaxPatchBytes(opts.PatchMaxBytes))
		}
		out = append(out, patch.New(workspaceRoot, patchOpts...))

		var searchOpts []search.Option
		if opts.SearchMaxResults > 0 {
			searchOpts = append(searchOpts, search.WithMaxResults(opts.SearchMaxResults))
		}
		if opts.SearchTimeout > 0 {
			searchOpts = append(searchOpts, search.WithTimeout(opts.SearchTimeout))
		}
		// 隐藏路径与文件工具用**同一份判据**：search 走自己的遍历，不经过
		// Policy.OpenPath，所以必须显式把判定接进去，否则"设置里隐藏了 .env"
		// 之后内容检索照样能把它读出来。
		if policy != nil {
			searchOpts = append(searchOpts, search.WithHiddenFunc(policy.Hidden))
		}
		out = append(out, search.New(workspaceRoot, searchOpts...))

		tracker := opts.UndoTracker
		if tracker == nil {
			tracker = undo.NewTracker(workspaceRoot, undo.Options{})
		}
		out = append(out, undo.New(tracker))
	}

	// 终端管理器：注入优先（共享 PTY 池），否则按本工具集的策略新建一个。
	terminals := opts.BackgroundTerminals
	if terminals == nil {
		terminalOpts := []terminal.Option{terminal.WithPolicy(policy), terminal.WithBackgroundJobs(background)}
		if opts.SandboxRunner != nil {
			terminalOpts = append(terminalOpts, terminal.WithRunner(opts.SandboxRunner))
		}
		terminals = terminal.NewManager(terminalOpts...)
	}
	out = append(out, terminals.ToolsWithListFor(opts.Owner)...)
	jobTools := background.ToolsFor(opts.Owner)
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
	// tool_search 是动态工具加载的发现入口。full.All 无法知道"哪些工具没激活"
	// （那是 agent 的按会话状态），因此这里构造的是**未接线**版本：调用会返回
	// 可读错误。kit 会用自己带回调的实例替换它（按名字去重）。
	out = append(out, discovery.New(discovery.Options{}))
	if opts.MemoryStore != nil {
		out = append(out, memory.ToolsWithStore(opts.MemoryStore)...)
	} else {
		out = append(out, memory.Tools(resolveMemoryWiki(opts))...)
	}
	// 溢出结果的读回入口：只有宿主接了 Store 才存在（没有 Store 时模型看到的
	// 是文件路径，read_file 已经能读）。
	if tool := builtin.NewSpillRead(opts.SpillStore); tool != nil {
		out = append(out, tool)
	}
	out = append(out, schedule.Tools(opts.Scheduler)...)
	out = append(out, web.NewFetcher(policy), web.NewSearcher(opts.Searcher))
	host := opts.Host
	if opts.Presenter != nil && host.FilePresenter == nil {
		host.FilePresenter = opts.Presenter
	}
	if opts.Artifact != nil && host.Artifact == nil {
		host.Artifact = opts.Artifact
	}
	if opts.Asker != nil && host.Asker == nil {
		host.Asker = opts.Asker
		// AllOptions.Asker 的语义就是"有交互通道"，因此顺带把 Interactive 打开：
		// 否则 ask_user 会因为 Interactive=false 直接返回 "no interactive UI"，
		// 这正是审计 2C 抓到的"配了 Asker 反而更糟"的陷阱。
		host.Interactive = true
	}
	out = append(out, hosttools.Tools(host)...)
	out = append(out, session.Tools(opts.SessionLog)...)
	out = append(out, inspect.New(inspect.Options{Root: opts.Root, Log: opts.SessionLog}))
	worktreeOpts := []worktree.Option{}
	if opts.SandboxRunner != nil {
		worktreeOpts = append(worktreeOpts, worktree.WithRunner(opts.SandboxRunner))
	}
	out = append(out, worktree.Tools(policy, worktreeOpts...)...)

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
	if len(opts.MCPClients) > 0 {
		// MCP 工具（tools/list）与资源/提示工具都在 mcp 层按"连接 + 原始名"分配名字：
		// 名字带 server 身份、已分配过的名字加确定性的 __2/__3 后缀，因此两个 client
		// 即使声明同一个 ServerName 也不会重名。这里只做一次直白聚合，不再需要任何
		// 装配层的改名兜底。
		servers := make([]mcp.ServerTools, 0, len(opts.MCPClients))
		for _, client := range opts.MCPClients {
			if client != nil {
				servers = append(servers, mcp.ServerTools{Client: client, Resources: true})
			}
		}
		if len(servers) > 0 {
			aggregated, _, aggErr := mcp.AggregateTools(servers, opts.MCPPrefix)
			if aggErr == nil {
				out = append(out, aggregated...)
			} else {
				// 列工具失败时退回逐 client 的既有行为：All 不能返回错误，
				// 聚合失败不该让整个工具集变空。
				for _, server := range servers {
					out = append(out, mcp.Tools(server.Client, opts.MCPPrefix)...)
					out = append(out, mcp.ResourceTools(server.Client, opts.MCPPrefix)...)
				}
			}
		}
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
	// 默认技能库以 AllOptions.Root 为工作区根，而不是进程 cwd：守护进程启动时
	// 二者通常不同，用 cwd 会让项目级技能静默消失。
	// 技能根必须与 patch/search/undo 用同一个 workspaceRoot：只给 Policy 的形态下
	// opts.Root 是空串，退回它会让项目级技能静默消失。
	out = append(out, skill.ToolsWithRoot(workspaceRoot, opts.Skill)...)
	out = append(out, review.Tools(opts.Reviewer)...)

	if opts.SandboxGrants {
		grantOpts := []sandboxgrant.Option{}
		if opts.SandboxGrantObserver != nil {
			grantOpts = append(grantOpts, sandboxgrant.WithObserver(opts.SandboxGrantObserver))
		}
		statusOpts := []sandboxstatus.Option{}
		if opts.SandboxRunner != nil {
			runner := opts.SandboxRunner
			statusOpts = append(statusOpts,
				sandboxstatus.WithRunnerInfo(
					func() (string, sandbox.Enforcement) {
						name, enforcement, _ := sandbox.RunnerStatus(runner)
						return name, enforcement
					}),
				sandboxstatus.WithRuntimeView(func() sandboxstatus.RuntimeView {
					report := osrunner.DescribeRuntime(policy, runner)
					return sandboxstatus.RuntimeView{
						ToolchainCaches:    report.ToolchainCaches,
						MissingCaches:      report.MissingToolchainCaches,
						MaskedSockets:      report.MaskedSockets,
						NetworkCoverage:    report.NetworkCoverage,
						PrivateTmpDisabled: report.PrivateTmpDisabledReason,
					}
				}))
		}
		out = append(out,
			sandboxstatus.New(policy, statusOpts...),
			sandboxgrant.New(policy, grantOpts...),
		)
	}

	return Runtime{Tools: out, jobs: background, terminals: terminals}, nil
}

// resolveMemoryWiki 选择 memory 工具的存储：Wiki 优先；其次把宿主的扁平
// MemoryStore 投影成 wiki（一条记忆 = 一页，只读迁移通道）；最后回落到进程内 wiki。
func resolveMemoryWiki(opts AllOptions) memory.Wiki {
	if opts.Wiki != nil {
		return opts.Wiki
	}
	if opts.Memory != nil {
		return memory.WikiFromStore(opts.Memory)
	}
	return memory.NewMemoryWiki()
}
