//go:build !unix

package terminal

import (
	"errors"
	"os/exec"
)

// configureProcessGroup is a no-op on platforms without POSIX process groups.
// The persistent-shell terminal is Unix-oriented; on other platforms signals
// fall back to killing the shell process directly (child processes are not
// guaranteed to be reaped).
func configureProcessGroup(_ *exec.Cmd) {}

// signalProcessGroup sends a signal to the shell process. Windows only supports
// kill semantics through the os package, so every signal name is treated as a
// kill; the returned error explains that for non-kill requests.
func signalProcessGroup(cmd *exec.Cmd, spec signalSpec) error {
	if cmd == nil || cmd.Process == nil {
		return errors.New("terminal: process is not running")
	}
	if spec.name != "" && spec.name != "KILL" {
		if err := cmd.Process.Kill(); err != nil {
			return err
		}
		return errors.New("terminal: only KILL is supported on this platform; the process was killed instead")
	}
	return cmd.Process.Kill()
}
