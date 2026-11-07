//go:build unix

package terminal

import (
	"errors"
	"os/exec"
	"syscall"
)

// configureProcessGroup puts the shell in its own process group so a signal can
// target the shell and every child it spawned (PTY-less equivalent of a
// terminal's foreground process group).
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// signalProcessGroup sends sig to the command's process group.
func signalProcessGroup(cmd *exec.Cmd, spec signalSpec) error {
	if cmd == nil || cmd.Process == nil {
		return errors.New("terminal: process is not running")
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
	case "TSTP":
		return syscall.SIGTSTP
	case "KILL", "":
		return syscall.SIGKILL
	}
	return syscall.Signal(spec.number)
}
