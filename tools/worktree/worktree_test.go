package worktree_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/worktree"
)

// requireGit skips the test when git is not installed.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git is not installed: %v", err)
	}
}

// runGit runs git in dir and fails the test on a non-zero exit.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, output)
	}
	return strings.TrimSpace(string(output))
}

// gitOK reports whether a git command exits zero.
func gitOK(t *testing.T, dir string, args ...string) bool {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	return command.Run() == nil
}

// newRepo creates a temporary git repository with one commit.
func newRepo(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	runGit(t, directory, "init", "-q")
	runGit(t, directory, "config", "user.email", "rig-tests@example.com")
	runGit(t, directory, "config", "user.name", "Rig Tests")
	if err := os.WriteFile(filepath.Join(directory, "README.md"), []byte("init\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit(t, directory, "add", "README.md")
	runGit(t, directory, "commit", "-q", "-m", "init")
	return directory
}

// repoTop returns the repository toplevel as git reports it.
func repoTop(t *testing.T, repository string) string {
	t.Helper()
	return runGit(t, repository, "rev-parse", "--show-toplevel")
}

// toolsFor builds the worktree tools bound to the policy root.
func toolsFor(t *testing.T, root string) map[string]rig.Tool {
	t.Helper()
	policy := sandbox.New(sandbox.WithRoots(root))
	tools := worktree.Tools(policy)
	byName := make(map[string]rig.Tool, len(tools))
	for _, tool := range tools {
		byName[tool.Name()] = tool
	}
	return byName
}

// invoke runs one worktree tool call and fails on a Go error.
func invoke(t *testing.T, tool rig.Tool, arguments string) rig.ToolResult {
	t.Helper()
	result, err := tool.Invoke(context.Background(), rig.ToolCall{Name: tool.Name(), Arguments: arguments})
	if err != nil {
		t.Fatalf("%s.Invoke: %v", tool.Name(), err)
	}
	return result
}

func requireContains(t *testing.T, text string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Fatalf("text %q does not contain %q", text, want)
		}
	}
}

func requireError(t *testing.T, result rig.ToolResult, wants ...string) {
	t.Helper()
	if !result.IsError {
		t.Fatalf("expected a tool error, got %q", result.Content)
	}
	requireContains(t, result.Content, wants...)
}

