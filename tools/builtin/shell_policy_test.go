package builtin_test

// Tests for the shell-tool allowlist hardening: an allow-listed first command
// cannot chain another command with unquoted shell operators, while quoted
// operators stay literal text.

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/builtin"
)

func policyBashTool(t *testing.T, policy *sandbox.Policy, name string) rig.Tool {
	t.Helper()
	for _, tool := range builtin.NewWithPolicy(policy) {
		if tool.Name() == name {
			return tool
		}
	}
	t.Fatalf("%s tool not found", name)
	return nil
}

func invokeBash(t *testing.T, tool rig.Tool, command string) rig.ToolResult {
	t.Helper()
	result, err := tool.Invoke(context.Background(), rig.ToolCall{
		Name:      "bash",
		Arguments: `{"command":` + quoteJSON(t, command) + `}`,
	})
	if err != nil {
		t.Fatalf("Invoke(%q) error: %v", command, err)
	}
	return result
}

func quoteJSON(t *testing.T, value string) string {
	t.Helper()
	var builder strings.Builder
	builder.WriteByte('"')
	for _, r := range value {
		switch r {
		case '\\':
			builder.WriteString(`\\`)
		case '"':
			builder.WriteString(`\"`)
		case '\n':
			builder.WriteString(`\n`)
		default:
			builder.WriteRune(r)
		}
	}
	builder.WriteByte('"')
	return builder.String()
}

func TestBashPolicyRejectsChainedCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the bash tool runs /bin/sh")
	}
	root := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithCommandAllowlist("echo"))
	tool := policyBashTool(t, policy, "bash")

	for _, command := range []string{"echo hi; id", "echo hi && id", "echo hi | cat", "echo $(id)", "echo hi > out.txt"} {
		result := invokeBash(t, tool, command)
		if !result.IsError {
			t.Errorf("command %q was not rejected: %q", command, result.Content)
			continue
		}
		if strings.Contains(result.Content, "uid=") {
			t.Errorf("command %q executed the chained program: %q", command, result.Content)
		}
		if !strings.Contains(result.Content, "not allowed") {
			t.Errorf("command %q denial = %q, want a policy denial", command, result.Content)
		}
	}
}

func TestBashPolicyAllowsQuotedOperator(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the bash tool runs /bin/sh")
	}
	root := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithCommandAllowlist("echo"))
	tool := policyBashTool(t, policy, "bash")

	result := invokeBash(t, tool, `echo "a;b"`)
	if result.IsError {
		t.Fatalf("quoted operator rejected: %q", result.Content)
	}
	if !strings.Contains(result.Content, "a;b") {
		t.Fatalf("content = %q, want a;b", result.Content)
	}
}

func TestBashPolicyOptInAllowsOperators(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the bash tool runs /bin/sh")
	}
	root := t.TempDir()
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithCommandAllowlist("echo"),
		sandbox.WithAllowShellOperators(),
	)
	tool := policyBashTool(t, policy, "bash")

	result := invokeBash(t, tool, "echo hi | cat")
	if result.IsError {
		t.Fatalf("opt-in policy rejected the pipeline: %q", result.Content)
	}
	if !strings.Contains(result.Content, "hi") {
		t.Fatalf("content = %q, want hi", result.Content)
	}
}
