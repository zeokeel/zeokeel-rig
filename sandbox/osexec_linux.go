//go:build linux

package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
)

// nativeSandboxHelperArg is the hidden argv marker used to re-exec the host
// binary as a tiny sandbox runner. The sandbox package's init() applies
// Landlock/seccomp and then execs the real command; no helper binary or system
// package is required.
const nativeSandboxHelperArg = "__rig_os_sandbox_helper__"

// nativeSandboxBackend is the cgo-free native Linux backend: Landlock LSM for
// filesystem write confinement plus a seccomp filter that denies INET sockets
// when network access is off, both applied to a re-exec'd copy of the host
// binary before it execs the real command.
const nativeSandboxBackend = "landlock"

// bwrapBackend is the optional bubblewrap fallback (requires the bwrap binary).
const bwrapBackend = "bwrap"

// prepareOSExec selects the default native Linux backend. "bwrap" can be
// requested explicitly for hosts that prefer it or run on a kernel without
// Landlock; "landlock" never depends on an installed binary.
func prepareOSExec(sandboxOS *OSExec, opts OSExecOptions) error {
	switch backend := strings.TrimSpace(opts.Backend); backend {
	case "", nativeSandboxBackend:
		if err := preflightNativeSandbox(sandboxOS.allowNetwork); err != nil {
			if backend == nativeSandboxBackend {
				return err
			}
			// Auto mode may still work with bubblewrap on an old kernel.
			if bwrap, lookErr := exec.LookPath("bwrap"); lookErr == nil {
				sandboxOS.backend = bwrapBackend
				sandboxOS.launcher = bwrap
				return nil
			}
			return err
		}
		sandboxOS.backend = nativeSandboxBackend
		return nil
	case bwrapBackend:
		resolved, err := exec.LookPath(strings.TrimSpace(opts.Launcher))
		if strings.TrimSpace(opts.Launcher) == "" {
			resolved, err = exec.LookPath("bwrap")
		}
		if err != nil {
			return fmt.Errorf("%w: bubblewrap (bwrap) not found in PATH", ErrOSExecUnavailable)
		}
		sandboxOS.backend = bwrapBackend
		sandboxOS.launcher = resolved
		return nil
	default:
		return fmt.Errorf("%w: unsupported Linux backend %q (want landlock or bwrap)", ErrOSExecUnavailable, backend)
	}
}

// wrapOSCommand dispatches to the selected backend.
func wrapOSCommand(e *OSExec, ctx context.Context, dir, name string, args []string) (*exec.Cmd, error) {
	if e.backend == bwrapBackend {
		return wrapBwrapCommand(e, ctx, dir, name, args)
	}
	return wrapNativeSandboxCommand(e, ctx, dir, name, args)
}

// wrapNativeSandboxCommand re-executes the host binary (which imports this
// package) with a spec argument; init() applies the sandbox and execs name/args.
func wrapNativeSandboxCommand(e *OSExec, ctx context.Context, dir, name string, args []string) (*exec.Cmd, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("sandbox: locate host binary for OS sandbox: %w", err)
	}
	spec, err := json.Marshal(nativeSandboxSpec{
		Writable:     slices.Clone(e.writable),
		AllowNetwork: e.allowNetwork,
	})
	if err != nil {
		return nil, fmt.Errorf("sandbox: encode OS sandbox spec: %w", err)
	}
	helperArgs := make([]string, 0, len(args)+4)
	helperArgs = append(helperArgs, nativeSandboxHelperArg, string(spec), "--", name)
	helperArgs = append(helperArgs, args...)
	cmd := exec.CommandContext(ctx, self, helperArgs...)
	cmd.Dir = dir
	return cmd, nil
}

// nativeSandboxSpec travels from the parent to the re-exec'd sandbox runner.
type nativeSandboxSpec struct {
	Writable     []string `json:"writable"`
	AllowNetwork bool     `json:"allow_network"`
}

// wrapBwrapCommand builds the optional "bwrap [flags] -- name args" command.
func wrapBwrapCommand(e *OSExec, ctx context.Context, dir, name string, args []string) (*exec.Cmd, error) {
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

// nativeSandboxSeccompSupported reports whether the seccomp network filter can
// validate the calling ABI on this architecture.
func nativeSandboxSeccompSupported() bool {
	switch runtime.GOARCH {
	case "amd64", "arm64":
		return true
	default:
		return false
	}
}
