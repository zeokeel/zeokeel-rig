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
// # 平台差异
//
// 四个后端都从 BuildProfile（profile.go）渲染参数，因此“策略等于什么”只有一份
// 定义；但平台能力不同，覆盖程度也不同：
//
//   - bwrap 提供私有 tmpfs /tmp（PrivateTmp），read-only 下 mktemp 这类只读意图
//     的命令仍可用；
//   - Landlock 无法创建 tmpfs，也没有“私有 /tmp”的概念，因此它的 read-only 是
//     严格只读（只授予 /dev/null 等设备）：需要写临时文件的命令会失败；
//   - Landlock 不治理 chmod/chown/utimensat，所以即使 ABI >= 3 也报告
//     EnforcementPartial；
//   - Landlock 的 path-beneath 规则是**并集**语义（实测：父目录 --rw + 子路径
//     --ro 时子路径仍可写），所以保护路径不能靠“父 rw + 子 ro”实现。Landlock
//     后端改为把写授权下移到保护链的兄弟项：含保护路径的目录本身只读，它的其它
//     子项逐个 --rw。代价是**不能在那种目录里新建/删除条目**（已存在的直接子文件
//     仍可写）。bwrap 用“后挂载 --ro-bind 覆盖前面的 rw bind”，Seatbelt 用
//     SBPL 的 last-match-wins，都不受这个限制；
//   - read-only 不授予 /dev/shm：它是可写的 tmpfs，写进去会真的落在宿主上；
//     workspace-write 才授予它；
//   - Windows ACL 后端**强制**策略的保护路径（policy.ProtectedPaths）：helper 把
//     每条 --protect 规范化并校验（必须是已存在的目录，否则 fail closed），只对
//     落在某个已授权工作区之内的路径生效（授权面之外的路径本来就不能写，跳过并
//     在 stderr 记一行诊断），然后给保护目录**及其既有子树**的每个对象加一条针对
//     capability SID 的继承 deny ACE（拒绝写/删/改元数据，**读仍然允许**）。
//     与 bwrap/Landlock/Seatbelt 的差别：Windows 后端整体仍是 partial（读与网络
//     不受限），且保护路径的 deny ACE 作为缓存资产在 helper 退出后仍保留在真实
//     目录上（只对沙箱 capability SID 生效，不影响宿主用户）；Everyone 的删除
//     子项拒绝与 Low 完整性标签仍然每次运行恢复；
//   - danger-full-access 是透传，Enforcement 为 EnforcementNone（没有被任何
//     runner 包装）。
//
// # 边界
//
// 这些 runner 只治理文件效果：网络、进程可见性、设备访问都不在承诺内；它们是
// 同世界（same-world）约束，不是容器/VM 级隔离。请把进程级隔离留给容器或 VM。
package osrunner
