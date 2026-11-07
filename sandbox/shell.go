package sandbox

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// Shell parsing and the shell-command policy.
//
// Rig's command allowlist is an in-process policy layer, not a security
// boundary: it exists so a host can constrain what a cooperative model is
// allowed to run, while the approval Gate (and, for untrusted models, an
// OS-level sandbox) remains the real boundary.
//
// A policy still has to agree with the shell about what a command means.
// Rather than re-implement POSIX quoting (previous revisions shipped three
// copies of a hand-written splitter and still missed backslash-newline
// continuations), the policy uses the industry-standard mvdan.cc/sh parser for
// the shell grammar. The one piece that parser handles differently from
// /bin/sh is removal of backslash-newline continuations before tokenization,
// so stripShellLineContinuations performs that step first, exactly like the
// shell (outside single quotes).
//
// In strict (default) mode the AST is then checked for a single simple command
// and every construct that can start or splice another program is rejected:
// command substitution (including inside double quotes, parameter expansions
// and arithmetic), backtick substitution, process substitution, pipelines,
// lists (; / && / ||), subshells, redirections, background jobs, function
// definitions and control structures. Pure arithmetic expansion is allowed
// (command substitutions inside it are still rejected by the walk).
//
// With WithAllowShellOperators the policy does not reject shell syntax; the
// first simple command's words are returned so the allowlist can still be
// applied (advisory-only in that mode).

// ParseShellCommand validates command against the shell-syntax policy and
// returns its words (argv) for the command allow/deny check. It is the single
// parser used by the built-in bash, pwsh, jobs and terminal tools, so the
// validation and the argv[0] check can never disagree about quoting.
//
// Callers must still apply CheckCommand to the returned argv and must treat the
// result as policy input, not as a security boundary: the approval Gate is the
// boundary for irreversible actions, and an OS-level sandbox is required when
// the model is untrusted; run the host in a container/VM for OS isolation.
//
// With WithAllowShellOperators the policy does not reject shell syntax; the
// command is parsed and the first simple command's words are returned (the
// allowlist is then advisory-only).
func (p *Policy) ParseShellCommand(command string) ([]string, error) {
	if p == nil {
		return nil, deny("nil policy")
	}
	normalized := stripShellLineContinuations(command)
	file, err := syntax.NewParser(syntax.Variant(syntax.LangPOSIX)).Parse(strings.NewReader(normalized), "")
	if err != nil {
		return nil, deny("shell command is not valid POSIX sh: %v", err)
	}
	if p.allowShellOperators {
		return firstCommandWords(file, normalized), nil
	}
	return validateSimpleCommand(file, normalized)
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
//   - unquoted shell control operators: ;, newlines, |, &, &&, ||, redirects;
//   - command substitution `$(...)` and backticks, including inside double
//     quotes, parameter expansions and arithmetic expressions;
//   - process substitution `<(...)` and `>(...)`;
//   - line continuations are removed before parsing (the policy models what
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
	_, err := p.ParseShellCommand(command)
	return err
}

// validateSimpleCommand enforces the strict policy on a parsed POSIX file and
// returns the words of its single simple command.
func validateSimpleCommand(file *syntax.File, source string) ([]string, error) {
	if file == nil {
		return nil, deny("shell command is empty")
	}
	if len(file.Stmts) == 0 {
		return nil, nil // empty or comment-only command; CheckCommand rejects empty argv
	}
	if len(file.Stmts) > 1 {
		return nil, deny("shell command must be a single simple command: compound commands and separators (;, &&, ||, newline) are not allowed (use WithAllowShellOperators to opt out)")
	}
	stmt := file.Stmts[0]
	if stmt.Negated {
		return nil, deny("shell command negation (!) is not allowed (use WithAllowShellOperators to opt out)")
	}
	if stmt.Background {
		return nil, deny("background shell commands (&) are not allowed (use WithAllowShellOperators to opt out)")
	}
	if stmt.Coprocess {
		return nil, deny("coprocess shell commands are not allowed (use WithAllowShellOperators to opt out)")
	}
	if len(stmt.Redirs) > 0 {
		return nil, deny("shell redirections are not allowed (use WithAllowShellOperators to opt out)")
	}
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok {
		return nil, deny("shell command must be a simple command: pipes, subshells, blocks, tests and control structures are not allowed (use WithAllowShellOperators to opt out)")
	}
	if len(call.Assigns) > 0 {
		return nil, deny("environment variable assignments before the command are not allowed in sandboxed commands")
	}
	if len(call.Args) == 0 {
		return nil, nil
	}
	if err := rejectSubstitutions(file); err != nil {
		return nil, err
	}
	argv := make([]string, 0, len(call.Args))
	for index, word := range call.Args {
		value, static := decodeShellWord(word, source)
		if index == 0 && !static {
			return nil, deny("the command name must be a literal word (expansions in argv[0] cannot be checked against the allowlist)")
		}
		argv = append(argv, value)
	}
	return argv, nil
}

