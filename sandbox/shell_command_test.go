package sandbox_test

// Tests for the strict shell-command check: unquoted control operators are
// rejected so a command allowlist cannot be escaped with `;`, `|`, `&&`, `$()`
// or redirects, while quoted/escaped operators stay literal text.

import (
	"errors"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

func TestCheckShellCommandRejectsUnquotedOperators(t *testing.T) {
	policy := sandbox.New(sandbox.WithCommandAllowlist("echo"))
	blocked := []string{
		"echo hi; id",
		"echo hi\nid",
		"echo hi && id",
		"echo hi | cat",
		"echo $(id)",
		"echo `id`",
		"echo hi > /tmp/written",
		"echo hi < /etc/passwd",
		"echo hi & id",
		"(id)",
		"echo \"unterminated",
	}
	for _, command := range blocked {
		if err := policy.CheckShellCommand(command); !errors.Is(err, sandbox.ErrDenied) {
			t.Errorf("CheckShellCommand(%q) = %v, want a sandbox.ErrDenied", command, err)
		}
	}
}

func TestCheckShellCommandAllowsQuotedAndEscapedOperators(t *testing.T) {
	policy := sandbox.New(sandbox.WithCommandAllowlist("echo"))
	allowed := []string{
		"echo hi",
		`echo "a;b"`,
		`echo 'a|b'`,
		`echo a\;b`,
		`echo '$('`,
		`echo "a && b"`,
	}
	for _, command := range allowed {
		if err := policy.CheckShellCommand(command); err != nil {
			t.Errorf("CheckShellCommand(%q) = %v, want nil", command, err)
		}
	}
}

func TestCheckShellCommandOptOut(t *testing.T) {
	policy := sandbox.New(
		sandbox.WithCommandAllowlist("echo"),
		sandbox.WithAllowShellOperators(),
	)
	if err := policy.CheckShellCommand("echo hi | cat"); err != nil {
		t.Fatalf("CheckShellCommand with WithAllowShellOperators = %v, want nil", err)
	}
	if !policy.AllowShellOperators() {
		t.Fatal("AllowShellOperators() = false, want true")
	}
	strict := sandbox.New(sandbox.WithCommandAllowlist("echo"))
	if strict.AllowShellOperators() {
		t.Fatal("AllowShellOperators() = true without the option")
	}
}

func TestCheckShellCommandNilPolicyDenies(t *testing.T) {
	var policy *sandbox.Policy
	if err := policy.CheckShellCommand("echo hi"); !errors.Is(err, sandbox.ErrDenied) {
		t.Fatalf("nil policy CheckShellCommand = %v, want ErrDenied", err)
	}
}
