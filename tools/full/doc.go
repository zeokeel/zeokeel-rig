// Package full 把 ZeoKeel Rig 的全部静态工具聚合到一个切片，供想要完整注册表
// 的宿主使用，也用于 catalog 覆盖检查。它单独成包，是为了让 tools registry
// 包不导入各工具实现（否则会在测试中形成导入环）。
//
// # 入口
//
// All(AllOptions) 是唯一入口：它根据选项组装内置文件/shell 工具、编辑器、图片
// 工具、jobs 与 terminal（含二者共享的后台任务管理器）、memory、schedule、
// web、host、settings、session、inspect、report、worktree、LSP/MCP（仅在配置
// 了客户端时）、subagent/workflow/ralph/team 以及 skill/review 等工具，返回
// 可直接交给 tools.NewRegistry 的切片。
//
// # 选项
//
// AllOptions 中：
//
//   - Policy 是文件/shell/terminal/任务工具使用的沙箱策略；nil 时回退为以
//     Root 为根、不含 shell 与网络的空策略。
//   - Memory、Scheduler、TeamTasks 为 nil 时会创建全新的内存实现。
//   - Host、Settings、SessionLog、Searcher、Runner、Reviewer 等依赖可缺失：
//     对应工具仍会构造，但调用时返回“未配置”的工具错误。
//   - LSP/MCP 只有配置了对应客户端才会加入（服务端绑定工具）。
//   - Root 是 working_directory/inspect/report 报告的 workspace 根，也是
//     默认策略的根。
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
//	all := full.All(full.AllOptions{Policy: policy, Root: "/srv/workspace"})
//	registry := tools.NewRegistry(all...)
//
// 工具行为遵循同一套 DSH 权限模式：read-only 禁用写/执行类工具（文件读取与
// 只读工具仍可用），workspace-write（默认）把写操作与 shell 命令限制在
// workspace 与命令允许列表内，danger-full-access 去掉这些围栏（显式拒绝列表
// 仍生效）。full.All 会自动把一个 jobs.Manager 接到 bash/pwsh/terminal 上，
// 并通过 job_output 装饰器把完成通知交给 agent 排空；手工组装时需自行传入
// WithBackgroundJobs。
//
// 只想要部分工具的宿主可以改为直接调用对应工具包；静态工具总数与清单以
// tools/catalog 的覆盖检查（go test ./tools/full）为准。
package full
