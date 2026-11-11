// Package jobs 提供后台进程工具：job_start、job_output、job_kill、job_list
// 与 job_wait。
//
// 一个 Manager 拥有某个 agent 会话的全部任务。每个进程型任务是在自己进程组
// 中运行的命令，stdout+stderr 合并写入受限的环形缓冲（默认每个任务 1 MiB）。
// 操作型任务把任意生产者拥有的异步工作（例如一次持久终端发送）置于同一生命
// 周期、输出环、等待/终止控制与完成通知之下。
//
// # 模型可见工具
//
//   - job_start：command（必填）、cwd（可选）、timeout_ms（可选，为正时到点
//     杀死任务）；立即返回任务 id。
//   - job_output：job_id（必填）、wait（可选）、timeout_ms（可选，等待上限
//     默认 DefaultWaitTimeout 30s，最大 MaxWaitTimeout 10min）、tail（可选，
//     返回结尾 N 行）、offset（可选，按绝对字节偏移读取）；默认返回自上次读取
//     以来的新输出。
//   - job_kill：job_id、reason（可选）、signal（可选，符号名或 POSIX 编号，
//     缺省 KILL）；返回最终状态与输出尾部。
//   - job_list：列出保留任务的 id、种类、状态、退出码、时长与命令。
//   - job_wait：job_id、timeout_ms（可选）；阻塞等待并返回最终状态与结尾
//     输出，超时后返回当前状态且任务继续运行。
//
// # DSH 兼容
//
// 管理器还实现了 tools/internal/background 中面向生产者的后台契约，因此
// bash、pwsh 与 terminal 工具无需导入本包即可登记后台执行。面向模型的参数
// 接受 DSH 命名：job_output 接受 wait 与 timeout_ms，job_kill 接受 reason，
// job_list 报告每个任务的 kind 与 label。已结束任务会保留在表中，直到宿主
// 关闭管理器，或前台调用方丢弃自己收集的任务。
//
// # 生命周期与 Owner 作用域
//
// 已结束任务保持登记，其输出与退出码仍可读取；job_list 会把它们显示为
// exited/killed/timed_out/failed。宿主在关闭时（或 agent 实例消失时）调用
// Manager.CloseAll 杀死运行中的任务并释放所有缓冲。超过 timeout 的任务会被
// 杀死并报告为 timed_out。
//
// 每个任务带一个 owner 标签：WithOwner 设置管理器默认 owner（job_start 与未显式
// 指定 Owner 的 StartProcess/StartOperation 继承它）；background.ProcessSpec /
// OperationSpec 的 Owner 字段可按任务覆盖。job_start 与 job_list 会展示 owner。
// Manager.CloseAllOwned(owner) (int, error) 只清理 owner 精确匹配的任务并返回
// 清理数量（包含已结束的匹配任务），空字符串匹配无 owner 的任务；不匹配的任务
// 保持运行。CloseAll 仍然不区分 owner 地清理全部任务。
//
// # 完成通知
//
// 当任务在没有调用方等待的情况下结束（且不是被模型杀死）时，管理器会排队一条
// 确定性的完成通知；宿主用 PendingNotices 读取，或注册 WithNoticeHandler。
// 若结束事件释放了一个存活的等待——job_output(wait: true)、job_wait，或前台
// shell/工具等待——则标记为“已等待”并且不产生通知，与 DSH 一致。
//
// # 沙箱策略与权限模式
//
// WithPolicy 为每个任务应用 sandbox.Policy：read-only 模式拒绝启动任务；进程
// 命令依据 Policy.CheckCommand 校验；工作目录必须位于策略根内；进程只继承
// Policy.CommandEnv 的环境。策略的 MaxOutputBytes 小于管理器自身的上限时，
// 会收紧单次工具响应。
//
// WithRunner 可选地挂接 OS 级受限执行器（sandbox.Runner）：对于未标记
// PolicyChecked 的进程请求，startExec 在 exec.Command 之前调用 Runner.Confine，
// 只启动它返回的 argv；Confine 失败则任务启动失败（fail closed，不 spawn）。
// 生产者（bash/pwsh）自行包装后传 PolicyChecked=true，管理器不会再包一次。
// 未挂接（nil，默认）时行为与之前完全一致。
//
// 权限模式对应关系：read-only 禁用任务启动；workspace-write（默认）把命令
// 限制在 workspaces 与命令允许列表内；danger-full-access 去掉这些围栏（显式
// 拒绝列表仍生效）。未传 WithPolicy 的管理器没有沙箱策略，此时审批 gate 是
// 唯一边界。
//
// # 上限
//
// 输出环保留最新字节并丢弃最旧字节；读者错过数据时会看到 "[truncated: ...]"
// 提示。工具响应以 DefaultMaxOutputBytes 为上限，单任务不能淹没模型上下文。
// 每个管理器最多保留 DefaultMaxJobs 个任务。
//
// # 接线
//
// 后台执行由宿主把同一个管理器接到 shell 与 terminal 上（full.All 已自动
// 接好）：
//
//	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
//	background := jobs.NewManager(jobs.WithPolicy(policy))
//	bash := builtin.NewShellWithOptions(root, timeout, builtin.WithBackgroundJobs(background))
//	terminals := terminal.NewManager(terminal.WithPolicy(policy), terminal.WithBackgroundJobs(background))
//	jobTools := background.Tools()
//
// 宿主进程退出前应调用 Manager.CloseAll，并在每个 agent 步骤后读取
// PendingNotices（或注册 WithNoticeHandler），把结束事件转成会话内通知。
package jobs
