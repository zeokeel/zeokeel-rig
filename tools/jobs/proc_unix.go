//go:build unix

package jobs

import (
	"errors"
	"os/exec"
	"syscall"
)

// configureProcessGroup 把任务放入它自己的进程组，使 job_kill 能对整个
// 进程树（`sh -c` 及其子进程）发送信号。
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// signalProcessGroup 向任务的进程组发送 sig。
func signalProcessGroup(cmd *exec.Cmd, spec signalSpec) error {
	if cmd == nil || cmd.Process == nil {
		return errors.New("jobs: process is not running")
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
	case "KILL", "":
		return syscall.SIGKILL
	}
	return syscall.Signal(spec.number)
}
