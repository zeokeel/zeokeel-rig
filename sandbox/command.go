package sandbox

import (
	"path/filepath"
	"runtime"
	"strings"
)

// CheckCommand validates one command against the command policy and confirms
// that dir (its working directory, "" or "." meaning the primary root) is
// inside the configured roots.
//
// The command name is the basename of argv[0], so an allowlist entry "git"
// matches /usr/bin/git. The denylist always wins over the allowlist. A policy
// with no allowlist denies every command; use WithCommandAllowlist to enable
// shell execution.
func (p *Policy) CheckCommand(argv []string, dir string) error {
	if p == nil {
		return deny("nil policy")
	}
	if len(argv) == 0 {
		return deny("empty command")
	}
	for _, arg := range argv {
		if strings.ContainsRune(arg, 0) {
			return deny("command argument contains a NUL byte")
		}
	}
	name := strings.TrimSpace(argv[0])
	if name == "" || name == "." || name == string(filepath.Separator) {
		return deny("empty command name")
	}
	name = filepath.Base(name)

	if len(p.commandAllow) == 0 {
		return deny("shell commands are disabled: no command allowlist configured")
	}
	if !containsCommandName(p.commandAllow, name) {
		return deny("command %q is not in the allowlist", name)
	}
	if containsCommandName(p.commandDeny, name) {
		return deny("command %q is denied by the policy", name)
	}

	workdir := strings.TrimSpace(dir)
	if workdir == "" {
		workdir = "."
	}
	if _, err := p.ResolvePath(workdir); err != nil {
		return err
	}
	return nil
}

// CommandEnv filters base down to the variables a sandboxed command may see.
// When WithEnvAllowlist was used only the listed names survive; otherwise the
// minimal PATH/HOME/LANG set applies. Variables are matched
// case-sensitively on Unix and case-insensitively on Windows, mirroring the OS
// environment semantics.
func (p *Policy) CommandEnv(base []string) []string {
	if p == nil {
		return nil
	}
	allowed := defaultEnvAllowlist
	if p.envSet {
		allowed = p.envAllow
	}
	if len(allowed) == 0 {
		return nil
	}
	allow := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		allow[envKey(name)] = struct{}{}
	}
	out := make([]string, 0, len(allowed))
	seen := make(map[string]struct{}, len(allowed))
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		lookup := envKey(key)
		if _, permitted := allow[lookup]; !permitted {
			continue
		}
		if _, duplicate := seen[lookup]; duplicate {
			continue
		}
		seen[lookup] = struct{}{}
		out = append(out, entry)
	}
	return out
}

// containsCommandName reports whether name is in names (basename semantics,
// case-insensitive on Windows).
func containsCommandName(names []string, name string) bool {
	for _, candidate := range names {
		if candidate == name {
			return true
		}
		if runtime.GOOS == "windows" && strings.EqualFold(candidate, name) {
			return true
		}
	}
	return false
}

// envKey normalizes an environment variable name for matching.
func envKey(name string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(name)
	}
	return name
}
