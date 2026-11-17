package sandbox_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

func TestWorkspaceMetadataPathsIncludesGitDirAndCredentials(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	paths := sandbox.WorkspaceMetadataPaths(root)
	wantGit, _ := filepath.EvalSymlinks(filepath.Join(root, ".git"))
	found := map[string]bool{}
	for _, path := range paths {
		found[filepath.Base(path)] = true
		if path == wantGit {
			found[".git-resolved"] = true
		}
	}
	for _, name := range []string{".git", ".aws", ".codex"} {
		if !found[name] {
			t.Fatalf("metadata paths = %v, want %s", paths, name)
		}
	}
}

func TestWorkspaceMetadataPathsResolvesGitDirPointer(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "worktree")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	realGitDir := filepath.Join(base, "real-git-dir")
	if err := os.Mkdir(realGitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// .git 是文件（git worktree / submodule 形态），指向真实 gitdir。
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: ../real-git-dir\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	paths := sandbox.WorkspaceMetadataPaths(root)
	sawPointer, sawTarget := false, false
	want, _ := filepath.EvalSymlinks(realGitDir)
	for _, path := range paths {
		if strings.HasSuffix(path, string(filepath.Separator)+".git") {
			sawPointer = true
		}
		if path == want {
			sawTarget = true
		}
	}
	if !sawPointer || !sawTarget {
		t.Fatalf("paths = %v, want both the pointer file and the resolved gitdir target", paths)
	}
}

func TestAddProtectedPathsAppendsAndDeduplicates(t *testing.T) {
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	if err := os.Mkdir(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	policy := sandbox.New(sandbox.WithRoots(root))
	policy.AddProtectedPaths(sandbox.WorkspaceMetadataPaths(root)...)
	policy.AddProtectedPaths(sandbox.WorkspaceMetadataPaths(root)...)

	protected := policy.ProtectedPaths()
	seen := map[string]int{}
	for _, path := range protected {
		seen[path]++
	}
	if len(seen) != len(protected) {
		t.Fatalf("protected = %v, want no duplicates", protected)
	}
	resolved, _ := filepath.EvalSymlinks(gitDir)
	if seen[resolved] != 1 {
		t.Fatalf("protected = %v, want .git protected exactly once", protected)
	}
}

func TestWorkspaceMetadataPathsToleratesMissingRoot(t *testing.T) {
	if paths := sandbox.WorkspaceMetadataPaths(""); len(paths) != 0 {
		t.Fatalf("paths = %v, want empty for an empty root", paths)
	}
}