// TestEnterListExit covers the full worktree lifecycle.
func TestEnterListExit(t *testing.T) {
	requireGit(t)
	repository := newRepo(t)
	top := repoTop(t, repository)
	tools := toolsFor(t, repository)
	enter := tools[worktree.ToolEnter]
	list := tools[worktree.ToolList]
	exit := tools[worktree.ToolExit]

	entered := invoke(t, enter, `{"branch":"feature-x"}`)
	requireContains(t, entered.Content, "created worktree", "feature-x")
	wantPath := filepath.Join(top, ".rig", "worktrees", "feature-x")
	requireContains(t, entered.Content, wantPath)
	if info, err := os.Stat(wantPath); err != nil || !info.IsDir() {
		t.Fatalf("worktree directory %q not created: %v", wantPath, err)
	}
	if !gitOK(t, repository, "show-ref", "--verify", "refs/heads/feature-x") {
		t.Fatal("branch feature-x does not exist")
	}

	listed := invoke(t, list, `{}`)
	requireContains(t, listed.Content, "PATH", "BRANCH", "HEAD", "STATE", wantPath, "feature-x", "main")

	if err := os.WriteFile(filepath.Join(wantPath, "note.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("write worktree file: %v", err)
	}
	exited := invoke(t, exit, `{"path":"`+wantPath+`"}`)
	requireContains(t, exited.Content, "removed worktree", wantPath, "feature-x kept")
	if _, err := os.Stat(wantPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("worktree directory still exists after exit: %v", err)
	}
	if !gitOK(t, repository, "show-ref", "--verify", "refs/heads/feature-x") {
		t.Fatal("branch was deleted although keep_branch defaults to true")
	}

	// A second worktree with keep_branch=false deletes the branch.
	invoke(t, enter, `{"branch":"feature-y"}`)
	wantPathY := filepath.Join(top, ".rig", "worktrees", "feature-y")
	exited = invoke(t, exit, `{"path":"`+wantPathY+`","keep_branch":false}`)
	requireContains(t, exited.Content, "branch feature-y deleted")
	if gitOK(t, repository, "show-ref", "--verify", "refs/heads/feature-y") {
		t.Fatal("branch feature-y still exists after keep_branch=false")
	}
}

// TestEnterPathOverride verifies an explicit path inside the sandbox.
func TestEnterPathOverride(t *testing.T) {
	requireGit(t)
	repository := newRepo(t)
	top := repoTop(t, repository)
	tools := toolsFor(t, repository)
	entered := invoke(t, tools[worktree.ToolEnter], `{"path":"custom/wt-one","branch":"custom-branch"}`)
	wantPath := filepath.Join(top, "custom", "wt-one")
	requireContains(t, entered.Content, wantPath, "custom-branch")
	if info, err := os.Stat(wantPath); err != nil || !info.IsDir() {
		t.Fatalf("custom worktree %q not created: %v", wantPath, err)
	}
	invoke(t, tools[worktree.ToolExit], `{"path":"custom/wt-one"}`)
	if _, err := os.Stat(wantPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("custom worktree still exists: %v", err)
	}
}

// TestPathConfinement verifies paths outside the sandbox are refused.
func TestPathConfinement(t *testing.T) {
	requireGit(t)
	repository := newRepo(t)
	tools := toolsFor(t, repository)
	requireError(t, invoke(t, tools[worktree.ToolEnter], `{"path":"../escape","branch":"escape"}`), "path")
	requireError(t, invoke(t, tools[worktree.ToolExit], `{"path":"../escape"}`), "path")
	if _, err := os.Stat(filepath.Join(filepath.Dir(repository), "escape")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a worktree escaped the sandbox: %v", err)
	}
}

// TestNonGitWorkspace verifies a readable error outside a repository.
func TestNonGitWorkspace(t *testing.T) {
	requireGit(t)
	directory := t.TempDir()
	tools := toolsFor(t, directory)
	for _, name := range []string{worktree.ToolEnter, worktree.ToolList} {
		result := invoke(t, tools[name], `{"branch":"feature"}`)
		requireError(t, result, "not")
		requireContains(t, result.Content, "git repository")
	}
}

// TestReadOnlyPolicy verifies mutations are refused but listing works.
func TestReadOnlyPolicy(t *testing.T) {
	requireGit(t)
	repository := newRepo(t)
	top := repoTop(t, repository)
	writable := toolsFor(t, repository)
	invoke(t, writable[worktree.ToolEnter], `{"branch":"feature-z"}`)
	worktreePath := filepath.Join(top, ".rig", "worktrees", "feature-z")

	policy := sandbox.New(sandbox.WithRoots(repository), sandbox.WithReadOnly(true))
	readOnlyTools := worktree.Tools(policy)
	byName := make(map[string]rig.Tool, len(readOnlyTools))
	for _, tool := range readOnlyTools {
		byName[tool.Name()] = tool
	}
	requireError(t, invoke(t, byName[worktree.ToolEnter], `{"branch":"new-branch"}`), "read-only")
	requireError(t, invoke(t, byName[worktree.ToolExit], `{"path":"`+worktreePath+`"}`), "read-only")
	listed := invoke(t, byName[worktree.ToolList], `{}`)
	if listed.IsError {
		t.Fatalf("list_worktrees failed on a read-only policy: %s", listed.Content)
	}
	requireContains(t, listed.Content, worktreePath, "feature-z")
}

// TestExitRejectsMainAndUnknownWorktree verifies exit safety checks.
func TestExitRejectsMainAndUnknownWorktree(t *testing.T) {
	requireGit(t)
	repository := newRepo(t)
	tools := toolsFor(t, repository)
	requireError(t, invoke(t, tools[worktree.ToolExit], `{"path":"`+repository+`"}`), "main")
	requireError(t, invoke(t, tools[worktree.ToolExit], `{"path":"missing-worktree"}`),
		"not a registered worktree")
}

// TestRepositoryRootMustBeSandboxed verifies the repository root check when the
// policy points at a subdirectory of the repository.
func TestRepositoryRootMustBeSandboxed(t *testing.T) {
	requireGit(t)
	repository := newRepo(t)
	subdirectory := filepath.Join(repository, "sub")
	if err := os.MkdirAll(subdirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	tools := toolsFor(t, subdirectory)
	requireError(t, invoke(t, tools[worktree.ToolEnter], `{"branch":"feature-sub"}`),
		"outside the sandbox roots")
}

// TestBranchValidation verifies unsafe branch names are refused.
func TestBranchValidation(t *testing.T) {
	requireGit(t)
	repository := newRepo(t)
	tools := toolsFor(t, repository)
	for _, branch := range []string{"--force", "a/b", "has space", ".."} {
		requireError(t, invoke(t, tools[worktree.ToolEnter],
			`{"branch":"`+branch+`"}`), "invalid branch")
	}
}

// TestToolMetadata verifies names, descriptions and schemas.
func TestToolMetadata(t *testing.T) {
	tools := worktree.Tools(sandbox.New())
	wantNames := []string{worktree.ToolEnter, worktree.ToolExit, worktree.ToolList}
	if len(tools) != len(wantNames) {
		t.Fatalf("Tools returned %d tools, want %d", len(tools), len(wantNames))
	}
	byName := make(map[string]rig.Tool, len(tools))
	for _, tool := range tools {
		byName[tool.Name()] = tool
	}
	for _, name := range wantNames {
		tool, ok := byName[name]
		if !ok {
			t.Fatalf("missing tool %q", name)
		}
		if strings.TrimSpace(tool.Description()) == "" {
			t.Fatalf("%s has an empty description", name)
		}
		if tool.Schema()["type"] != "object" {
			t.Fatalf("%s schema = %v", name, tool.Schema())
		}
	}
	required, ok := byName[worktree.ToolExit].Schema()["required"].([]string)
	if !ok || len(required) != 1 || required[0] != "path" {
		t.Fatalf("exit schema required = %v", byName[worktree.ToolExit].Schema()["required"])
	}
}
