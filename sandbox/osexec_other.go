//go:build !linux && !darwin && !windows

package sandbox

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
)

func prepareOSExec(_ *OSExec, _ OSExecOptions) error {
	return fmt.Errorf("%w: unsupported platform %s (built-in OS sandbox backends: Linux bwrap, macOS sandbox-exec, Windows restricted-token)", ErrOSExecUnavailable, runtime.GOOS)
}

func wrapOSCommand(*OSExec, context.Context, string, string, []string) (*exec.Cmd, error) {
	return nil, fmt.Errorf("%w: unsupported platform", ErrOSExecUnavailable)
}
