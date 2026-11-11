// Package terminal 提供有状态的 shell 会话工具：terminal_open、
// terminal_send、terminal_read、terminal_signal、terminal_close，以及可选的
// terminal_list。
//
// 一个 Manager 拥有多个持久 shell 会话；每个会话是独立进程组中的 shell，通过
// 真实 PTY（Unix 默认）或基于标记的 pipe 传输与工具通信。
//
// # 模型可见工具
//
//   - terminal_open：cwd（可选）、shell（可选，默认 /bin/sh）、type（可选，
//     DSH 后端类型 "shell"/"pty"/"pipe"）、name（可选，owner-local 显示名）；
//     返回 session_id、实际传输方式与警告。
//   - terminal_send：sessionId（DSH 拼写）与 text（必填）；submit 默认 true
//     （追加 Enter）；timeout_ms 默认 DefaultTimeout（30s）、上限 MaxTimeout
//     （10min）；run_in_background 需要挂接后台任务管理器，进行中的发送会变成
//     可用 job_output 收集、job_kill 停止的任务。
//   - terminal_read：sessionId（必填）；可用 offset/count 分页读取保留滚屏
//     （offset 为最新相对行偏移，0 = 最新一页；count 默认 500、上限 2000），
//     或使用 wait_ms 保留“自上次读取/发送以来的新输出”等待语义。
//   - terminal_signal：sessionId 与 signal（SIGINT/SIGTERM/SIGKILL/SIGTSTP/
//     SIGHUP，也接受 INT/TERM/KILL/HUP/QUIT 与编号）；PTY 上 INT 以 Ctrl-C
//     字符（0x03）交付给前台进程组，其他信号发往 shell 的进程组。
//   - terminal_close：sessionId；杀死进程组、回收 shell 并释放缓冲。
//   - terminal_list：列出本管理器的会话及其 id、传输方式、工作目录、是否仍在
//     运行、退出码与 owner 标签（设置时）。
//
// # 安全与权限模式
//
// 沙箱策略的命令允许列表限制会话可以启动哪个 shell 二进制；会话运行后，交互
// 式 shell 会接受 terminal_send 写入的任何内容，因此逐条命令的允许/拒绝不适用
// 于终端会话。模型不可信时，应由审批 Gate 约束该表面。终端工具不提供操作系统
// 级隔离。
//
// 策略模式下的行为：
//
//   - read-only：拒绝 terminal_open（终端可以写文件）。
//   - workspace-write（默认）：shell 二进制必须位于允许列表；每次
//     terminal_send 用策略校验该行（拒绝 shell 操作符/替换，第一条命令必须在
//     允许列表内），cwd 必须在策略根内。
//   - danger-full-access：去掉根与允许列表围栏（显式拒绝列表仍生效）。
//   - 未传 WithPolicy：没有沙箱策略，审批 Gate 是唯一边界。
//
// 可用 WithRunner 挂接 OS 级受限执行器：PTY 与 pipe 两个 spawn 站点都会在启动
// 任何进程之前调用 Runner.Confine，并只启动它返回的 argv；Confine 失败会让
// terminal_open 失败（fail closed），不会创建未受限会话。未挂接（nil）时行为
// 与之前完全一致。
//
// # 传输方式
//
// 会话通过 WithTransport（默认 TransportAuto）选择传输：
//
//   - PTY（Unix 默认）：真实伪终端（/dev/ptmx + 从端，子进程为会话首进程，
//     从端是它的控制终端）。shell 交互运行，提示符、行编辑、作业控制、Ctrl-C
//     与全屏程序都可用。terminal_send 用静默期（DefaultQuietPeriod）而不是
//     标记判断命令结束。PTY 输出会被清洗：移除 ANSI 转义序列（WithStripANSI，
//     默认开启）并把 CRLF 归一化为 LF。
//   - Pipe：持久 /bin/sh + 普通 stdio 管道，每次发送使用唯一的
//     `printf '__RIG_DONE_<random>__\n'` 标记。确定性且到处可用；自行消费
//     stdin 的命令（无文件参数的 cat、交互式 REPL）会吞掉标记，这类发送将
//     超时并把会话标记为失同步。Windows（v0.5 无 ConPTY）以及 /dev/ptmx
//     不可用的宿主会自动使用该传输。
//
// # 完成语义
//
// PTY 发送在会话静默达到配置的静默期（或超时、或 shell 退出）时返回。长时间
// 静默运行的命令因此会带着已有输出提前返回；可用 terminal_read + wait_ms 继续
// 跟进，或用 terminal_signal 中断。pipe 发送等待标记，超时会作为错误报告。
//
// # Owner 作用域
//
// 每个会话带一个 owner 标签：WithOwner 设置管理器默认 owner，工具路径打开的
// 会话继承它；宿主 API Manager.Open(OpenOptions{Owner: ...}) 可以按会话指定
// owner（空表示使用默认 owner）。Manager.Sessions / terminal_list 会传播该标签。
// Manager.CloseAllOwned(owner) (int, error) 只关闭 owner 精确匹配的会话并返回
// 关闭数量，空字符串匹配无 owner 的会话；不匹配的会话保持运行。CloseAll 仍然
// 不区分 owner 地关闭全部会话。
//
// # 生命周期与接线
//
// terminal_open 在自己的会话/进程组中启动 shell；terminal_close（或
// Manager.CloseAll / Manager.CloseAllOwned）杀死进程组、关闭终端并回收进程。
// 已结束会话保持注册，直到关闭，以便 terminal_read 排空最后输出。宿主可用
// WithBackgroundJobs 把 jobs 管理器接到 terminal_send 的后台发送上，并在关闭时
// 调用 CloseAll：
//
//	policy := sandbox.New(
//		sandbox.WithRoots(root),
//		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
//		sandbox.WithCommandAllowlist("bash", "ls", "cat"),
//	)
//	background := jobs.NewManager(jobs.WithPolicy(policy))
//	terminals := terminal.NewManager(
//		terminal.WithPolicy(policy),
//		terminal.WithBackgroundJobs(background),
//	)
//	tools := terminals.ToolsWithList()
//
// # 上限
//
// 每个会话的输出环默认 1 MiB（WithBufferBytes），单次工具响应默认 32 KiB
// （WithMaxOutputBytes），每个管理器最多 MaxSessions 个会话；初始 PTY 窗口为
// 80x24（WithWindowSize）。
package terminal
