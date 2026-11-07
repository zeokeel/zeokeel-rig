// Package team 提供进程内的多子代理编排：DSH 兼容的异步委派/团队控制工具
// （spawn_teammate、list_agents、wait_agent、subagent、subagent_fork）、team_* 工具集、
// 旧版兼容别名，以及共享任务板 team_task_*。
//
// # 组装
//
// 工具都是 Manager 之上的薄封装。Manager 接收任意 rig.SubagentRunner；agent 包提供默认的
// 进程内实现（agent.NewRunner），Manager 自身从不启动进程：
//
//	parent := rig.AgentSpec{
//		Provider: prov,
//		Model:    "gpt-4o-mini",
//		Tools:    parentTools, // 包含团队工具，父代理才能编排
//		History:  session.NewMemory(),
//	}
//	// 子代理不得再编排：把团队工具从它们的工具集中排除。
//	excluded := append(append([]string{}, agent.DefaultExcludedTools...), team.ToolNames()...)
//	runner, err := agent.NewRunner(parent, agent.RunnerOptions{
//		MaxDepth:      2, // 默认父代理加一层子代理
//		MaxConcurrent: 4,
//		ExcludedTools: excluded,
//	})
//	if err != nil {
//		return err
//	}
//	manager := team.NewManager(runner)
//	defer manager.CloseAll()
//	registry := tools.NewRegistry(append(parentTools, manager.Tools()...)...)
//
// # 模型可见工具
//
// DSH 兼容的异步委派与控制（DSHTools，tools/full.All 默认使用）：
//
//   - spawn_teammate {name, prompt|task, description?, model?}：在后台创建一名具名队友并
//     立即返回；完成通知会经 rig.NoticeSource 注入父代理的下一步。
//   - list_agents {}：列出所有成员的 target（id）、名称、可用状态与最近结果状态。
//   - wait_agent {timeout_ms?}：等待下一次团队变化（成员运行结束或可用状态改变）；
//     无成员在运行时立即返回 noProgress。
//   - subagent / subagent_fork {prompt, name?, model?, ...}：异步委派一个子代理并立即返回，
//     fork 变体让子代理继承父会话已完成历史。
//
// team_* 工具集（Manager.Tools）：
//
//   - team_spawn {name, prompt, model?, system_prompt?}：启动成员，首次运行在后台进行。
//   - team_list {}：成员 id、名称、状态、uptime。
//   - team_send {id|name, message}：发送后续消息并等待该成员本轮回复。
//   - team_wait {id|name, timeout_ms?}：等待指定成员的当前/首次运行并返回结果。
//   - team_last {id|name}：返回成员最近一次已完成的结果。
//   - team_close {id|name}：停止并移除成员。
//   - team_interrupt {id|name}：停止当前运行但保留成员（状态 interrupted）。
//
// 旧版别名（LegacyTools）：spawn_team、list_teams、team_status、close_team、wait_team、
// send_message、interrupt_agent、list_subagent_models；它们复用同一 Manager 操作与格式化，
// 只是名字与少量参数别名不同（spawn_team 接受 task 作为 prompt 别名等）。
//
// 共享任务板（TaskTools，tools/full.All 用内存实现）：team_task_create {subject,
// description?, owner?, blocked_by?, write_scopes?}、team_task_list {status?, owner?}、
// team_task_get {id}、team_task_update {id, action: claim|complete|reopen|release|edit|
// set_dependencies|reassign, ...}。
//
// # 生命周期与限制
//
// 生成为异步：spawn 立即返回成员 id/名称，子代理的首次运行在后台继续。成员引用先按 id 精确
// 匹配，再按唯一名称匹配；重名时名称引用会报告歧义，必须使用 id。成员状态为 active、
// interrupted（已请求停止、仍在列表）或 closed（已停止并移除）。默认最多 8 个活跃成员
// （WithMaxMembers，被中断的成员在关闭前仍占名额）；team_wait 默认超时 30s、单次上限
// 10 分钟。宿主在 agent 实例销毁时调用 Manager.CloseAll，它会停止所有存活子代理并拒绝后续
// 操作。
//
// Manager 只是单个进程内一个 agent 会话对其子代理的视图：不跨重启持久化团队状态，不跨进程
// 共享成员，也不监管跨进程的 agent 运行时。真实 runner 会在进程内以父代理的
// provider/model/工具配置运行每个子代理。
//
// # 权限
//
// 工具自身不访问文件、网络或 shell，只通过 runner 启动/控制子代理；子代理权限继承父配置，
// 其工具调用仍经过 agent 层 rig.Gate 审批。任务板是内存状态（默认上限 256 条），
// owner 为字符串标识。审批在 agent 层由 rig.Gate 控制。
package team
