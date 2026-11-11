package jobs_test

// Tests for wiring an OS-level sandbox.Runner into the background job manager:
// job_start confines argv exactly once, a Confine error fails the job start
// without spawning, and producer-checked specs (PolicyChecked) are never
// re-confined.

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

	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/internal/background"
	"github.com/zeokeel/zeokeel-rig/tools/jobs"
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

// newRunnerPolicy 返回一个有临时根、允许指定命令的策略。
func newRunnerPolicy(t *testing.T, root string, allow ...string) *sandbox.Policy {
	t.Helper()
	if root == "" {
		root = t.TempDir()
	}
	return sandbox.New(sandbox.WithRoots(root), sandbox.WithCommandAllowlist(allow...))
}

// TestJobStartUsesConfinedArgv 证明 job_start 的后台命令使用 runner 返回的
// argv 执行。
func TestJobStartUsesConfinedArgv(t *testing.T) {
	root := t.TempDir()
	policy := newRunnerPolicy(t, root, "echo")
	runner := &fakeRunner{rewrite: func([]string) ([]string, error) {
		return []string{"/bin/sh", "-c", "echo wrapped-job"}, nil
	}}
	_, tools := newJobTools(t, jobs.WithPolicy(policy), jobs.WithRunner(runner))

	jobID := startJob(t, tools, fmt.Sprintf(`{"command":"echo original","cwd":%q}`, root))
	output := invokeJob(t, tools, jobs.ToolJobOutput,
		`{"job_id":"`+jobID+`","wait":true,"timeout_ms":5000}`)
	if output.IsError {
		t.Fatalf("job_output = %q", output.Content)
	}
	if !strings.Contains(output.Content, "wrapped-job") {
		t.Fatalf("job output = %q, want wrapped-job", output.Content)
	}
	if runner.callCount() != 1 {
		t.Fatalf("runner calls = %d, want 1", runner.callCount())
	}
	call := runner.callAt(t, 0)
	if strings.Join(call.argv, "\x00") != strings.Join([]string{"/bin/sh", "-c", "echo original"}, "\x00") {
		t.Fatalf("runner argv = %q, want [/bin/sh -c echo original]", call.argv)
	}
	if call.policy != policy {
		t.Fatalf("runner policy = %p, want manager policy %p", call.policy, policy)
	}
}

// TestJobStartRunnerFailureFailsClosed 证明 Confine 失败时 job_start 返回工具
// 错误、不登记任务、也不启动子进程。
func TestJobStartRunnerFailureFailsClosed(t *testing.T) {
	root := t.TempDir()
	policy := newRunnerPolicy(t, root, "touch")
	runner := &fakeRunner{rewrite: func([]string) ([]string, error) {
		return nil, fmt.Errorf("confine: %w", sandbox.ErrSandboxUnavailable)
	}}
	manager, tools := newJobTools(t, jobs.WithPolicy(policy), jobs.WithRunner(runner))

	marker := filepath.Join(root, "must-not-exist")
	result := invokeJob(t, tools, jobs.ToolJobStart, fmt.Sprintf(`{"command":"touch %s","cwd":%q}`, marker, root))
	if !result.IsError {
		t.Fatalf("job_start = %q, want a fail-closed tool error", result.Content)
	}
	if !strings.Contains(result.Content, sandbox.ErrSandboxUnavailable.Error()) {
		t.Fatalf("job_start error = %q, want it to mention %q", result.Content, sandbox.ErrSandboxUnavailable)
	}
	if manager.Len() != 0 {
		t.Fatalf("manager.Len = %d, want 0 (no job registered)", manager.Len())
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("marker stat = %v, want it not to exist (no spawn)", err)
	}
	if runner.callCount() != 1 {
		t.Fatalf("runner calls = %d, want 1", runner.callCount())
	}
}

// TestPolicyCheckedSpecSkipsRunner 证明生产者已校验的 ProcessSpec
// （PolicyChecked=true）不会被管理器二次包装：即使 runner 必然失败，任务仍然
// 正常启动。
func TestPolicyCheckedSpecSkipsRunner(t *testing.T) {
	root := t.TempDir()
	policy := newRunnerPolicy(t, root, "echo")
	runner := &fakeRunner{rewrite: func([]string) ([]string, error) {
		return nil, sandbox.ErrSandboxUnavailable
	}}
	manager := jobs.NewManager(jobs.WithPolicy(policy), jobs.WithRunner(runner))
	t.Cleanup(func() { _ = manager.CloseAll() })

	job, err := manager.StartProcess(background.ProcessSpec{
		Kind:          string(background.KindBash),
		Label:         "echo checked",
		Command:       "echo checked",
		Argv:          []string{"/bin/sh", "-c", "echo checked"},
		Dir:           root,
		PolicyChecked: true,
	})
	if err != nil {
		t.Fatalf("StartProcess(PolicyChecked) = %v, want the runner to be skipped", err)
	}
	view, settled, waitErr := job.Wait(context.Background(), 5*time.Second)
	if waitErr != nil || !settled {
		t.Fatalf("job.Wait = settled=%v err=%v", settled, waitErr)
	}
	if view.ExitCode != 0 {
		t.Fatalf("job exit code = %d, want 0", view.ExitCode)
	}
	output, _ := job.Read()
	if !strings.Contains(output, "checked") {
		t.Fatalf("job output = %q, want checked", output)
	}
	if runner.callCount() != 0 {
		t.Fatalf("runner calls = %d, want 0 for a producer-checked spec", runner.callCount())
	}
}

// TestNilRunnerKeepsJobStartBehavior 证明未挂接 runner 时 job_start 的行为
// 不变（仍然直接执行 /bin/sh -c）。
func TestNilRunnerKeepsJobStartBehavior(t *testing.T) {
	root := t.TempDir()
	policy := newRunnerPolicy(t, root, "echo")
	_, tools := newJobTools(t, jobs.WithPolicy(policy))
	jobID := startJob(t, tools, fmt.Sprintf(`{"command":"echo unchanged","cwd":%q}`, root))
	output := invokeJob(t, tools, jobs.ToolJobOutput,
		`{"job_id":"`+jobID+`","wait":true,"timeout_ms":5000}`)
	if output.IsError {
		t.Fatalf("job_output = %q", output.Content)
	}
	if !strings.Contains(output.Content, "unchanged") {
		t.Fatalf("job output = %q, want unchanged", output.Content)
	}
}

// TestRunnerWithoutPolicyFailsClosed 证明挂接 runner 但管理器没有沙箱策略时
// fail closed：任务启动失败、不调用 Confine、也不 spawn。
func TestRunnerWithoutPolicyFailsClosed(t *testing.T) {
	runner := &fakeRunner{}
	manager := jobs.NewManager(jobs.WithRunner(runner))
	t.Cleanup(func() { _ = manager.CloseAll() })

	_, err := manager.StartProcess(background.ProcessSpec{
		Kind:    string(background.KindBash),
		Command: "echo should-not-run",
		Argv:    []string{"/bin/sh", "-c", "echo should-not-run"},
	})
	if err == nil {
		t.Fatal("StartProcess succeeded without a policy")
	}
	if !strings.Contains(err.Error(), "requires a sandbox policy") {
		t.Fatalf("error = %v, want it to mention the missing policy", err)
	}
	if runner.callCount() != 0 {
		t.Fatalf("runner calls = %d, want 0 without a policy", runner.callCount())
	}
	if manager.Len() != 0 {
		t.Fatalf("manager.Len = %d, want 0", manager.Len())
	}
}
