package builtin_test

// Tests for wiring an OS-level sandbox.Runner into the builtin bash/pwsh
// tools. A fake runner proves three things: the confined argv (not the raw
// argv) is what spawns, a Confine error fails closed without spawning, and a
// nil runner keeps the existing behavior (covered by the other test files).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/builtin"
	"github.com/zeokeel/zeokeel-rig/tools/jobs"
)

// runnerCall 记录一次 Confine 调用。
type runnerCall struct {
	argv   []string
	policy *sandbox.Policy
}

// fakeRunner 是测试用 sandbox.Runner：记录每次调用，并按 rewrite 改写 argv。
// rewrite 为 nil 时原样返回 argv（用来证明“调用确实发生且被使用”）。
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

// wantArgv 断言一次调用的 argv 与 want 完全一致。
func wantArgv(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("runner argv = %q, want %q", got, want)
	}
}

// newRunnerRoot 返回策略模式的真实根与策略。
func newRunnerRoot(t *testing.T, allow ...string) (string, *sandbox.Policy) {
	t.Helper()
	root := policyRoot(t)
	return root, sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithCommandAllowlist(allow...),
	)
}

// TestBashConfinesArgvWithRunner 证明 bash 直接前台执行时 spawn 的是 runner
// 返回的 argv，而不是原始 /bin/sh -c command。
func TestBashConfinesArgvWithRunner(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("/bin/sh unavailable: %v", err)
	}
	_, policy := newRunnerRoot(t, "echo")
	runner := &fakeRunner{rewrite: func([]string) ([]string, error) {
		return []string{"/bin/sh", "-c", "echo confined"}, nil
	}}
	tools := policyToolMap(t, builtin.NewWithPolicyOptions(policy, builtin.WithRunner(runner)))

	result := invokePolicy(t, tools, builtin.ToolBash, `{"command":"echo original","workdir":"."}`)
	if result.IsError {
		t.Fatalf("bash = %q, want success", result.Content)
	}
	requireContains(t, result.Content, "confined")
	requireNotContains(t, result.Content, "original")

	if runner.callCount() != 1 {
		t.Fatalf("runner calls = %d, want 1", runner.callCount())
	}
	call := runner.callAt(t, 0)
	wantArgv(t, call.argv, "/bin/sh", "-c", "echo original")
	if call.policy != policy {
		t.Fatalf("runner policy = %p, want the tool policy %p", call.policy, policy)
	}
}

// TestBashRunnerFailureFailsClosed 证明 Confine 失败时 bash 返回工具错误且
// 子进程从未启动（标记文件不存在）。
func TestBashRunnerFailureFailsClosed(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("/bin/sh unavailable: %v", err)
	}
	root, policy := newRunnerRoot(t, "touch")
	runner := &fakeRunner{rewrite: func([]string) ([]string, error) {
		return nil, fmt.Errorf("confine: %w", sandbox.ErrSandboxUnavailable)
	}}
	tools := policyToolMap(t, builtin.NewWithPolicyOptions(policy, builtin.WithRunner(runner)))

	marker := filepath.Join(root, "must-not-exist")
	result := invokePolicy(t, tools, builtin.ToolBash, fmt.Sprintf(`{"command":"touch %s"}`, marker))
	if !result.IsError {
		t.Fatalf("bash = %q, want a fail-closed tool error", result.Content)
	}
	requireContains(t, result.Content, sandbox.ErrSandboxUnavailable.Error())
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("marker stat = %v, want it not to exist (no spawn)", err)
	}
	if runner.callCount() != 1 {
		t.Fatalf("runner calls = %d, want 1", runner.callCount())
	}
}

// TestBashBackgroundUsesConfinedArgvAndDoesNotDoubleWrap 证明后台路径使用
// bash 侧的受限 argv，并且 PolicyChecked=true 使 jobs.Manager 不再二次包装。
func TestBashBackgroundUsesConfinedArgvAndDoesNotDoubleWrap(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("/bin/sh unavailable: %v", err)
	}
	_, policy := newRunnerRoot(t, "echo")
	bashRunner := &fakeRunner{rewrite: func([]string) ([]string, error) {
		return []string{"/bin/sh", "-c", "echo confined-bg"}, nil
	}}
	jobRunner := &fakeRunner{rewrite: func([]string) ([]string, error) {
		return nil, fmt.Errorf("jobs must not re-confine: %w", sandbox.ErrSandboxUnavailable)
	}}
	manager := jobs.NewManager(jobs.WithPolicy(policy), jobs.WithRunner(jobRunner))
	t.Cleanup(func() { _ = manager.CloseAll() })
	jobTools := make(map[string]rig.Tool)
	for _, tool := range manager.Tools() {
		jobTools[tool.Name()] = tool
	}
	tools := policyToolMap(t, builtin.NewWithPolicyOptions(
		policy,
		builtin.WithBackgroundJobs(manager),
		builtin.WithRunner(bashRunner),
	))

	result := invokePolicy(t, tools, builtin.ToolBash, `{"command":"echo original","run_in_background":true}`)
	if result.IsError {
		t.Fatalf("bash(run_in_background) = %q", result.Content)
	}
	jobID := backgroundJobID(t, result.Content)
	output := invokeBackgroundJob(t, jobTools, jobs.ToolJobOutput,
		`{"job_id":"`+jobID+`","wait":true,"timeout_ms":5000}`)
	if output.IsError {
		t.Fatalf("job_output = %q", output.Content)
	}
	// 注意：job_output 的 command 元数据仍显示原始命令文本；这里只断言真正
	// 执行的输出来自受限 argv。
	requireContains(t, output.Content, "confined-bg")

	if bashRunner.callCount() != 1 {
		t.Fatalf("bash runner calls = %d, want 1", bashRunner.callCount())
	}
	wantArgv(t, bashRunner.callAt(t, 0).argv, "/bin/sh", "-c", "echo original")
	if jobRunner.callCount() != 0 {
		t.Fatalf("jobs runner calls = %d, want 0 (PolicyChecked must skip re-wrapping)", jobRunner.callCount())
	}
}

