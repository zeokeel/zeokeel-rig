package sandbox

import (
	"strings"
)

// Shell parsing and the shell-command policy.
//
// Rig's command allowlist is an in-process policy layer, not a security
// boundary: it exists so a host can constrain what a cooperative model is
// allowed to run, while the approval Gate (and, for untrusted models, an
// OS-level sandbox) remains the real boundary. A correct policy still has to
// agree with the shell about what a command means, otherwise a quoted
// substitution or a line continuation can smuggle a second program past it.
// The parser below is therefore a small, explicit POSIX-shell word splitter
// that understands:
//
//   - whitespace word separation;
//   - single quotes (every byte literal, including backslashes and newlines);
//   - double quotes (literal except for the substitution constructs the policy
//     rejects); a backslash inside double quotes quotes the next byte only for
//     `$`, backtick, `"`, `\` and newline, as POSIX specifies;
//   - backslash escaping outside single quotes, including the shell's
//     backslash-newline line continuation, which is removed before the policy
//     evaluates the remaining text (this is what a hand-written scanner that
//     only looks at raw bytes gets wrong: `"$\<newline>(id)"` becomes
//     `"$(id)"` after continuation removal and executes when we do not model
//     it);
//   - `#` comments when the `#` starts a word.
//
// In strict (default) mode the parser also rejects every construct that can
// start or splice another program: unquoted control operators, command
// substitution (even inside double quotes), backtick substitution and process
// substitution. With WithAllowShellOperators the parser still splits words but
// treats those constructs as ordinary characters, matching the documented
// opt-out (the allowlist then only bounds the first program and is advisory).
//
// The parser is deliberately conservative: constructs that are harmless but
// hard to prove safe, such as `$((...))` arithmetic expansion (which may
// contain command substitution) and parentheses inside `${...}`, are rejected.
// A caller that needs them can run the command through the approval Gate or
// WithAllowShellOperators.

// ParseShellCommand validates command against the shell-syntax policy and
// returns its words (argv) for the command allow/deny check. It is the single
// parser used by the built-in bash, pwsh, jobs and terminal tools, so the
// validation and the argv[0] check can never disagree about quoting.
//
// The callers must still apply CheckCommand to the returned argv and must
// treat the result as policy input, not as a security boundary: the approval
// Gate is the boundary for irreversible actions, and an OS-level sandbox is
// required when the model is untrusted.
//
// With WithAllowShellOperators the policy does not reject shell syntax; the
// command is still split into words, but operators and substitutions become
// ordinary word characters (the allowlist is then advisory-only).
func (p *Policy) ParseShellCommand(command string) ([]string, error) {
	if p == nil {
		return nil, deny("nil policy")
	}
	return parseShellCommand(command, !p.allowShellOperators)
}

// CheckShellCommand validates a command string that will be executed by
// /bin/sh -c (or an equivalent script engine), beyond the argv[0] check
// CheckCommand performs. It is the validation half of ParseShellCommand; tools
// that also need argv[0] should call ParseShellCommand once instead of calling
// both functions.
//
// The allowlist only sees the first command, so anything that can start or
// splice another program is rejected unless the policy opted out with
// WithAllowShellOperators:
//
//   - unquoted shell control operators: ;, newlines, |, &, redirects (<, >),
//     subshell parentheses;
//   - command substitution `$(...)` and backticks, including inside double
//     quotes: "echo \"$(id)\"" executes id even though the operator is quoted;
//   - process substitution `<(...)` and `>(...)`;
//   - line continuations are removed before the check (the parser models what
//     /bin/sh will actually see), so `"$\<newline>(id)"` is rejected as the
//     `$(...)` it becomes.
//
// Single-quoted text is fully literal. Double-quoted text is literal text
// except for the substitution constructs above. Escaped operators remain
// literal (echo a\;b). Leading environment assignments are rejected by
// CheckCommand. WithAllowShellOperators turns this check off entirely.
//
// Tools that do not use /bin/sh -c (for example PowerShell) get the same
// conservative rejection; pass a command that already contains the intended
// operators quoted, or opt out per policy.
func (p *Policy) CheckShellCommand(command string) error {
	if p == nil {
		return deny("nil policy")
	}
	if p.allowShellOperators {
		return nil
	}
	_, err := parseShellCommand(command, true)
	return err
}

type shellQuoteState int

const (
	shellUnquoted shellQuoteState = iota
	shellSingleQuoted
	shellDoubleQuoted
)

