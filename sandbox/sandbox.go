// Package sandbox implements ZeoKeel-Rig's agent sandbox policy layer.
//
// A Policy is an immutable description of what an agent's tools may touch:
// which filesystem roots are visible, whether writes are allowed, which shell
// commands may run, which environment variables a command inherits, whether the
// network may be used, output and time limits, and path globs that are always
// denied (for example "**/.env"). The built-in tools take a *Policy through
// tool/builtin.NewWithPolicy; a host that only needs the default behavior keeps
// using NewFS/NewShell/NewBuiltins.
//
// The package is standard-library only, and a Policy is safe for concurrent use
// after New returns: options are applied at construction and the fields are
// never mutated afterwards.
//
// # Fail closed
//
// The zero policy (New() with no options) allows nothing: no file access, no
// shell commands, no network. Denials are reported as errors that wrap
// ErrDenied, so a model-facing tool can distinguish "the sandbox refused" from
// "the operation failed".
//
// # Path resolution
//
// ResolvePath validates one requested path against the configured roots. It
// rejects NUL bytes, any ".." component, absolute paths outside every root, and
// symbolic links that escape a root (the deepest existing ancestor is resolved
// with filepath.EvalSymlinks and the result must stay inside a root; remaining
// non-existent components cannot be symlinks). The returned value is the real
// absolute path (symlinks resolved). OpenPath additionally returns an *os.Root
// handle for the containing root, so tools can perform the actual I/O under the
// same confinement os.Root enforces at the syscall-adjacent layer.
//
// Relative paths resolve against the first configured root (the primary root);
// absolute paths may point into any configured root.
//
// # Policy is not a security boundary by itself
//
// The policy layer constrains Rig's built-in tools. It is not an OS sandbox:
// shell commands run as the current user (the approval Gate is the real
// boundary there, and command allow-listing is best effort for /bin/sh -c), and
// a tool that ignores the policy can still do anything the process can do.
package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrDenied is wrapped by every policy denial. Callers can test it with
// errors.Is to distinguish a sandbox refusal from a tool failure.
var ErrDenied = errors.New("sandbox: denied")

// Default limits used when the corresponding option is not configured. They
// mirror the built-in tool defaults so a zero policy line up with v0.2
// behavior once roots/commands are allowed.
const (
	// DefaultMaxOutputBytes is the default model-facing output cap (32 KiB).
	DefaultMaxOutputBytes = 32 << 10
	// DefaultTimeout is the default command timeout (30s).
	DefaultTimeout = 30 * time.Second
)

// defaultEnvAllowlist is the minimal environment a command inherits when
// WithEnvAllowlist is not used: PATH, HOME and LANG. Everything else (API keys,
// tokens, credentials) is dropped before the command starts.
var defaultEnvAllowlist = []string{"PATH", "HOME", "LANG"}

// Policy is one immutable sandbox policy. Construct it with New and Option
// functions; never mutate the fields directly.
type Policy struct {
	roots          []string // real absolute root paths, first is primary
	readOnly       bool
	commandAllow   []string // basenames
	commandDeny    []string // basenames (win over the allowlist)
	envAllow       []string
	envSet         bool
	network        bool
	maxOutputBytes int
	timeout        time.Duration
	deniedGlobs    []string // slash-separated, relative to a root
}

// Option configures a Policy. Options are applied in order by New.
type Option func(*Policy)

// New builds a Policy from opts. A policy with no options denies everything.
func New(opts ...Option) *Policy {
	policy := &Policy{
		maxOutputBytes: DefaultMaxOutputBytes,
		timeout:        DefaultTimeout,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(policy)
		}
	}
	return policy
}

// WithRoots sets the filesystem roots the policy exposes. It accepts absolute
// or relative paths; relative paths are resolved against the process working
// directory at construction time, symlinks are resolved when possible, and
// duplicates are dropped. Calling WithRoots with no roots removes all access.
// The first root is the primary root used to resolve relative path arguments.
func WithRoots(roots ...string) Option {
	return func(policy *Policy) {
		policy.roots = normalizeRoots(roots)
	}
}

// WithReadOnly marks the policy read-only: file-writing tools refuse to run and
// shell commands are disabled (a shell can write). Read tools keep working.
func WithReadOnly(readOnly bool) Option {
	return func(policy *Policy) { policy.readOnly = readOnly }
}

// WithCommandAllowlist allows only the listed command names. Names are matched
// against the basename of argv[0] (so "/usr/bin/git" matches "git"). Calling it
// with no names (or never calling it) disables shell execution entirely.
func WithCommandAllowlist(commands ...string) Option {
	return func(policy *Policy) { policy.commandAllow = normalizeCommandNames(commands) }
}

// WithCommandDenylist denies the listed command names even when they are also
// allow-listed. Names are matched like WithCommandAllowlist.
func WithCommandDenylist(commands ...string) Option {
	return func(policy *Policy) { policy.commandDeny = normalizeCommandNames(commands) }
}

