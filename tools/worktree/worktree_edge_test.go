package worktree_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/worktree"
)

// TestEnterGeneratedBranchAndDefaultPath covers the no-argument default: a
// generated branch under .rig/worktrees.
func TestEnterGeneratedBranchAndDefaultPath(t *testing.T) {
	requireGit(t)
	repository := newRepo(t)
	top := repoTop(t, repository)
	tools := toolsFor(t, repository)

	entered := invoke(t, tools[worktree.ToolEnter], `{}`)
	if entered.IsError {
		t.Fatalf("enter_worktree with defaults failed: %s", entered.Content)
	}
	requireContains(t, entered.Content, "created worktree", ".rig/worktrees/", "rig-wt-")
	matches, err := filepath.Glob(filepath.Join(top, ".rig", "worktrees", "rig-wt-*"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("default worktrees = %v/%v, want exactly one", matches, err)
	}
	listed := invoke(t, tools[worktree.ToolList], `{}`)
	requireContains(t, listed.Content, matches[0], "managed", "rig-wt-")
	invoke(t, tools[worktree.ToolExit], `{"path":"`+matches[0]+`"}`)
}

// TestRepositoryWithoutCommit verifies the readable error for an empty
// repository.
func TestRepositoryWithoutCommit(t *testing.T) {
	requireGit(t)
	directory := t.TempDir()
	runGit(t, directory, "init", "-q")
	tools := toolsFor(t, directory)
	requireError(t, invoke(t, tools[worktree.ToolEnter], `{}`), "no usable HEAD commit")
}

// TestEnterPathDerivesBranch verifies branch derivation from an explicit path.
func TestEnterPathDerivesBranch(t *testing.T) {
	requireGit(t)
	repository := newRepo(t)
	top := repoTop(t, repository)
	tools := toolsFor(t, repository)
	entered := invoke(t, tools[worktree.ToolEnter], `{"path":"derived/wt-one"}`)
	requireContains(t, entered.Content, "branch wt-one")
	if !gitOK(t, repository, "show-ref", "--verify", "refs/heads/wt-one") {
		t.Fatal("derived branch wt-one does not exist")
	}
	requireError(t, invoke(t, tools[worktree.ToolEnter], `{"path":"derived/wt-one"}`), "already exists")
	invoke(t, tools[worktree.ToolExit], `{"path":"`+filepath.Join(top, "derived", "wt-one")+`","keep_branch":false}`)
	if gitOK(t, repository, "show-ref", "--verify", "refs/heads/wt-one") {
		t.Fatal("derived branch survived keep_branch=false")
	}
}

// TestInvalidArgumentsJSON verifies protocol errors stay Go errors.
func TestInvalidArgumentsJSON(t *testing.T) {
	tools := worktree.Tools(nil)
	for _, tool := range tools {
		if _, err := tool.Invoke(context.Background(), rig.ToolCall{Arguments: `{not json`}); err == nil {
			t.Fatalf("%s returned no Go error for invalid JSON", tool.Name())
		}
	}
}

// TestNilPolicyFailsClosed verifies the fail-closed default policy.
func TestNilPolicyFailsClosed(t *testing.T) {
	tools := worktree.Tools(nil)
	byName := make(map[string]rig.Tool, len(tools))
	for _, tool := range tools {
		byName[tool.Name()] = tool
	}
	for _, name := range []string{worktree.ToolEnter, worktree.ToolList} {
		result := invoke(t, byName[name], `{}`)
		if !result.IsError {
			t.Fatalf("%s succeeded with a nil policy: %q", name, result.Content)
		}
		requireContains(t, result.Content, "no roots")
	}
	// exit_worktree validates its required path before touching the repository
	// so it stays a model-facing error even with the fail-closed policy.
	exitResult := invoke(t, byName[worktree.ToolExit], `{"path":"somewhere"}`)
	if !exitResult.IsError {
		t.Fatalf("exit_worktree succeeded with a nil policy: %q", exitResult.Content)
	}
}

// TestDirectoryStateAndMissingParent verifies the worktree parent directory is
// created inside the sandbox for nested explicit paths.
func TestNestedExplicitPath(t *testing.T) {
	requireGit(t)
	repository := newRepo(t)
	top := repoTop(t, repository)
	tools := toolsFor(t, repository)
	nested := filepath.Join("deep", "nested", "checkout")
	entered := invoke(t, tools[worktree.ToolEnter], `{"path":"`+filepath.ToSlash(nested)+`","branch":"nested-branch"}`)
	requireContains(t, entered.Content, filepath.Join(top, "deep", "nested", "checkout"), "nested-branch")
	if info, err := os.Stat(filepath.Join(top, "deep", "nested", "checkout")); err != nil || !info.IsDir() {
		t.Fatalf("nested worktree not created: %v", err)
	}
	invoke(t, tools[worktree.ToolExit], `{"path":"`+filepath.ToSlash(nested)+`"}`)
}

// TestListEmptyRepositoryMainOnly verifies list works with only the main
// checkout.
func TestListEmptyRepositoryMainOnly(t *testing.T) {
	requireGit(t)
	repository := newRepo(t)
	top := repoTop(t, repository)
	listed := invoke(t, toolsFor(t, repository)[worktree.ToolList], `{}`)
	requireContains(t, listed.Content, top, "main")
	if strings.Count(listed.Content, "\n") < 1 {
		t.Fatalf("list output has no table rows: %q", listed.Content)
	}
}
