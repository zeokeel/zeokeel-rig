package worktree

// Internal tests for the porcelain parser and helper functions (no git
// executable required).

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestParseWorktreePorcelain covers every field emitted by
// "git worktree list --porcelain".
func TestParseWorktreePorcelain(t *testing.T) {
	raw := `worktree /repo
HEAD 1111111111111111111111111111111111111111
branch refs/heads/main

worktree /repo/.rig/worktrees/detached
HEAD 2222222222222222222222222222222222222222
detached

worktree /repo/bare
bare

worktree /repo/locked
HEAD 3333333333333333333333333333333333333333
branch refs/heads/locked
locked maintenance window

worktree /repo/prunable
HEAD 4444444444444444444444444444444444444444
branch refs/heads/prune
prunable gitdir not found
`
	worktrees := parseWorktreePorcelain(raw)
	if len(worktrees) != 5 {
		t.Fatalf("parsed %d worktrees, want 5: %+v", len(worktrees), worktrees)
	}
	main := worktrees[0]
	if main.path != "/repo" || main.branch != "main" || main.branchRef != "refs/heads/main" {
		t.Fatalf("main entry = %+v", main)
	}
	if main.head != "1111111111111111111111111111111111111111" {
		t.Fatalf("main head = %q", main.head)
	}
	if !worktrees[1].detached || worktrees[1].branch != "" {
		t.Fatalf("detached entry = %+v", worktrees[1])
	}
	if !worktrees[2].bare {
		t.Fatalf("bare entry = %+v", worktrees[2])
	}
	if !worktrees[3].locked || worktrees[3].lockReason != "maintenance window" {
		t.Fatalf("locked entry = %+v", worktrees[3])
	}
	if !worktrees[4].prunable || worktrees[4].prunableReason != "gitdir not found" {
		t.Fatalf("prunable entry = %+v", worktrees[4])
	}
}

// TestWorktreeState verifies the table STATE rendering.
func TestWorktreeState(t *testing.T) {
	repoTop := filepath.Clean("/repo")
	if got := worktreeState(worktree{path: "/repo"}, repoTop); got != "main" {
		t.Fatalf("main state = %q", got)
	}
	if got := worktreeState(worktree{path: "/repo/.rig/worktrees/x"}, repoTop); got != "managed" {
		t.Fatalf("managed state = %q", got)
	}
	if got := worktreeState(worktree{path: "/repo/.rig/worktrees/x", detached: true}, repoTop); got != "managed,detached" {
		t.Fatalf("detached managed state = %q", got)
	}
	if got := worktreeState(worktree{path: "/elsewhere", locked: true, prunable: true}, repoTop); got != "locked,prunable" {
		t.Fatalf("flagged state = %q", got)
	}
	if got := worktreeState(worktree{path: "/elsewhere"}, repoTop); got != "linked" {
		t.Fatalf("plain state = %q", got)
	}
}

// TestPorcelainHelpers covers display and comparison helpers.
func TestPorcelainHelpers(t *testing.T) {
	if got := displayBranch(""); got != "(detached)" {
		t.Fatalf("displayBranch(\"\") = %q", got)
	}
	if got := shortCommit("1234567890abcdef"); got != "1234567890ab" {
		t.Fatalf("shortCommit = %q", got)
	}
	if got := shortCommit(""); got != "-" {
		t.Fatalf("shortCommit(\"\") = %q", got)
	}
	worktrees := []worktree{{path: "/repo/wt", branch: "x"}}
	if entry, ok := findWorktree(worktrees, "/repo/wt"); !ok || entry.branch != "x" {
		t.Fatalf("findWorktree = %+v/%v", entry, ok)
	}
	if _, ok := findWorktree(worktrees, "/repo/missing"); ok {
		t.Fatal("findWorktree matched a missing path")
	}
	if !samePath("/repo/wt", "/repo/wt/") {
		t.Fatal("samePath should ignore trailing separators")
	}
	if samePath("/repo/wt", "/repo/other") {
		t.Fatal("samePath matched different paths")
	}
}

// TestValidateBranch covers branch name validation.
func TestValidateBranch(t *testing.T) {
	for _, valid := range []string{"main", "feature-x", "a.b_c-1"} {
		if err := validateBranch(valid); err != nil {
			t.Fatalf("validateBranch(%q) = %v", valid, err)
		}
	}
	for _, invalid := range []string{"", "-x", "a/b", "has space", "..", strings.Repeat("a", 65), "a b"} {
		if err := validateBranch(invalid); err == nil {
			t.Fatalf("validateBranch(%q) accepted an invalid name", invalid)
		}
	}
}
