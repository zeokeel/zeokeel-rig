package worktree_test

// worktree 侧的沙箱归因（docs/SANDBOX-OS.md §5 记录的缺口）：git 把 OS 沙箱
// 拒绝报成普通的 "Permission denied" 时，失败的 git 错误文本末尾追加结构化
// 说明（被拒目标、命中方言、可写面与处置顺序），让模型能把它与「仓库自身出错」
// 区分开。未挂 runner 或输出不命中方言时结果逐字节不变。

import (
	"errors"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/worktree"
)

// denialGitRunner 返回一个 fake runner：把任何 git 调用改写为「向 stderr 打印
// emitted 并以 128 退出」的 /bin/sh 命令，并报告给定拒绝方言。
func denialGitRunner(signatures []string, emitted string, enforcement sandbox.Enforcement) *fakeRunner {
	return &fakeRunner{
		signatures:  signatures,
		enforcement: enforcement,
		rewrite: func(argv []string) ([]string, error) {
			return []string{"/bin/sh", "-c", `echo "` + emitted + `" >&2; exit 128`}, nil
		},
	}
}

// TestWorktreeAttributesSandboxDenial 证明失败的 git 结果携带结构化归因说明。
func TestWorktreeAttributesSandboxDenial(t *testing.T) {
	requireGit(t)
	repository := newRepo(t)
	runner := denialGitRunner([]string{"read-only file system"}, "Read-only file system", sandbox.EnforcementPartial)
	tools := toolsWithRunner(t, repository, runner)

	result := invoke(t, tools[worktree.ToolEnter], `{"branch":"denied-branch"}`)
	requireError(t, result,
		"Read-only file system",
		"[sandbox: denied ",
		`matched "read-only file system"`,
		"mode workspace-write",
		"enforcement partial",
		"writable root",
	)
}

// TestWorktreeDenialControls 是归因的对照组：不匹配方言的 git 失败、没有 runner
// 的普通 git 失败与 Confine 失败都不得出现归因说明。
func TestWorktreeDenialControls(t *testing.T) {
	requireGit(t)
	t.Run("non-matching-output", func(t *testing.T) {
		repository := newRepo(t)
		runner := denialGitRunner([]string{"read-only file system"}, "No such file or directory", sandbox.EnforcementFull)
		tools := toolsWithRunner(t, repository, runner)
		result := invoke(t, tools[worktree.ToolEnter], `{"branch":"clean-branch"}`)
		requireError(t, result, "No such file or directory")
		if hasAttribution(result.Content) {
			t.Fatalf("non-matching git failure must not carry attribution:\n%s", result.Content)
		}
	})
	t.Run("no-runner", func(t *testing.T) {
		repository := newRepo(t)
		tools := toolsFor(t, repository)
		result := invoke(t, tools[worktree.ToolEnter], `{"branch":"plain-branch"}`)
		if result.IsError {
			t.Fatalf("enter_worktree = %q, want success without a runner", result.Content)
		}
		if hasAttribution(result.Content) {
			t.Fatalf("no-runner result must not carry attribution:\n%s", result.Content)
		}
	})
	t.Run("confine-failure", func(t *testing.T) {
		repository := newRepo(t)
		runner := &fakeRunner{
			signatures: []string{"read-only file system"},
			rewrite: func([]string) ([]string, error) {
				return nil, errors.New("confine: " + sandbox.ErrSandboxUnavailable.Error())
			},
		}
		tools := toolsWithRunner(t, repository, runner)
		result := invoke(t, tools[worktree.ToolEnter], `{}`)
		requireError(t, result, sandbox.ErrSandboxUnavailable.Error())
		if hasAttribution(result.Content) {
			t.Fatalf("a Confine error is not a denial attribution:\n%s", result.Content)
		}
	})
}

// hasAttribution 报告文本是否携带结构化归因说明。
func hasAttribution(text string) bool {
	return strings.Contains(text, "[sandbox: denied ")
}
