package full

import (
	"context"
	"errors"
	"strings"
	"testing"

	rig "github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// confinedCall records one Runner.Confine invocation.
type confinedCall struct {
	argv   []string
	policy *sandbox.Policy
}

// fakeRunner is a sandbox.Runner test double: it records calls, can rewrite the
// argv, and can fail closed.
type fakeRunner struct {
	calls   []confinedCall
	rewrite []string
	err     error
}

func (r *fakeRunner) Confine(argv []string, policy *sandbox.Policy) (sandbox.ConfinedCommand, error) {
	r.calls = append(r.calls, confinedCall{argv: append([]string(nil), argv...), policy: policy})
	if r.err != nil {
		return sandbox.ConfinedCommand{}, r.err
	}
	target := r.rewrite
	if target == nil {
		target = argv
	}
	return sandbox.ConfinedCommand{Argv: append([]string(nil), target...), Enforcement: sandbox.EnforcementFull}, nil
}

// findTool returns the tool with the given name, or fails.
func findTool(t *testing.T, tools []rig.Tool, name string) rig.Tool {
	t.Helper()
	for _, tool := range tools {
		if tool.Name() == name {
			return tool
		}
	}
	t.Fatalf("tool %q not found", name)
	return nil
}

// TestAllWiresSandboxRunnerIntoBash proves AllOptions.SandboxRunner reaches the
// bash tool: the confined argv is the one that runs.
func TestAllWiresSandboxRunnerIntoBash(t *testing.T) {
	dir := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(dir), sandbox.WithCommandAllowlist("echo"))
	runner := &fakeRunner{rewrite: []string{"/bin/sh", "-c", "echo confined"}}

	tools := All(AllOptions{Policy: policy, Root: dir, SandboxRunner: runner})
	bash := findTool(t, tools, "bash")
	result, err := bash.Invoke(context.Background(), rig.ToolCall{
		Name:      "bash",
		Arguments: `{"command":"echo hi","description":"say hi"}`,
	})
	if err != nil {
		t.Fatalf("bash invoke: %v", err)
	}
	if result.IsError {
		t.Fatalf("bash result is an error: %s", result.Content)
	}
	if !strings.Contains(result.Content, "confined") {
		t.Fatalf("bash output = %q, want the confined argv output", result.Content)
	}
	if len(runner.calls) == 0 {
		t.Fatal("SandboxRunner.Confine was not called")
	}
	if got, want := strings.Join(runner.calls[0].argv, " "), "/bin/sh -c echo hi"; got != want {
		t.Fatalf("Confine argv = %q, want %q", got, want)
	}
	if runner.calls[0].policy == nil {
		t.Fatal("Confine policy is nil")
	}
}

// TestAllSandboxRunnerFailureFailsClosed proves a Confine error prevents the
// command from running.
func TestAllSandboxRunnerFailureFailsClosed(t *testing.T) {
	dir := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(dir), sandbox.WithCommandAllowlist("echo"))
	runner := &fakeRunner{err: sandbox.ErrSandboxUnavailable}

	tools := All(AllOptions{Policy: policy, Root: dir, SandboxRunner: runner})
	bash := findTool(t, tools, "bash")
	result, err := bash.Invoke(context.Background(), rig.ToolCall{
		Name:      "bash",
		Arguments: `{"command":"echo should-not-run","description":"should not run"}`,
	})
	if err == nil && !result.IsError {
		t.Fatalf("bash succeeded despite a Confine error: %q", result.Content)
	}
	if strings.Contains(result.Content, "should-not-run") {
		t.Fatalf("command output leaked: %q", result.Content)
	}
	if !errors.Is(runner.err, sandbox.ErrSandboxUnavailable) {
		t.Fatal("test setup lost the sentinel error")
	}
}

// TestAllNilSandboxRunnerKeepsBehavior proves the default path still runs.
func TestAllNilSandboxRunnerKeepsBehavior(t *testing.T) {
	dir := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(dir), sandbox.WithCommandAllowlist("echo"))

	tools := All(AllOptions{Policy: policy, Root: dir})
	bash := findTool(t, tools, "bash")
	result, err := bash.Invoke(context.Background(), rig.ToolCall{
		Name:      "bash",
		Arguments: `{"command":"echo plain","description":"say plain"}`,
	})
	if err != nil {
		t.Fatalf("bash invoke: %v", err)
	}
	if result.IsError || !strings.Contains(result.Content, "plain") {
		t.Fatalf("bash output = %q (isError=%v), want plain output", result.Content, result.IsError)
	}
}
