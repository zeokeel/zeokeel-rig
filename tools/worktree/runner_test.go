package worktree_test

// Tests for wiring an OS-level sandbox.Runner into the worktree tools: git is
// spawned through the confined argv, and a Confine error fails the command
// closed without side effects.

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/worktree"
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

// toolsWithRunner 构建带 runner 的 worktree 工具映射。
func toolsWithRunner(t *testing.T, root string, runner sandbox.Runner) map[string]rig.Tool {
	t.Helper()
	policy := sandbox.New(sandbox.WithRoots(root))
	tools := worktree.Tools(policy, worktree.WithRunner(runner))
	byName := make(map[string]rig.Tool, len(tools))
	for _, tool := range tools {
		byName[tool.Name()] = tool
	}
	return byName
}

// TestWorktreeConfinesGitArgv 用记录调用并 exec 真实 git 的包装脚本证明：
// worktree 的 git 子进程 spawn 的是 runner 返回的 argv，而不是裸 "git"。
func TestWorktreeConfinesGitArgv(t *testing.T) {
	requireGit(t)
	repository := newRepo(t)

	helperDir := t.TempDir()
	logPath := filepath.Join(helperDir, "git-calls.log")
	wrapper := filepath.Join(helperDir, "git-wrapper")
	script := "#!/bin/sh\necho \"$@\" >> \"$RIG_GIT_WRAP_LOG\"\nexec git \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatalf("write git wrapper: %v", err)
	}
	t.Setenv("RIG_GIT_WRAP_LOG", logPath)

	runner := &fakeRunner{rewrite: func(argv []string) ([]string, error) {
		if len(argv) == 0 || filepath.Base(argv[0]) != "git" {
			return argv, nil
		}
		return append([]string{wrapper}, argv[1:]...), nil
	}}
	tools := toolsWithRunner(t, repository, runner)

	entered := invoke(t, tools[worktree.ToolEnter], `{"branch":"runner-branch"}`)
	if entered.IsError {
		t.Fatalf("enter_worktree = %q, want success", entered.Content)
	}
	requireContains(t, entered.Content, "created worktree", "runner-branch")
	if !gitOK(t, repository, "show-ref", "--verify", "refs/heads/runner-branch") {
		t.Fatal("runner-branch does not exist")
	}
	if runner.callCount() == 0 {
		t.Fatal("runner was never called")
	}
	if got := runner.callAt(t, 0).argv; len(got) == 0 || got[0] != "git" {
		t.Fatalf("runner argv = %q, want it to start with git", got)
	}
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read git wrapper log: %v", err)
	}
	requireContains(t, string(logged), "rev-parse", "--show-toplevel", "worktree", "add")
}

// TestWorktreeRunnerFailureFailsClosed 证明 Confine 失败时命令直接失败，且
// 不产生 worktree。
func TestWorktreeRunnerFailureFailsClosed(t *testing.T) {
	requireGit(t)
	repository := newRepo(t)
	runner := &fakeRunner{rewrite: func([]string) ([]string, error) {
		return nil, errors.New("confine: " + sandbox.ErrSandboxUnavailable.Error())
	}}
	tools := toolsWithRunner(t, repository, runner)

	result := invoke(t, tools[worktree.ToolEnter], `{}`)
	if !result.IsError {
		t.Fatalf("enter_worktree = %q, want a fail-closed tool error", result.Content)
	}
	requireContains(t, result.Content, sandbox.ErrSandboxUnavailable.Error())
	if runner.callCount() == 0 {
		t.Fatal("runner was never called")
	}
	entries, err := os.ReadDir(filepath.Join(repository, ".rig", "worktrees"))
	if err == nil && len(entries) != 0 {
		t.Fatalf("worktrees = %v, want none when the runner fails", entries)
	}
}

// TestWorktreeRunnerNilKeepsBehavior 证明未挂接 runner 时行为不变。
func TestWorktreeRunnerNilKeepsBehavior(t *testing.T) {
	requireGit(t)
	repository := newRepo(t)
	tools := toolsFor(t, repository)
	entered := invoke(t, tools[worktree.ToolEnter], `{"branch":"no-runner"}`)
	if entered.IsError {
		t.Fatalf("enter_worktree without a runner = %q", entered.Content)
	}
	requireContains(t, entered.Content, "created worktree", "no-runner")
}
