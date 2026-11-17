// Package escalate 实现"沙箱执行提权"协议与宿主侧 broker。
//
// 我们已有的提权是**路径级**的（申请一个可写目录，见 sandbox.Policy.AddWritableRoot）。
// 有些命令无论拿多少可写目录都跑不成：git fetch 需要网络 + 宿主凭据，curl 需要
// 宿主侧的 ~/.netrc / 客户端证书，cloud CLI 需要登录态。这类请求的粒度是"**这条
// 命令请宿主侧执行**"，而不是"放宽某个目录"。本包就是这条通道的协议与宿主实现：
//
//	沙箱内进程 ──(NDJSON over stdin/stdout 或 unix socket)──> 宿主 broker ──> os/exec
//
// # 角色
//
//   - Client 运行在沙箱内（不受信）：它只能**请求**执行，不能决定是否执行。
//   - Broker 运行在宿主侧（受信）：它对每个请求重新做策略判定、审批、环境过滤、
//     目录校验与资源限制，然后才 spawn 子进程。
//
// # 线协议
//
// 一行一个 JSON 对象，换行分隔（NDJSON）：一行 Request，一行 Response，靠
// Request.ID / Response.ID 配对；同一个连接上可以有多条请求在途。
//
//	{"id":"r1","argv":["git","fetch","origin"],"dir":"/srv/app","reason":"拉取远端","timeoutMs":30000}
//	{"id":"r1","exitCode":0,"stdout":"...","stderr":""}
//
// 未知字段被忽略（前后兼容），单行上限 1MiB，坏行回一条 Denied 而不是断开连接。
//
// # 安全立场
//
// 这里的关键判断是：**沙箱内的一切都是不可信的**，所以 broker 不信任请求里的
// 任何字段——argv 必须在 Allow 前缀内且不在 Deny 内，Dir 必须绝对、存在、且
// EvalSymlinks 后仍落在 Roots 内，Env 只保留白名单前缀（GIT_/LC_ 等）并额外
// 丢弃会把命令变成执行器的高危变量（GIT_SSH_COMMAND、GIT_EXTERNAL_DIFF、
// GIT_CONFIG_COUNT 之类），ID 必须非空且不可重放。任何一步不满足就拒绝，且拒绝
// 一定是"不执行"（fail closed）。
//
// 必须清楚它**没有**解决什么：broker 执行的是宿主身份的命令，所以一条被允许的
// 命令本身就有宿主权限（能读宿主凭据、能联网）。Allow 列表就是全部边界——别把
// 带解释器语义的条目放进去（"sh"、"-c"、"--eval"、能读配置的 "git" 子命令等）。
//
// # 安全边界
//
// 本包封住的攻击面（都有测试覆盖）：
//
//   - argv 注入：只能执行 Allow 前缀内的命令，Deny 优先级更高，且 --flag=value
//     形式也按前缀判定（否则加个等号就能绕过禁止列表）。Deny 必须按**整个子命令**
//     写（Deny={{"git","push"}}），不要用 flag 级 Deny：--force / -f /
//     --force-with-lease 这类别名与等价形式太多，逐条列举必然漏；
//   - 可执行参数：git 的 --upload-pack / --receive-pack / --exec-path / --template /
//     -c / --config-env / --ext-diff / --textconv / -O / --output / --no-index
//     一律拒绝（见 builtinDangerousArgs）——它们能把"只读"子命令变成任意程序
//     执行器，或在 Roots 之外读写；这类参数不是子命令，Host 无法靠 Deny 前缀穷举；
//   - 目录逃逸：Dir 必须绝对、存在、EvalSymlinks 后仍在 Roots 内；".."、指向根外
//     的符号链接、前缀伪装（/root 与 /root-evil）都拒绝，执行前再解析一次；
//   - 环境注入：客户端 Env 只保留白名单前缀（默认 GIT_/LC_），PATH/LD_PRELOAD
//     之类一律丢弃；GIT_SSH_COMMAND、GIT_EXTERNAL_DIFF、GIT_CONFIG_COUNT、
//     GIT_TRACE* 这类"把命令变成执行器"的变量即使前缀命中也被丢弃；
//   - 重放：Request.ID 必须非空且同 broker 生命周期内唯一（FIFO 淘汰保持内存有界）；
//   - 资源：输出按 MaxOutputBytes 截断，执行按 MaxTimeout 有界（含孙进程占着输出
//     管道的情形），同时在途执行数受 MaxConcurrent 限制；
//   - 协议：单行上限 1MiB、坏 JSON 回 Denied 而不是断开、未知字段忽略、对端断开时
//     Serve 返回可归因错误且不留 goroutine。
//
// 本包**不**封住的（接线时必须自己处理）：
//
//   - 命令自身的语义：一条被允许的 curl 能访问任意 URL、读 ~/.netrc；一条被允许的
//     git 能用宿主凭据推代码。Allow 的粒度就是权限的粒度。
//   - 子进程资源：CPU/内存/进程数没有限额，只有时长与输出上限。
//   - 子进程派生：命令 fork 出来的进程（daemon、后台 job）会活得比请求久；超时只保证
//     broker 不再等它，不保证它被杀死（可靠杀进程组需要平台代码，本包刻意不做）。
//     这在 Windows 上尤其要看清：exec.CommandContext + WaitDelay 只保证"父进程不再等
//     输出管道"，**不会结束整棵进程树**，因此孙进程（cmd /c 起的子命令、PowerShell 起的
//     子进程）可能在本包已经返回之后继续存活。需要"杀干净"的宿主必须自己用 Job Object
//     （Windows）或进程组（POSIX）收尾，或者干脆把命令放进容器/VM 里执行。
//   - 传输通道认证：谁能连上这条连接谁就能提权。用宿主私有路径的 unix socket（0700）
//     或直接继承 fd，不要放在沙箱可写的目录里。
//   - 读取方向：提权是"执行"，不是"读宿主文件"；读操作仍走原有工具与策略。
//   - 工作目录的 TOCTOU：Dir 在判定时与 spawn 之前各解析一次，执行后还会复核并标记
//     （结果不可信时 exit=-1 + 诊断行）。但沙箱能写 Roots，因此"解析之后、chdir 之前"
//     把路径中的某一段换成符号链接，仍可能让命令在 Roots 之外执行——本包只能把窗口压到
//     最小并事后归因，彻底封死需要 dirfd + fchdir 这类平台代码（同理刻意不做）。
//   - 排队：MaxConcurrent 限制"同时执行数"，Policy.MaxQueue 限制"排队数"（默认 64，
//     -1 表示不限）。超过排队上限的请求立刻收到 Denied + QueueFullReason（不是超时），
//     客户端应当退避重试。排队时间本身仍不计入 MaxTimeout——默认上限把最坏延迟压到
//     (MaxQueue+MaxConcurrent) 个请求的量级；需要更严格限流的宿主应在工具层做频率
//     限制（broker 是库，不做跨连接的全局限流）。
//
// # 典型接线
//
//	policy := escalate.Policy{
//		// Allow 用**子命令级**前缀（不要写 {"git"}：那等于把所有 git 子命令与
//		// 参数都交给沙箱）；Deny 也用整个子命令，不要用 flag 级 Deny。
//		Allow:  [][]string{{"git", "fetch"}, {"git", "pull"}},
//		Deny:   [][]string{{"git", "push"}},
//		Roots:  []string{workspace},
//		MaxOutputBytes: 256 << 10,
//		MaxTimeout:     60 * time.Second,
//	}
//	broker, err := escalate.NewBroker(policy,
//		escalate.WithApprover(func(ctx context.Context, req escalate.Request) (bool, string, error) {
//			return host.Asker().Ask(ctx, "允许沙箱执行: "+strings.Join(req.Argv, " "))
//		}),
//		escalate.WithAuditor(func(req escalate.Request, resp escalate.Response) {
//			sessionLog.Append("sandbox/escalate", req, resp)
//		}),
//	)
//	if err != nil { ... }
//	go broker.Serve(ctx, conn)     // 宿主侧：一条连接一个 Serve
//	client := escalate.NewClient(conn2) // 沙箱内
//
// 本包只依赖标准库，纯 Go、无 cgo，默认不启用：宿主必须显式构造 Broker 并把连接
// 接到工具上。
package escalate
