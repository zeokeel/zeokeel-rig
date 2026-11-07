//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly && !windows

package terminal

import (
	"errors"
	"os"
	"os/exec"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// errPTYUnsupported is returned on platforms without a PTY implementation.
var errPTYUnsupported = errors.New("terminal: PTY transport is not supported on this platform; using pipe")

// ptySupported reports whether this build can open a pseudo-terminal.
func ptySupported() bool { return false }

// startPTYCommand always fails on unsupported platforms.
func startPTYCommand(_ *sandbox.Policy, _, _ string, _ []string, _, _ int) (*os.File, *exec.Cmd, error) {
	return nil, nil, errPTYUnsupported
}
