//go:build darwin

package sandbox

import (
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strings"
)

// prepareOSExec selects sandbox-exec (Seatbelt) on macOS.
func prepareOSExec(sandboxOS *OSExec, opts OSExecOptions) error {
	if backend := strings.TrimSpace(opts.Backend); backend != "" && backend != "sandbox-exec" {
		return fmt.Errorf("%w: unsupported macOS backend %q (want sandbox-exec)", ErrOSExecUnavailable, backend)
	}
	launcher := strings.TrimSpace(opts.Launcher)
	if launcher == "" {
		launcher = "sandbox-exec"
	}
	resolved, err := exec.LookPath(launcher)
	if err != nil {
		return fmt.Errorf("%w: sandbox-exec not found (macOS system tool)", ErrOSExecUnavailable)
	}
	sandboxOS.backend = "sandbox-exec"
	sandboxOS.launcher = resolved
	return nil
}

// wrapOSCommand builds "sandbox-exec -p <profile> -- name args".
func wrapOSCommand(e *OSExec, ctx context.Context, dir, name string, args []string) (*exec.Cmd, error) {
	command := []string{"-p", sandboxProfile(e), "--", name}
	command = append(command, args...)
	cmd := exec.CommandContext(ctx, e.launcher, command...)
	cmd.Dir = dir
	return cmd, nil
}

// sandboxProfile is a Seatbelt profile: allow everything by default (the
// in-process policy still gates what tools may do), then deny writes outside
// the workspace/temp dirs and deny network when network access is off.
func sandboxProfile(e *OSExec) string {
	writable := slices.Clone(e.writable)
	writable = append(writable, "/tmp", "/private/tmp", "/private/var/folders")
	var builder strings.Builder
	builder.WriteString("(version 1)\n(allow default)\n(deny file-write*)\n(allow file-write*\n")
	for _, dir := range writable {
		fmt.Fprintf(&builder, "  (subpath %q)\n", dir)
	}
	builder.WriteString("  (literal \"/dev/null\")\n  (literal \"/dev/stdout\")\n  (literal \"/dev/stderr\")\n  (literal \"/dev/tty\")\n)\n")
	if !e.allowNetwork {
		builder.WriteString("(deny network*)\n")
	}
	return builder.String()
}
