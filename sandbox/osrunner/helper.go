package osrunner

import "os"

// 隐藏 helper 子命令参数。宿主必须在 main 的最开始调用 MaybeRunHelper，否则
// Linux 与 Windows 的 argv 前缀式 runner 无法工作（会启动一个普通的宿主进程）。
const (
	// HelperArgLandlock 是 Linux Landlock 启动器的隐藏参数。
	HelperArgLandlock = "__rig_os_sandbox_landlock__"
	// HelperArgWindowsACL 是 Windows 受限令牌 helper 的隐藏参数。
	HelperArgWindowsACL = "__rig_os_sandbox_winacl__"
)

// MaybeRunHelper 检查 argv 是否是本包的内置 helper 调用；是则执行 helper、
// 以 helper 的退出码退出进程，并返回 true。不是 helper 调用时返回 false，
// 宿主应继续正常启动。
//
// 必须在任何 flag 解析或服务初始化之前调用，因为 helper 参数是 argv 前缀而不是
// 应用参数。
func MaybeRunHelper(args []string) bool {
	if len(args) == 0 {
		return false
	}
	handled, code := runHelper(args[1:])
	if !handled {
		return false
	}
	os.Exit(code)
	return true
}
