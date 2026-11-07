package sandbox

import (
	"runtime"
	"slices"
	"strings"
)

// CheckCommand validates one command against the command policy and confirms
// that dir (its working directory, "" or "." meaning the primary root) is
// inside the configured roots.
//
// Sandboxed commands must be invoked by bare name: argv[0] is matched against
// the allowlist after trimming whitespace, and path forms ("./git",
// "/usr/bin/git", "..\\git") are rejected so a workspace-controlled binary
// cannot be executed by giving it an allow-listed basename. Leading
// environment assignments ("PATH=. git status", "LD_PRELOAD=... git status")
// are rejected as well: they change how the shell resolves or loads the
// command, which would make the allowlist advisory. The denylist always wins
// over the allowlist. A policy with no allowlist denies every command; use
// WithCommandAllowlist to enable shell execution.
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
	if isEnvAssignment(argv[0]) {
		return deny("environment variable assignments before the command are not allowed in sandboxed commands")
	}
	name := strings.TrimSpace(argv[0])
	if name == "" || name == "." || name == string(filepathSeparator()) {
		return deny("empty command name")
	}
	if strings.ContainsAny(name, `/\`) {
		return deny("command %q must be a bare command name: paths are not allowed (use the allow-listed name)", name)
	}
	if strings.HasPrefix(name, "-") {
		return deny("command name %q must not start with '-'", name)
	}

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

// filepathSeparator returns the platform path separator as a string for error
// messages and empty-name checks.
func filepathSeparator() string {
	if runtime.GOOS == "windows" {
		return `\`
	}
	return "/"
}

// isEnvAssignment reports whether token is a shell VAR=value prefix
// (for example "PATH=.", "LD_PRELOAD=/tmp/x.so" or "FOO=1"). The shell applies
// these before starting the command, so a sandboxed command may not use them.
func isEnvAssignment(token string) bool {
	token = strings.TrimSpace(token)
	name, _, ok := strings.Cut(token, "=")
	if !ok || name == "" {
		return false
	}
	for index, r := range name {
		switch {
		case r == '_', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case index > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
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
	if runtime.GOOS == "windows" {
		return slices.ContainsFunc(names, func(candidate string) bool { return strings.EqualFold(candidate, name) })
	}
	return slices.Contains(names, name)
}

// envKey normalizes an environment variable name for matching.
func envKey(name string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(name)
	}
	return name
}
