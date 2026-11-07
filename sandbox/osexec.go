package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrOSExecUnavailable is returned (wrapped) by NewOSExec when no supported
// OS-level sandbox backend is available on the host.
var ErrOSExecUnavailable = errors.New("sandbox: no usable OS sandbox backend")

// OSExecOptions configures the opt-in OS-level process sandbox.
//
// The OS sandbox confines processes started by Rig's tools (bash, pwsh, jobs,
// terminal and worktree git). It is a second layer under the approval Gate:
// the in-process Policy remains the policy layer, the Gate remains the approval
// boundary, and this layer is what makes a misbehaving/cooperative model unable
// to write outside the workspace or (on Linux/macOS) reach the network when
// network access is off.
//
// Backends:
//   - Linux: bubblewrap ("bwrap"), unprivileged user namespaces; writable
//     paths are the workspace root + WritableDirs, /tmp is a private tmpfs.
//   - macOS: sandbox-exec (Seatbelt profile); writes are limited to the
//     workspace root, WritableDirs and the system temp dirs; network is denied
//     unless AllowNetwork is set.
//   - Windows: restricted token + low integrity level + low-integrity label on
//     the writable roots (via icacls). This blocks writes outside the workspace
//     and drops privileges; it does not isolate the network. Use a container or
//     OS sandbox (Windows Sandbox, WSL2, Hyper-V) for untrusted models.
//   - Other platforms: NewOSExec fails closed with ErrOSExecUnavailable.
//
// The backend never uses cgo: Linux/macOS launch a system helper through
// os/exec, Windows uses syscall-level APIs and the standard library.
type OSExecOptions struct {
	// Root is the writable workspace root. Required.
	Root string
	// AllowNetwork keeps network access inside the sandbox (default: denied on
	// Linux/macOS where the backend can unshare it; Windows does not isolate
	// the network at all).
	AllowNetwork bool
	// WritableDirs optionally adds absolute paths that stay writable inside the
	// sandbox (for example a cache directory). Every path must exist.
	WritableDirs []string
	// Backend optionally overrides auto-detection: "bwrap" (Linux) or
	// "sandbox-exec" (macOS) or "restricted-token" (Windows). Empty means the
	// platform default. Unknown/unsupported values fail closed.
	Backend string
	// Launcher optionally overrides the helper executable path (tests or
	// non-standard installations such as a bwrap shim).
	Launcher string
}

// OSExec is an immutable OS-level sandbox launcher. It is safe for concurrent
// use.
type OSExec struct {
	backend      string
	launcher     string
	root         string
	allowNetwork bool
	writable     []string
}

// NewOSExec detects/validates a platform backend and prepares it (for Windows
// this includes labelling the writable roots as low integrity). It fails closed
// with ErrOSExecUnavailable when the platform has no usable backend.
func NewOSExec(opts OSExecOptions) (*OSExec, error) {
	root := strings.TrimSpace(opts.Root)
	if root == "" {
		return nil, errors.New("sandbox: OSExecOptions.Root is required")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("sandbox: resolve OS sandbox root %q: %w", root, err)
	}
	info, err := os.Stat(absRoot)
	if err != nil {
		return nil, fmt.Errorf("sandbox: OS sandbox root %q: %w", absRoot, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("sandbox: OS sandbox root %q is not a directory", absRoot)
	}
	writable := []string{absRoot}
	for _, dir := range opts.WritableDirs {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			return nil, fmt.Errorf("sandbox: resolve writable dir %q: %w", dir, err)
		}
		info, err := os.Stat(abs)
		if err != nil {
			return nil, fmt.Errorf("sandbox: writable dir %q: %w", abs, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("sandbox: writable dir %q is not a directory", abs)
		}
		writable = append(writable, abs)
	}
	exec := &OSExec{root: absRoot, allowNetwork: opts.AllowNetwork, writable: writable}
	if err := prepareOSExec(exec, opts); err != nil {
		return nil, err
	}
	return exec, nil
}

// Backend reports the selected backend name ("bwrap", "sandbox-exec" or
// "restricted-token").
func (e *OSExec) Backend() string {
	if e == nil {
		return ""
	}
	return e.backend
}

// Root reports the writable workspace root the sandbox was built for.
func (e *OSExec) Root() string {
	if e == nil {
		return ""
	}
	return e.root
}

// AllowNetwork reports whether the sandbox keeps network access.
func (e *OSExec) AllowNetwork() bool {
	return e != nil && e.allowNetwork
}

// CommandContext builds an *exec.Cmd that runs name/args inside the OS sandbox.
// The caller owns the returned command and may set Env/Stdout/Stderr as usual.
func (e *OSExec) CommandContext(ctx context.Context, dir, name string, args ...string) (*exec.Cmd, error) {
	workdir, err := e.resolveWorkDir(dir)
	if err != nil {
		return nil, err
	}
	return wrapOSCommand(e, ctx, workdir, name, args)
}

func (e *OSExec) resolveWorkDir(dir string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return e.root, nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("sandbox: resolve OS sandbox workdir %q: %w", dir, err)
	}
	for _, allowed := range e.writable {
		if osPathWithin(allowed, abs) {
			return abs, nil
		}
	}
	return "", fmt.Errorf("sandbox: workdir %q is outside the OS sandbox writable roots %v", abs, e.writable)
}

// osPathWithin reports whether path is root or below it.
func osPathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// CommandContext builds a command for the tools: it applies the OS sandbox when
// the policy has one, and falls back to a plain exec.CommandContext otherwise.
// It is safe to call on a nil policy.
func (p *Policy) CommandContext(ctx context.Context, dir, name string, args ...string) (*exec.Cmd, error) {
	if p == nil || p.osExec == nil {
		cmd := exec.CommandContext(ctx, name, args...)
		if dir != "" {
			cmd.Dir = dir
		}
		return cmd, nil
	}
	return p.osExec.CommandContext(ctx, dir, name, args...)
}

// OSExec returns the attached OS sandbox, or nil.
func (p *Policy) OSExec() *OSExec {
	if p == nil {
		return nil
	}
	return p.osExec
}