// rejectSubstitutions walks the whole AST and rejects every construct that can
// run another program, wherever it appears (double quotes, parameter expansions,
// arithmetic expressions, here-strings, ...).
func rejectSubstitutions(file *syntax.File) error {
	var found error
	syntax.Walk(file, func(node syntax.Node) bool {
		switch node.(type) {
		case *syntax.CmdSubst:
			found = deny("command substitution $(...) and backticks are not allowed (use WithAllowShellOperators to opt out)")
			return false
		case *syntax.ProcSubst:
			found = deny("process substitution <(...) and >(...) are not allowed (use WithAllowShellOperators to opt out)")
			return false
		}
		return true
	})
	return found
}

// firstCommandWords returns the words of the first simple command in the file.
// It is used in the WithAllowShellOperators opt-out mode, where the allowlist
// is advisory and only argv[0] matters.
func firstCommandWords(file *syntax.File, source string) []string {
	if file == nil {
		return nil
	}
	var call *syntax.CallExpr
	syntax.Walk(file, func(node syntax.Node) bool {
		if call != nil {
			return false
		}
		if candidate, ok := node.(*syntax.CallExpr); ok {
			call = candidate
			return false
		}
		return true
	})
	if call == nil || len(call.Args) == 0 {
		return nil
	}
	argv := make([]string, 0, len(call.Args))
	for _, word := range call.Args {
		value, _ := decodeShellWord(word, source)
		argv = append(argv, value)
	}
	return argv
}

// stripShellLineContinuations removes POSIX line continuations (backslash
// followed by newline) outside single quotes, mirroring the shell's
// pre-tokenization processing. Inside single quotes every byte is literal, so a
// backslash-newline there is preserved. Double quotes do not change the rule:
// backslash-newline is removed inside them too.
func stripShellLineContinuations(command string) string {
	if !strings.Contains(command, "\\\n") {
		return command
	}
	var out strings.Builder
	out.Grow(len(command))
	state := shellUnquoted
	runes := []rune(command)
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
				if index+1 < len(runes) && runes[index+1] == '\n' {
					index++
					continue
				}
				out.WriteRune(r)
				if index+1 < len(runes) {
					out.WriteRune(runes[index+1])
					index++
				}
			case '"':
				state = shellUnquoted
				out.WriteRune(r)
			default:
				out.WriteRune(r)
			}
		default:
			switch r {
			case '\\':
				if index+1 < len(runes) && runes[index+1] == '\n' {
					index++
					continue
				}
				out.WriteRune(r)
				if index+1 < len(runes) {
					out.WriteRune(runes[index+1])
					index++
				}
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

// shellQuoteState tracks enough context to apply the line-continuation rule.
type shellQuoteState int

const (
	shellUnquoted shellQuoteState = iota
	shellSingleQuoted
	shellDoubleQuoted
)

// decodeShellWord returns the shell word's value and whether it is static (no
// expansions). Static words are unquoted/un-escaped for the allowlist check;
// non-static words are returned as their source text so callers see what was
// written without having to expand it.
func decodeShellWord(word *syntax.Word, source string) (string, bool) {
	if word == nil {
		return "", true
	}
	var builder strings.Builder
	static := true
	for _, part := range word.Parts {
		switch typed := part.(type) {
		case *syntax.Lit:
			builder.WriteString(unescapeShellLit(typed.Value, false))
		case *syntax.SglQuoted:
			if typed.Dollar {
				static = false // $'...' performs escapes; keep the decision conservative
			}
			builder.WriteString(typed.Value)
		case *syntax.DblQuoted:
			for _, inner := range typed.Parts {
				lit, ok := inner.(*syntax.Lit)
				if !ok {
					static = false
					continue
				}
				builder.WriteString(unescapeShellLit(lit.Value, true))
			}
		default:
			static = false
		}
	}
	if static {
		return builder.String(), true
	}
	if word.Pos().IsValid() && word.End().IsValid() {
		start, end := int(word.Pos().Offset()), int(word.End().Offset())
		if start >= 0 && end <= len(source) && start < end {
			return source[start:end], false
		}
	}
	return builder.String(), false
}

// unescapeShellLit removes POSIX backslash quoting from a literal fragment.
// Inside double quotes only $, `, ", \ and newline lose their backslash;
// elsewhere every character loses it.
func unescapeShellLit(raw string, inDoubleQuotes bool) string {
	if !strings.Contains(raw, "\\") {
		return raw
	}
	var out strings.Builder
	out.Grow(len(raw))
	for index := 0; index < len(raw); index++ {
		char := raw[index]
		if char != '\\' || index+1 >= len(raw) {
			out.WriteByte(char)
			continue
		}
		next := raw[index+1]
		if inDoubleQuotes {
			switch next {
			case '$', '`', '"', '\\':
				out.WriteByte(next)
				index++
				continue
			default:
				out.WriteByte(char)
				continue
			}
		}
		out.WriteByte(next)
		index++
	}
	return out.String()
}
