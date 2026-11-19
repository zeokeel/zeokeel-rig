//go:build unix

package builtin

import (
	"errors"
	"os/exec"
	"syscall"
)

// 前台 shell 的进程组治理。
//
// 为什么前台也需要进程组：`sh -c '(sleep 1; echo late > f) & sleep 30'` 这类命令
// 会把真正的工作派给孙进程。exec.CommandContext 在截止时间只 Kill 直接子进程，
// 孙进程继续运行并在工具已经返回"超时"之后写入工作区 —— 一次调用既报失败又产生
// 副作用，调用方无法据此判断状态（审计 P1）。后台执行路径（tools/jobs）一直用
// 独立的进程组解决同一个问题（proc_unix.go），这里补齐前台路径。

// configureForegroundProcessGroup 把前台命令放进它自己的进程组。
func configureForegroundProcessGroup(command *exec.Cmd) {
	if command == nil {
		return
	}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killForegroundProcessGroup 向整个进程组发 SIGKILL。进程已经退出时返回 nil：
// 截止时间/取消路径里"进程刚好自己结束"是常态，不该因此变成工具错误。
func killForegroundProcessGroup(command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return nil
	}
	err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
