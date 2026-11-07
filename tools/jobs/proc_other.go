//go:build !unix

package jobs

import (
	"errors"
	"os/exec"
)

// configureProcessGroup 在没有 POSIX 进程组的平台上为空操作。
func configureProcessGroup(_ *exec.Cmd) {}

// signalProcessGroup 回退为直接杀死进程；在这些平台上不保证子进程被回收。
func signalProcessGroup(cmd *exec.Cmd, spec signalSpec) error {
	if cmd == nil || cmd.Process == nil {
		return errors.New("jobs: process is not running")
	}
	if err := cmd.Process.Kill(); err != nil {
		return err
	}
	if spec.name != "" && spec.name != "KILL" {
		return errors.New("jobs: only KILL is supported on this platform; the process was killed instead")
	}
	return nil
}
