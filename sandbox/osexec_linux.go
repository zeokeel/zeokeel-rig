//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// prepareOSExec selects bubblewrap on Linux. bwrap is a small, widely packaged
// unprivileged sandbox (used by Flatpak and the DeepSeek Harness reference
// implementation) and needs no cgo from Rig.
func prepareOSExec(sandboxOS *OSExec, opts OSExecOptions) error {
	if backend := strings.TrimSpace(opts.Backend); backend != "" && backend != "bwrap" {
		return fmt.Errorf("%w: unsupported Linux backend %q (want bwrap)", ErrOSExecUnavailable, backend)
	}
	launcher := strings.TrimSpace(opts.Launcher)
	if launcher == "" {
		launcher = "bwrap"
	}
	resolved, err := exec.LookPath(launcher)
	if err != nil {
		return fmt.Errorf("%w: bubblewrap (bwrap) not found in PATH (install bubblewrap, or pass OSExecOptions.Launcher)", ErrOSExecUnavailable)
	}
	sandboxOS.backend = "bwrap"
	sandboxOS.launcher = resolved
	return nil
}

// wrapOSCommand builds "bwrap [flags] -- name args". The workspace root and
// extra writable dirs are re-mounted writable; / is read-only; /proc and /dev
// are fresh; /tmp is a private tmpfs; network is unshared unless allowed.
func wrapOSCommand(e *OSExec, ctx context.Context, dir, name string, args []string) (*exec.Cmd, error) {
	flags := []string{
		"--die-with-parent",
		"--unshare-all",
		"--cap-drop", "ALL",
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
		"--tmpfs", "/tmp",
	}
	if e.allowNetwork {
		flags = append(flags, "--share-net")
	}
	for _, writable := range e.writable {
		flags = append(flags, "--bind", writable, writable)
	}
	flags = append(flags, "--chdir", dir, "--", name)
	flags = append(flags, args...)
	cmd := exec.CommandContext(ctx, e.launcher, flags...)
	cmd.Dir = dir
	return cmd, nil
}
