// Package osrunner 提供可选的纯 Go OS 沙箱 runner。
//
// 它实现 sandbox.Runner：把 bash、pwsh、job_start、持久终端与 git worktree 的
// 命令包成平台原生的受限形式，让文件写入只能落在工作区与平台临时目录内。
// 三个平台各有原生机制：
//
//   - Linux：bubblewrap（bwrap）优先，其次自带的 Landlock 启动器；
//   - macOS：系统 sandbox-exec（Seatbelt/SBPL）；
//   - Windows：Write-Restricted 受限令牌 + 目录 ACL 授权的隐藏 helper。
//
// 包本身是纯 Go、无 cgo、零外部 Go 依赖：Linux 走原始 syscall，Windows 走
// syscall.NewLazyDLL，macOS 只 exec 系统二进制。默认关闭——宿主显式调用 New
// 并把 runner 传给工具选项后才生效；无法强制请求的模式时返回
// sandbox.ErrSandboxUnavailable，绝不裸跑。
//
// # 与 DSH 的严格对齐
//
// 四个后端的渲染结果与 DSH（sandbox-local）**逐 token / 逐字符一致**，由
// conformance_dsh_test.go 钉死：
//
//	bwrap read-only        --ro-bind / /  --dev /dev  --unshare-pid  --proc /proc  --die-with-parent
//	bwrap workspace-write  上面 + --tmpfs /tmp  --bind <workspaceRoot> <workspaceRoot>
//	Landlock               --ro /  --rw /dev/null   （workspace-write 再 --rw /tmp --rw <ws>）
//	Seatbelt read-only     (version 1) (allow default) (deny file-write*) (allow file-write* (literal "/dev/null"))
//	Seatbelt workspace-wr  上面 + (allow file-write* (subpath "<ws>") (subpath "<tmp>") (subpath "<tmpdir>"))
//	Windows                read-only 不授权任何根；workspace-write 授权 ws + 本次私有 temp
//
// 对齐的代价（早期 Rig 的加固，现在**没有**了）：不渲染 `--cap-drop ALL`、
// `--remount-ro /dev`、`--unshare-net`、seccomp 过滤器、宿主守护进程 socket 遮蔽、
// 工具链缓存根、`/var/tmp`、SBPL 的 pty/fcntl/user-preference 规则、以及保护路径
// 的只读规则。需要这些边界时请用容器/VM，或在宿主侧自行叠加。
//
// # 宿主接入
//
// Linux 与 Windows 的 runner 使用“argv 前缀 + 隐藏 helper 子命令”的方式，因此
// 宿主必须在 main 的最开始调用一次 MaybeRunHelper：
//
//	func main() {
//	    if osrunner.MaybeRunHelper(os.Args) {
//	        return // helper 已经执行并退出
//	    }
//	    // ... 正常启动
//	}
//
// macOS 不需要 helper（直接 exec sandbox-exec），该调用会返回 false。
//
// # 平台差异
//
// 四个后端都从 BuildProfile（profile.go）渲染参数，因此“策略等于什么”只有一份
// 定义；但平台能力不同：
//
//   - bwrap 用 `--tmpfs /tmp` 提供私有 /tmp，read-only 下**没有** tmpfs（严格只读）；
//   - Landlock 只治理"内容写/建/删"，不治理 chmod/chown/utimensat，所以整体恒报
//     EnforcementPartial；它也无法创建 tmpfs，read-only 是真的只读；
//   - Landlock 的 path-beneath 规则是**并集**语义，没有否定规则，因此"根内只读
//     子路径"在这个后端上无法表达（DSH 同样不尝试）；
//   - macOS 的 Seatbelt profile 用 allow-default + deny-write* 姿态，与 DSH 相同；
//   - Windows 不隔离网络与读，只治理写/删；read-only 不授权任何根。
//
// danger-full-access 是透传，Enforcement 为 EnforcementNone（没有被任何 runner
// 包装）。
//
// # 边界
//
// 这些 runner 只治理文件效果：网络、进程可见性、设备访问都不在承诺内；它们是
// 同世界（same-world）约束，不是容器/VM 级隔离。请把进程级隔离留给容器或 VM。
package osrunner
