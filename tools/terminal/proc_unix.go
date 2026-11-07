//go:build unix

package terminal

import (
	"errors"
	"os/exec"
	"syscall"
)

// configureProcessGroup 把 shell 放入它自己的进程组，使信号可以同时打到
// shell 及其派生的每个子进程（无 PTY 时对终端前台进程组的等价实现）。
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// signalProcessGroup 向命令的进程组发送 sig。
func signalProcessGroup(cmd *exec.Cmd, spec signalSpec) error {
	if cmd == nil || cmd.Process == nil {
		return errors.New("terminal: process is not running")
	}
	return syscall.Kill(-cmd.Process.Pid, platformSignal(spec))
}

// platformSignal 把 signalSpec 映射为 syscall 信号。
func platformSignal(spec signalSpec) syscall.Signal {
	switch spec.name {
	case "INT":
		return syscall.SIGINT
	case "TERM":
		return syscall.SIGTERM
	case "HUP":
		return syscall.SIGHUP
	case "QUIT":
		return syscall.SIGQUIT
	case "TSTP":
		return syscall.SIGTSTP
	case "KILL", "":
		return syscall.SIGKILL
	}
	return syscall.Signal(spec.number)
}