// TestPwshConfinesArgvWithRunner 证明 pwsh 与 bash 一样在 spawn 前包装 argv。
func TestPwshConfinesArgvWithRunner(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("/bin/sh unavailable: %v", err)
	}
	_, policy := newRunnerRoot(t, "sh")
	runner := &fakeRunner{rewrite: func([]string) ([]string, error) {
		return []string{"/bin/sh", "-c", "echo confined-pwsh"}, nil
	}}
	tool := builtin.NewPwshWithConfig(builtin.PwshConfig{
		Policy: policy,
		Binary: "/bin/sh",
		Runner: runner,
	})

	result, err := tool.Invoke(context.Background(), rig.ToolCall{
		ID: "pwsh-1", Name: builtin.ToolPwsh, Arguments: `{"command":"Write-Output original"}`,
	})
	if err != nil {
		t.Fatalf("pwsh.Invoke returned Go error: %v", err)
	}
	if result.IsError {
		t.Fatalf("pwsh = %q, want success", result.Content)
	}
	requireContains(t, result.Content, "confined-pwsh")
	requireNotContains(t, result.Content, "original")
	if runner.callCount() != 1 {
		t.Fatalf("runner calls = %d, want 1", runner.callCount())
	}
	call := runner.callAt(t, 0)
	if len(call.argv) != 5 || call.argv[0] != "/bin/sh" || call.argv[1] != "-NoProfile" {
		t.Fatalf("runner argv = %q, want [/bin/sh -NoProfile -NonInteractive -Command Write-Output original]", call.argv)
	}
}

// TestPwshRunnerFailureFailsClosed 证明 pwsh 在 Confine 失败时不启动进程。
func TestPwshRunnerFailureFailsClosed(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("/bin/sh unavailable: %v", err)
	}
	root, policy := newRunnerRoot(t, "sh")
	runner := &fakeRunner{rewrite: func([]string) ([]string, error) {
		return nil, sandbox.ErrSandboxUnavailable
	}}
	tool := builtin.NewPwshWithConfig(builtin.PwshConfig{
		Policy: policy,
		Binary: "/bin/sh",
		Runner: runner,
	})
	marker := filepath.Join(root, "pwsh-must-not-exist")
	result, err := tool.Invoke(context.Background(), rig.ToolCall{
		ID: "pwsh-2", Name: builtin.ToolPwsh,
		Arguments: fmt.Sprintf(`{"command":"Write-Output %s"}`, marker),
	})
	if err != nil {
		t.Fatalf("pwsh.Invoke returned Go error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("pwsh = %q, want a fail-closed tool error", result.Content)
	}
	requireContains(t, result.Content, sandbox.ErrSandboxUnavailable.Error())
	if runner.callCount() != 1 {
		t.Fatalf("runner calls = %d, want 1", runner.callCount())
	}
}

// TestRunnerWithoutPolicyFailsClosed 证明挂接 runner 但工具没有沙箱策略时
// fail closed：不调用 Confine，也不启动未受限命令。
func TestRunnerWithoutPolicyFailsClosed(t *testing.T) {
	runner := &fakeRunner{}
	tool := builtin.NewShellWithOptions(t.TempDir(), time.Second, builtin.WithRunner(runner))
	result, err := tool.Invoke(context.Background(), rig.ToolCall{
		Name:      "bash",
		Arguments: `{"command":"echo should-not-run","description":"should not run"}`,
	})
	if err == nil && !result.IsError {
		t.Fatalf("bash succeeded without a policy: %q", result.Content)
	}
	if !strings.Contains(result.Content, "requires a sandbox policy") {
		t.Fatalf("error = %q, want it to mention the missing policy", result.Content)
	}
	if runner.callCount() != 0 {
		t.Fatalf("runner calls = %d, want 0 without a policy", runner.callCount())
	}
}
