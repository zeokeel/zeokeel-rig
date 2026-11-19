package builtin

import "strconv"

// windowsTaskkillArgv 返回"连同子进程一起强杀"的 taskkill 参数：
//
//	taskkill /T /F /PID <pid>
//
// /T 杀整棵进程树、/F 强制。它是 Windows 上前台 shell 超时唯一可用的"杀孙进程"
// 手段（Windows 没有 POSIX 进程组）。pid 非正数时返回 nil：绝不把 0/负数交给
// taskkill（`/PID 0` 会匹配到"所有进程"这类危险语义）。
//
// 放在无构建约束的文件里，使 Linux/macOS 上也能用单测钉住命令构造；真正的调用
// 只在 proc_group_windows.go（//go:build windows）里发生。
func windowsTaskkillArgv(pid int) []string {
	if pid <= 0 {
		return nil
	}
	return []string{"/T", "/F", "/PID", strconv.Itoa(pid)}
}