// WithEnvAllowlist controls which environment variables a shell command
// inherits. With no names the command runs with an empty environment. When the
// option is not used at all, DefaultEnvAllowlist (PATH/HOME/LANG) applies.
func WithEnvAllowlist(names ...string) Option {
	return func(policy *Policy) {
		policy.envSet = true
		policy.envAllow = normalizeNames(names)
	}
}

// WithNetwork allows outgoing network access. Even when enabled, CheckURL still
// refuses non-HTTP(S) schemes and loopback/private/link-local/cloud-metadata
// addresses, so the option only enables public HTTP(S) destinations.
func WithNetwork(allow bool) Option {
	return func(policy *Policy) { policy.network = allow }
}

// WithMaxOutputBytes caps the model-facing output of policy-aware tools. A
// non-positive value keeps DefaultMaxOutputBytes.
func WithMaxOutputBytes(maxBytes int) Option {
	return func(policy *Policy) {
		if maxBytes > 0 {
			policy.maxOutputBytes = maxBytes
		}
	}
}

// WithTimeout sets the command timeout ceiling. A non-positive value keeps
// DefaultTimeout. Shell tools use it as the default and as the upper bound for
// a per-call timeout.
func WithTimeout(timeout time.Duration) Option {
	return func(policy *Policy) {
		if timeout > 0 {
			policy.timeout = timeout
		}
	}
}

// WithDeniedGlobs denies paths matching any pattern, e.g. "**/.env",
// "**/*.pem" or "secrets/**". Patterns are matched against the path relative to
// the containing root ("*" and "?" match within one segment, "**" matches any
// number of segments); a pattern without "/" also matches a file's base name at
// any depth. Denied paths are refused by ResolvePath/CheckPath and hidden from
// glob/grep/list_dir results.
func WithDeniedGlobs(patterns ...string) Option {
	return func(policy *Policy) { policy.deniedGlobs = normalizeGlobs(patterns) }
}

// Roots returns the configured roots (real absolute paths, first is primary).
func (p *Policy) Roots() []string {
	if p == nil {
		return nil
	}
	return append([]string(nil), p.roots...)
}

// ReadOnly reports whether the policy is read-only.
func (p *Policy) ReadOnly() bool { return p != nil && p.readOnly }

// NetworkAllowed reports whether outgoing network access is enabled.
func (p *Policy) NetworkAllowed() bool { return p != nil && p.network }

// Timeout returns the command timeout ceiling (DefaultTimeout when unset).
func (p *Policy) Timeout() time.Duration {
	if p == nil || p.timeout <= 0 {
		return DefaultTimeout
	}
	return p.timeout
}

// MaxOutputBytes returns the model-facing output cap (DefaultMaxOutputBytes
// when unset).
func (p *Policy) MaxOutputBytes() int {
	if p == nil || p.maxOutputBytes <= 0 {
		return DefaultMaxOutputBytes
	}
	return p.maxOutputBytes
}

func deny(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrDenied, fmt.Sprintf(format, args...))
}

// normalizeRoots resolves roots to real absolute paths and drops empty /
// duplicated entries.
func normalizeRoots(roots []string) []string {
	out := make([]string, 0, len(roots))
	seen := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		abs, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		abs = filepath.Clean(abs)
		if real, err := filepath.EvalSymlinks(abs); err == nil {
			abs = filepath.Clean(real)
		}
		// Fail closed: a root that cannot be opened right now is not exposed.
		rootHandle, err := os.OpenRoot(abs)
		if err != nil {
			continue
		}
		rootHandle.Close()
		if _, exists := seen[abs]; exists {
			continue
		}
		seen[abs] = struct{}{}
		out = append(out, abs)
	}
	return out
}

// normalizeCommandNames keeps basenames only, trimmed and de-duplicated.
func normalizeCommandNames(commands []string) []string {
	out := make([]string, 0, len(commands))
	seen := make(map[string]struct{}, len(commands))
	for _, command := range commands {
		command = strings.TrimSpace(command)
		if command == "" {
			continue
		}
		command = filepath.Base(command)
		key := strings.ToLower(command)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, command)
	}
	return out
}

// normalizeNames trims and de-duplicates environment variable names.
func normalizeNames(names []string) []string {
	out := make([]string, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

// normalizeGlobs trims, slash-normalizes and de-duplicates denied path globs.
func normalizeGlobs(patterns []string) []string {
	out := make([]string, 0, len(patterns))
	seen := make(map[string]struct{}, len(patterns))
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(filepath.ToSlash(pattern))
		pattern = strings.TrimPrefix(pattern, "./")
		if pattern == "" || pattern == "." {
			continue
		}
		if _, exists := seen[pattern]; exists {
			continue
		}
		seen[pattern] = struct{}{}
		out = append(out, pattern)
	}
	return out
}
