// Package full 把 ZeoKeel Rig 的全部静态工具聚合到一个切片，供想要完整注册表
// 的宿主使用，也用于 catalog 覆盖检查。它单独成包，是为了让 tools registry
// 包不导入各工具实现（否则会在测试中形成导入环）。
//
// # 入口
//
// 两个入口：
//
//   - NewRuntime(AllOptions) (Runtime, error) 是**推荐入口**：它除了返回工具切片，
//     还交出 bash/pwsh/terminal 共享的后台任务管理器，宿主才能在退出时统一收尾
//     （Runtime.Close）。
//   - All(AllOptions) []rig.Tool 是保持旧签名的薄封装；它丢弃后台句柄，适合只要
//     一份工具列表的测试或一次性进程。
//
// 它们按选项组装内置文件/shell 工具、编辑器、图片工具、jobs 与 terminal（含二者
// 共享的后台任务管理器）、memory、schedule、web、host 交互（ask_user/present/
// plan/goal）、session、inspect、worktree、LSP/MCP（仅在配置了客户端时）、
// subagent/team/workflow/ralph、skill/review，以及可选的沙箱授权/诊断工具。
//
// # 选项
//
// AllOptions 中：
//
//   - Policy 是文件/shell/terminal/任务工具使用的沙箱策略；nil 时回退为以
//     Root 为根、不含 shell 与网络的空策略。
//   - Root 或带 roots 的 Policy **必须提供**：否则 apply_patch、search、undo
//     不会构造，文件工具全部返回 “no roots configured”。
//   - Memory/Wiki、Scheduler、TeamTasks 为 nil 时会创建全新的内存实现。
//   - Host、SessionLog、Searcher、Runner、Reviewer 等依赖可缺失：对应工具仍会
//     构造，但调用时返回“未配置”的工具错误。需要“缺依赖就不注册”的语义（例如
//     kit 的零配置体检），由调用方自己过滤。
//   - LSP/MCP 只有配置了对应客户端才会加入（服务端绑定工具）；SpillStore 非 nil
//     时追加 spill_read。
//   - SandboxGrants 为 true 时追加 sandbox_status 与 request_sandbox_access
//     （两个工具都不自行审批，是否放行由宿主 Gate 决定）。
//
// # 接线与权限模式
//
// 推荐用法：
//
//	policy := sandbox.New(
//		sandbox.WithRoots("/srv/workspace"),
//		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
//		sandbox.WithCommandAllowlist("git", "go", "ls"),
//	)
//	rt, err := full.NewRuntime(full.AllOptions{Policy: policy, Root: "/srv/workspace"})
//	if err != nil {
//		return err
//	}
//	defer rt.Close()
//	registry := tools.NewRegistry(rt.Tools...)
//
// 工具行为遵循同一权限模式：read-only 禁用写/执行类工具（文件读取与只读工具仍
// 可用），workspace-write（默认）把写操作与 shell 命令限制在 workspace 与命令
// 允许列表内，danger-full-access 去掉这些围栏（显式拒绝列表仍生效）。
// NewRuntime 会自动把一个 jobs.Manager 接到 bash/pwsh/terminal 上，并通过
// job_output 装饰器把完成通知交给 agent 排空；手工组装时需自行传入
// WithBackgroundJobs。
//
// 只想要部分工具的宿主可以改为直接调用对应工具包；静态工具总数与清单以
// tools/catalog 的覆盖检查（go test ./tools/full）为准。
package full
