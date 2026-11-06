//go:build !unix

package jobs

import (
	"errors"
	"os/exec"
)

// configureProcessGroup is a no-op on platforms without POSIX process groups.
func configureProcessGroup(_ *exec.Cmd) {}

// signalProcessGroup falls back to killing the process directly; child
// processes are not guaranteed to be reaped on these platforms.
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