// parseShellCommand is the single POSIX-shell word splitter shared by strict
// validation and the allowlist argv check. It never executes anything; it only
// reads bytes and reports the first construct the policy cannot model.
func parseShellCommand(command string, strict bool) ([]string, error) {
	if strings.ContainsRune(command, 0) {
		return nil, deny("command contains a NUL byte")
	}
	// The shell removes backslash-newline before tokenization everywhere except
	// inside single quotes; do the same up front so a construct split by a line
	// continuation ("$\<newline>(id)") is evaluated exactly as /bin/sh will
	// evaluate it. The parser below also handles continuations for commands that
	// reach it directly, but pre-stripping makes the "$" and "(" adjacent.
	runes := []rune(stripShellLineContinuations(command))
	args := make([]string, 0, 8)
	var current strings.Builder
	var started bool
	state := shellUnquoted

	flush := func() {
		if started {
			args = append(args, current.String())
			current.Reset()
			started = false
		}
	}

	for index := 0; index < len(runes); index++ {
		r := runes[index]
		switch state {
		case shellSingleQuoted:
			// POSIX: every byte is literal inside single quotes, including a
			// backslash and a newline; the only way out is the closing quote.
			if r == '\'' {
				state = shellUnquoted
				started = true
				continue
			}
			current.WriteRune(r)
			started = true

		case shellDoubleQuoted:
			switch r {
			case '\\':
				if index+1 >= len(runes) {
					return nil, deny("shell command ends with a backslash inside double quotes")
				}
				next := runes[index+1]
				if next == '\n' {
					// The shell removes backslash-newline before parsing,
					// including inside double quotes. Remove it here too so
					// `"$\\<newline>(id)"` is seen as `"$(id)"` and rejected.
					index++
					continue
				}
				// POSIX: in double quotes a backslash quotes only $, `, ", \
				// and newline; before any other byte it is literal. Writing the
				// pair through preserves shell word value; both bytes are
				// non-special for the policy, so either representation is safe.
				if next == '$' || next == '`' || next == '"' || next == '\\' {
					current.WriteRune(next)
					index++
					started = true
					continue
				}
				current.WriteRune(r)
				started = true
			case '"':
				state = shellUnquoted
				started = true
			case '$':
				if index+1 < len(runes) && runes[index+1] == '(' {
					return nil, deny("command substitution $(...) is not allowed, even inside double quotes (use WithAllowShellOperators to opt out)")
				}
				current.WriteRune(r)
				started = true
			case '`':
				return nil, deny("backtick command substitution is not allowed, even inside double quotes (use WithAllowShellOperators to opt out)")
			default:
				current.WriteRune(r)
				started = true
			}

		default: // shellUnquoted
			switch {
			case r == '\\':
				if index+1 >= len(runes) {
					return nil, deny("shell command ends with an escape character")
				}
				next := runes[index+1]
				if next == '\n' {
					// Line continuation: the shell removes both bytes before
					// tokenizing, so the policy must not see them either.
					index++
					continue
				}
				current.WriteRune(next)
				index++
				started = true
			case r == '\'':
				state = shellSingleQuoted
				started = true
			case r == '"':
				state = shellDoubleQuoted
				started = true
			case r == '$':
				if index+1 < len(runes) && runes[index+1] == '(' {
					if strict {
						return nil, deny("command substitution $(...) is not allowed (use WithAllowShellOperators to opt out)")
					}
					current.WriteRune(r)
					started = true
					continue
				}
				current.WriteRune(r)
				started = true
			case r == '`':
				if strict {
					return nil, deny("backtick command substitution is not allowed (use WithAllowShellOperators to opt out)")
				}
				current.WriteRune(r)
				started = true
			case r == ' ' || r == '\t' || r == '\r' || r == '\n':
				if strict && (r == '\r' || r == '\n') {
					return nil, deny("shell operator %q is not allowed in a command (use WithAllowShellOperators to opt out of this check)", string(r))
				}
				flush()
			case strict && isShellControlRune(r):
				if (r == '<' || r == '>') && index+1 < len(runes) && runes[index+1] == '(' {
					return nil, deny("process substitution %q is not allowed (use WithAllowShellOperators to opt out)", string(r)+"(")
				}
				return nil, deny("shell operator %q is not allowed in a command (use WithAllowShellOperators to opt out of this check)", string(r))
			case r == '#':
				if !started {
					// POSIX: a `#` that begins a word starts a comment.
					for index+1 < len(runes) && runes[index+1] != '\n' {
						index++
					}
					continue
				}
				current.WriteRune(r)
				started = true
			default:
				current.WriteRune(r)
				started = true
			}
		}
	}

	if state != shellUnquoted {
		return nil, deny("shell command has an unterminated quote")
	}
	flush()
	return args, nil
}

// stripShellLineContinuations removes POSIX line continuations (backslash
// followed by newline) outside single quotes, mirroring the shell's
// pre-tokenization processing. Inside single quotes every byte is literal, so a
// backslash-newline there is preserved. Double quotes do not change the rule:
// backslash-newline is removed inside them too.
func stripShellLineContinuations(command string) string {
	runes := []rune(command)
	var out strings.Builder
	out.Grow(len(command))
	state := shellUnquoted
	for index := 0; index < len(runes); index++ {
		r := runes[index]
		switch state {
		case shellSingleQuoted:
			if r == '\'' {
				state = shellUnquoted
			}
			out.WriteRune(r)
		case shellDoubleQuoted:
			switch r {
			case '\\':
				if index+1 < len(runes) {
					next := runes[index+1]
					if next == '\n' {
						index++ // drop the continuation
						continue
					}
					// Preserve the escape pair verbatim; the parser decides
					// which escapes are meaningful inside double quotes.
					out.WriteRune(r)
					out.WriteRune(next)
					index++
					continue
				}
				out.WriteRune(r)
			case '"':
				state = shellUnquoted
				out.WriteRune(r)
			default:
				out.WriteRune(r)
			}
		default:
			switch r {
			case '\\':
				if index+1 < len(runes) {
					next := runes[index+1]
					if next == '\n' {
						index++
						continue
					}
					out.WriteRune(r)
					out.WriteRune(next)
					index++
					continue
				}
				out.WriteRune(r)
			case '\'':
				state = shellSingleQuoted
				out.WriteRune(r)
			case '"':
				state = shellDoubleQuoted
				out.WriteRune(r)
			default:
				out.WriteRune(r)
			}
		}
	}
	return out.String()
}

// isShellControlRune reports whether r is an unquoted shell control character
// that can start or splice another command, and returns a short name for the
// error message. Newlines are handled separately because they also separate
// words in the lenient (allow-operators) path.
func isShellControlRune(r rune) bool {
	switch r {
	case ';', '|', '&', '<', '>', '(', ')':
		return true
	default:
		return false
	}
}
