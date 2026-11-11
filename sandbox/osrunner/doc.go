// Package osrunner 提供可选的纯 Go OS 沙箱 runner。
//
// 它实现 sandbox.Runner：把 bash、pwsh、job_start、持久终端与 git worktree 的
// 命令包成平台原生的受限形式，让文件写入只能落在策略允许的根与平台临时目录内。
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
// # 边界
//
// 这些 runner 只治理文件效果：网络、进程可见性、设备访问都不在承诺内；它们是
// 同世界（same-world）约束，不是容器/VM 级隔离。请把进程级隔离留给容器或 VM。
package osrunner
