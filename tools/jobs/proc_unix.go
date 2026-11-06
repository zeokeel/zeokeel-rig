//go:build unix

package jobs

import (
	"errors"
	"os/exec"
	"syscall"
)

// configureProcessGroup puts the job in its own process group so job_kill can
// signal the whole tree (`sh -c` plus children).
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// signalProcessGroup sends sig to the job's process group.
func signalProcessGroup(cmd *exec.Cmd, spec signalSpec) error {
	if cmd == nil || cmd.Process == nil {
		return errors.New("jobs: process is not running")
	}
	return syscall.Kill(-cmd.Process.Pid, platformSignal(spec))
}

// platformSignal maps a signalSpec to a syscall signal.
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
