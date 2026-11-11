package kit_test

import (
	"context"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/gate"
	"github.com/zeokeel/zeokeel-rig/kit"
	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// kitRunner is a sandbox.Runner test double for the kit wiring test.
type kitRunner struct {
	calls   [][]string
	rewrite []string
}

func (r *kitRunner) Confine(argv []string, policy *sandbox.Policy) (sandbox.ConfinedCommand, error) {
	r.calls = append(r.calls, append([]string(nil), argv...))
	target := r.rewrite
	if target == nil {
		target = argv
	}
	return sandbox.ConfinedCommand{Argv: append([]string(nil), target...), Enforcement: sandbox.EnforcementFull}, nil
}

// TestSandboxRunnerWiredIntoDefaultTools proves kit.Config.SandboxRunner reaches
// the default full.All tool set: a model-requested bash call runs through the
// runner's rewritten argv.
func TestSandboxRunnerWiredIntoDefaultTools(t *testing.T) {
	dir := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(dir), sandbox.WithCommandAllowlist("echo"))
	runner := &kitRunner{rewrite: []string{"/bin/sh", "-c", "echo confined"}}
	provider := &scriptProvider{scripts: [][]kit.Chunk{
		toolCallScript(kit.ToolCall{
			ID:        "call-1",
			Name:      "bash",
			Arguments: `{"command":"echo hi","description":"say hi"}`,
		}),
		textScript("done"),
	}}

	a, err := kit.New(kit.Config{
		Model:         "test-model",
		Provider:      provider,
		Root:          dir,
		Policy:        policy,
		SandboxRunner: runner,
		Gate:          gate.AllowAll(),
	})
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	text, _, err := a.RunText(context.Background(), "sess", "please run it", nil)
	if err != nil {
		t.Fatalf("RunText: %v", err)
	}
	if text != "done" {
		t.Fatalf("RunText text = %q, want %q", text, "done")
	}
	if len(runner.calls) == 0 {
		t.Fatal("SandboxRunner.Confine was not called by the default tool set")
	}
	if got, want := strings.Join(runner.calls[0], " "), "/bin/sh -c echo hi"; got != want {
		t.Fatalf("Confine argv = %q, want %q", got, want)
	}
}

// TestSandboxRunnerAbsentKeepsKitBehavior proves a nil runner leaves the default
// path working.
func TestSandboxRunnerAbsentKeepsKitBehavior(t *testing.T) {
	dir := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(dir), sandbox.WithCommandAllowlist("echo"))
	provider := &scriptProvider{scripts: [][]kit.Chunk{
		toolCallScript(kit.ToolCall{
			ID:        "call-1",
			Name:      "bash",
			Arguments: `{"command":"echo plain","description":"say plain"}`,
		}),
		textScript("done"),
	}}

	a, err := kit.New(kit.Config{
		Model:    "test-model",
		Provider: provider,
		Root:     dir,
		Policy:   policy,
		Gate:     gate.AllowAll(),
	})
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	if _, _, err := a.RunText(context.Background(), "sess", "please run it", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	second := provider.request(1)
	if len(second.Messages) == 0 {
		t.Fatal("second request has no messages")
	}
	found := false
	for _, message := range second.Messages {
		for _, part := range message.Content {
			if strings.Contains(part.Text, "plain") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("the bash result did not reach the model history")
	}
}
