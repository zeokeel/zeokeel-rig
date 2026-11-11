package terminal

// Internal test for the PTY and pipe spawn sites: both must pass the confined
// argv to the process launcher. It replaces the ptyStart hook so no real PTY is
// needed.

import (
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// confineCall 记录一次 Confine 调用。
type confineCall struct {
	argv   []string
	policy *sandbox.Policy
}

// recordingRunner 是记录调用的 sandbox.Runner（原样返回 argv）。
type recordingRunner struct {
	mu    sync.Mutex
	calls []confineCall
}

// Confine 实现 sandbox.Runner。
func (r *recordingRunner) Confine(argv []string, policy *sandbox.Policy) (sandbox.ConfinedCommand, error) {
	r.mu.Lock()
	r.calls = append(r.calls, confineCall{argv: slices.Clone(argv), policy: policy})
	r.mu.Unlock()
	return sandbox.ConfinedCommand{Argv: slices.Clone(argv), Enforcement: sandbox.EnforcementFull}, nil
}

// recordedArgvs 返回所有已记录的 argv。
func (r *recordingRunner) recordedArgvs() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, 0, len(r.calls))
	for _, call := range r.calls {
		out = append(out, call.argv)
	}
	return out
}

// TestTerminalOpenConfinesPTYAndPipeArgv 依次走 PTY 与 pipe 两个 spawn 站点：
// PTY 使用 shellPath -i，pipe 使用 shellPath，二者都先经过 Confine。
func TestTerminalOpenConfinesPTYAndPipeArgv(t *testing.T) {
	original := ptyStart
	var ptyArgv [][]string
	ptyStart = func(argv []string, _ string, _ []string, _, _ int) (*ptyEnd, process, error) {
		ptyArgv = append(ptyArgv, slices.Clone(argv))
		return nil, nil, errors.New("injected PTY failure")
	}
	t.Cleanup(func() { ptyStart = original })

	root := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithCommandAllowlist("sh"))
	runner := &recordingRunner{}
	manager := NewManager(
		WithTransport(TransportPTY),
		WithPolicy(policy),
		WithRunner(runner),
	)
	t.Cleanup(func() { _ = manager.CloseAll() })

	if _, _, _, err := manager.open("", "", "", "", ""); err != nil {
		t.Fatalf("open with PTY fallback = %v", err)
	}
	if len(ptyArgv) != 1 || !slices.Equal(ptyArgv[0], []string{"/bin/sh", "-i"}) {
		t.Fatalf("ptyStart argv = %q, want [[/bin/sh -i]]", ptyArgv)
	}
	want := [][]string{{"/bin/sh", "-i"}, {"/bin/sh"}}
	got := runner.recordedArgvs()
	if len(got) != len(want) {
		t.Fatalf("runner calls = %d, want %d: %q", len(got), len(want), got)
	}
	for index := range want {
		if !slices.Equal(got[index], want[index]) {
			t.Fatalf("runner call %d argv = %q, want %q", index, got[index], want[index])
		}
	}
}

// TestTerminalRunnerWithoutPolicyFailsClosed 证明挂接 runner 但管理器没有沙箱
// 策略时，Open 在启动任何进程之前失败，且不调用 Confine。
func TestTerminalRunnerWithoutPolicyFailsClosed(t *testing.T) {
	runner := &recordingRunner{}
	manager := NewManager(WithRunner(runner))
	t.Cleanup(func() { _ = manager.CloseAll() })
	if _, err := manager.Open(OpenOptions{}); err == nil {
		t.Fatal("Open succeeded without a policy")
	}
	if got := len(runner.recordedArgvs()); got != 0 {
		t.Fatalf("runner calls = %d, want 0 without a policy", got)
	}
}
