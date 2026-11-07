//go:build windows

package terminal

import (
	"errors"
	"os"
	"os/exec"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// errPTYUnsupported explains the Windows limitation; ConPTY is out of scope for
// v0.5, so the Manager automatically uses the pipe transport there.
var errPTYUnsupported = errors.New("terminal: PTY transport requires ConPTY, which is not implemented in v0.5; using pipe")

// ptySupported reports whether this build can open a pseudo-terminal.
func ptySupported() bool { return false }

// startPTYCommand always fails on Windows.
func startPTYCommand(_ *sandbox.Policy, _, _ string, _ []string, _, _ int) (*os.File, *exec.Cmd, error) {
	return nil, nil, errPTYUnsupported
}
