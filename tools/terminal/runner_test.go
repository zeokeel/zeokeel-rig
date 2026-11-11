package terminal_test

// Tests for wiring an OS-level sandbox.Runner into the terminal manager: the
// shell argv is confined before spawning and a Confine error fails the session
// creation before any process starts.

import (
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/terminal"
)

// runnerCall 记录一次 Confine 调用。
type runnerCall struct {
	argv   []string
	policy *sandbox.Policy
}

// fakeRunner 是测试用 sandbox.Runner：记录调用并按 rewrite 改写 argv。
type fakeRunner struct {
	mu      sync.Mutex
	calls   []runnerCall
	rewrite func(argv []string) ([]string, error)
}

// Confine 实现 sandbox.Runner。
func (r *fakeRunner) Confine(argv []string, policy *sandbox.Policy) (sandbox.ConfinedCommand, error) {
	r.mu.Lock()
	r.calls = append(r.calls, runnerCall{argv: append([]string(nil), argv...), policy: policy})
	r.mu.Unlock()
	confined := append([]string(nil), argv...)
	if r.rewrite != nil {
		rewritten, err := r.rewrite(confined)
		if err != nil {
			return sandbox.ConfinedCommand{}, err
		}
		confined = rewritten
	}
	return sandbox.ConfinedCommand{Argv: confined, Enforcement: sandbox.EnforcementFull}, nil
}

// callCount 返回 Confine 调用次数。
func (r *fakeRunner) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// callAt 返回第 index 次调用的记录。
func (r *fakeRunner) callAt(t *testing.T, index int) runnerCall {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if index >= len(r.calls) {
		t.Fatalf("runner calls = %d, want at least %d", len(r.calls), index+1)
	}
	return r.calls[index]
}

// TestTerminalOpenConfinesShellArgv 证明 pipe 会话在 spawn 前用 runner 包装
// 了 shell argv，且会话随后仍能正常工作。
func TestTerminalOpenConfinesShellArgv(t *testing.T) {
	root := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithCommandAllowlist("sh", "echo"))
	runner := &fakeRunner{}
	manager := terminal.NewManager(
		terminal.WithTransport(terminal.TransportPipe),
		terminal.WithPolicy(policy),
		terminal.WithRunner(runner),
	)
	t.Cleanup(func() { _ = manager.CloseAll() })

	info, err := manager.Open(terminal.OpenOptions{Cwd: root})
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	if info.ID == "" {
		t.Fatal("Open returned an empty session id")
	}
	if runner.callCount() != 1 {
		t.Fatalf("runner calls = %d, want 1", runner.callCount())
	}
	call := runner.callAt(t, 0)
	if len(call.argv) != 1 || call.argv[0] != "/bin/sh" {
		t.Fatalf("runner argv = %q, want [/bin/sh]", call.argv)
	}
	if call.policy != policy {
		t.Fatalf("runner policy = %p, want manager policy %p", call.policy, policy)
	}
}

// TestTerminalOpenRunnerFailureFailsClosed 证明 Confine 失败时 terminal_open
// 在任何进程启动前失败，且不登记会话。
func TestTerminalOpenRunnerFailureFailsClosed(t *testing.T) {
	root := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithCommandAllowlist("sh"))
	runner := &fakeRunner{rewrite: func([]string) ([]string, error) {
		return nil, sandbox.ErrSandboxUnavailable
	}}
	manager := terminal.NewManager(
		terminal.WithTransport(terminal.TransportPipe),
		terminal.WithPolicy(policy),
		terminal.WithRunner(runner),
	)
	t.Cleanup(func() { _ = manager.CloseAll() })

	if _, err := manager.Open(terminal.OpenOptions{Cwd: root}); err == nil {
		t.Fatal("Open = nil error, want fail-closed")
	}
	if manager.Len() != 0 {
		t.Fatalf("manager.Len = %d, want 0 (no session registered)", manager.Len())
	}
	if runner.callCount() != 1 {
		t.Fatalf("runner calls = %d, want 1", runner.callCount())
	}
}

// TestTerminalRunnerNilKeepsBehavior 证明未挂接 runner 时创建会话的行为不变。
func TestTerminalRunnerNilKeepsBehavior(t *testing.T) {
	root := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithCommandAllowlist("sh"))
	manager := terminal.NewManager(
		terminal.WithTransport(terminal.TransportPipe),
		terminal.WithPolicy(policy),
	)
	t.Cleanup(func() { _ = manager.CloseAll() })
	if _, err := manager.Open(terminal.OpenOptions{Cwd: root}); err != nil {
		t.Fatalf("Open without a runner = %v, want the existing behavior", err)
	}
}
