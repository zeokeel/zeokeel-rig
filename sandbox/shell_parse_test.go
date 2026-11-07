package sandbox_test

// Tests for the shared POSIX-shell word parser (ParseShellCommand). The parser
// is the single source of truth for shell validation and argv[0] extraction;
// these tests pin the quoting/escaping/line-continuation semantics and the
// regression corpus from the v0.9.0-v0.9.3 audits (quoted substitution,
// backslash-newline continuation).

import (
	"errors"
	"reflect"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

func TestParseShellCommandWords(t *testing.T) {
	policy := sandbox.New(sandbox.WithCommandAllowlist("echo"))
	cases := []struct {
		command string
		want    []string
	}{
		{`echo hi`, []string{"echo", "hi"}},
		{`echo   a   b`, []string{"echo", "a", "b"}},
		{`echo "a;b"`, []string{"echo", "a;b"}},
		{`echo 'a|b'`, []string{"echo", "a|b"}},
		{`echo a\;b`, []string{"echo", "a;b"}},
		{`echo a\ b`, []string{"echo", "a b"}},
		{`echo ""`, []string{"echo", ""}},
		{`echo "a\"b"`, []string{"echo", `a"b`}},
		{`echo 'it'\''s'`, []string{"echo", "it's"}},
		{"echo \"a\\\nb\"", []string{"echo", "ab"}},
		{"echo a\\\nb", []string{"echo", "ab"}},
		{"echo hi # ; ignored", []string{"echo", "hi"}},
	}
	for _, tc := range cases {
		got, err := policy.ParseShellCommand(tc.command)
		if err != nil {
			t.Errorf("ParseShellCommand(%q) error = %v", tc.command, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ParseShellCommand(%q) = %#v, want %#v", tc.command, got, tc.want)
		}
	}
}

func TestParseShellCommandRejectsContinuationSubstitution(t *testing.T) {
	policy := sandbox.New(sandbox.WithCommandAllowlist("echo", "git"))
	blocked := []string{
		// v0.9.3 residual: backslash-newline is removed by /bin/sh inside
		// double quotes, so the policy must remove it before looking for $(.
		"echo \"$\\\n(id)\"",
		"echo \"${x:-$\\\n(id)}\"",
		"echo $\\\n(id)",
		"echo hi \\\n| id",
		"echo hi \\\n; id",
		// Plain quoted substitutions and operators stay rejected.
		`echo "$(id)"`,
		"echo \"`id`\"",
		`echo ${x:-$(id)}`,
		"echo hi; id",
		"(id)",
		`cat <(id)`,
		`cat >(id)`,
	}
	for _, command := range blocked {
		if _, err := policy.ParseShellCommand(command); !errors.Is(err, sandbox.ErrDenied) {
			t.Errorf("ParseShellCommand(%q) = %v, want sandbox.ErrDenied", command, err)
		}
	}
}

func TestParseShellCommandLineContinuationInDoubleQuotes(t *testing.T) {
	policy := sandbox.New(sandbox.WithCommandAllowlist("echo"))
	got, err := policy.ParseShellCommand("echo \"a\\\nb\"")
	if err != nil {
		t.Fatalf("ParseShellCommand: %v", err)
	}
	want := []string{"echo", "ab"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("words = %#v, want %#v", got, want)
	}
}

func TestParseShellCommandAllowOperatorsMode(t *testing.T) {
	policy := sandbox.New(
		sandbox.WithCommandAllowlist("echo"),
		sandbox.WithAllowShellOperators(),
	)
	// With the opt-out the parser must still split words, but must not reject
	// the pipeline itself (the allowlist is advisory in that mode).
	got, err := policy.ParseShellCommand("echo hi | cat")
	if err != nil {
		t.Fatalf("ParseShellCommand with WithAllowShellOperators = %v", err)
	}
	if len(got) == 0 || got[0] != "echo" {
		t.Fatalf("words = %#v, want first word echo", got)
	}
	if err := policy.CheckShellCommand("echo hi | cat"); err != nil {
		t.Fatalf("CheckShellCommand with WithAllowShellOperators = %v", err)
	}
}

func TestParseShellCommandUnterminated(t *testing.T) {
	policy := sandbox.New(sandbox.WithCommandAllowlist("echo"))
	for _, command := range []string{`echo "x`, `echo 'x`} {
		if _, err := policy.ParseShellCommand(command); !errors.Is(err, sandbox.ErrDenied) {
			t.Errorf("ParseShellCommand(%q) = %v, want sandbox.ErrDenied", command, err)
		}
	}
}

func TestParseShellCommandEmpty(t *testing.T) {
	policy := sandbox.New(sandbox.WithCommandAllowlist("echo"))
	words, err := policy.ParseShellCommand("   # only a comment")
	if err != nil {
		t.Fatalf("ParseShellCommand(comment) = %v", err)
	}
	if len(words) != 0 {
		t.Fatalf("words = %#v, want none", words)
	}
}
