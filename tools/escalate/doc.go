// Package escalate 把"宿主权限执行"通道（sandbox/escalate 的 Broker + 协议）暴露成
// 一个模型可见的工具：request_command_escalation。
//
// # 它做什么
//
// 沙箱内的 agent 有些命令无论放宽多少可写目录都跑不成（git fetch 需要网络与宿主
// 凭据、cloud CLI 需要登录态）。这个工具让模型**申请**一条由宿主身份执行的命令：
//
//	模型 ──(工具调用)──> 本工具（校验 + 审批 + 限流）──(NDJSON)──> 宿主 Broker ──> os/exec
//
// 工具的职责是"把不可信的一侧关在门内"：它只接受 **argv 数组 + 理由 + 超时**，
// 不接受模型指定的可执行文件之外的东西——没有 shell 字符串、没有工作目录、没有
// Allow 列表、没有请求 ID。
//
// # 安全纪律（本包存在的理由）
//
//   - Allow 是**子命令级**前缀（`[][]string`），且只能由宿主在构造时给定。模型
//     无法通过参数放宽它——Schema 里根本没有这个字段（additionalProperties=false）。
//   - **每次调用都过审批**：构造时必须提供 approver，否则构造直接失败（fail
//     closed）。工具自己不再叠一层"是否要审批"的判断，只把 argv/reason 交给宿主。
//   - **工作目录由宿主派生**：工具用 Options.Roots 的第一个根作为执行目录，模型传
//     的任何目录字段都被忽略（Schema 里也没有 dir）。
//   - **拒绝解释器**：Allow/Deny/Approval 里出现 sh/bash/zsh/python/node/env/
//     xargs/find/make/awk/perl（及常见同族）或 git 的 difftool/mergetool/bisect/
//     submodule、以及会把 git 变成执行器的危险 flag，构造期就报错。
//   - 超时、输出、并发、排队都有上界（默认 20s / 128KiB / 2 / 8）。
//
// # 结果三态（稳定文案）
//
// 成功（命令跑了，含非零退出）：
//
//	exit_code: 0
//	stdout:
//	...
//	stderr:
//	...
//
// 策略拒绝（命令**没有**执行）：`[escalation denied: <原因>]`
//
// 通道故障，或命令没有正常退出（超时、取消、执行失败，协议里 ExitCode=-1）：
// `[escalation failed: <错误>]`
//
// 宿主繁忙（排队已满，不是策略拒绝，可以稍后重试）：`[escalation busy: <原因>; ...]`
//
// # 装配
//
// 宿主侧一次装配（推荐）：用 NewWithBroker 自建 Broker + Client。它接受两种 conn：
// 实现了 TransportPair 的连接对（真实 net.Pipe / socketpair，Broker 与 Client 各占
// 一端，推荐）或同进程内存自环连接；也可以用 NewWithPair 显式传入 (Broker 端,
// Client 端)。已有 Client 的宿主（例如提权 helper 连回宿主 Broker）用 New。
//
// 工具返回值额外实现 `Close() error`：宿主可以用它回收后台 Serve 与连接。
//
// # 一个可以直接抄的安全配置（deny-all 骨架 + 逐条放开）
//
// 起步姿态是"一条都不给"，然后按实际需要逐条放开——每条都必须是**子命令级**的完整
// 前缀（{"git","fetch"}，而不是 {"git"}）。理由：Allow 里写裸程序名等于把这个程序的
// 全部子命令、全部参数都交给宿主身份执行。
//
//	opts := escalate.Options{
//		Roots: []string{workspaceRoot}, // 必须存在、绝对路径；执行目录 = 第一个根
//		Allow: [][]string{
//			{"git", "fetch"}, // 第一条：只放开拉取
//			// {"git", "pull"},          // 需要时再逐条打开
//			// {"gh", "pr", "view"},     // 只读的远端查询
//			// {"kubectl", "get"},       // 只读的集群查询
//		},
//		Deny: [][]string{
//			// Deny 只用于给"宽 Allow"兜底（同样写整个子命令）：
//			{"git", "push"},
//		},
//		MaxTimeout:     30 * time.Second,
//		MaxOutputBytes: 256 << 10,
//		MaxConcurrent:  2,
//		MaxQueue:       8,
//		Auditor: func(req escalate.Request, resp escalate.Response) {
//			log.Printf("escalation argv=%q dir=%s denied=%v exit=%d", req.Argv, req.Dir, resp.Denied, resp.ExitCode)
//		},
//	}
//	tool, err := escalate.NewWithBroker(pair, approver, opts) // pair 见上文装配说明
//
// # 为什么"子命令级 Allow"还不够，以及危险参数由谁拦
//
// 真正的风险不在子命令本身，而在**参数**：`git fetch` 只读，但
// `git fetch --upload-pack=/tmp/evil` 会让宿主执行 /tmp/evil；
// `git log -O/bin/echo` 同理。这类参数不是子命令，无法用 Deny 前缀逐条穷举，而且
// 很容易因为写法变化绕过：
//
//   - **长选项缩写**：git 的 parse-options 接受无歧义缩写，`--upload-pac`、
//     `--upload`、`--upload-p` 都等于 `--upload-pack`；
//   - **短选项附加值**：`-O/bin/echo` 等于 `-O /bin/echo`；
//   - **程序名写法**：`/usr/bin/git`、`GIT`、`git.exe` 是同一个程序。
//
// 因此危险参数表放在 `sandbox/escalate.MatchDangerousArg`（按"互为前缀"的形状判定，
// 并规范化程序名），Broker 执行期与本工具构造期/调用期共用它——只比 token 相等
// 的实现会被上面三种写法直接绕过（这正是第二轮对抗审计的 P0）。宿主不需要、也
// **不应该**把危险参数写进 Deny：那是内置表负责的事，写进 Deny 只会漏。
//
// 工具不自己注册：模型的工具目录由 tools/catalog、tools/presets 与 tools/full
// 决定，宿主把它接进这些地方之后模型才可见。
package escalate
