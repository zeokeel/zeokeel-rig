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

// TestAddProtectedPathsIsRecordOnlyNoOp 钉住 DSH 对齐后的新语义：AddProtectedPaths
// 不再让任何路径"受保护"——ProtectedPaths 恒为空，写入判定也不看它。
//
// 这是**有意**的变更：DSH 的沙箱只有可写根围栏，没有保护路径这一层。导出 API 保留
// （编译兼容），但语义退化为记录型 no-op。
func TestAddProtectedPathsIsRecordOnlyNoOp(t *testing.T) {
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	if err := os.Mkdir(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	policy := sandbox.New(sandbox.WithRoots(root))
	policy.AddProtectedPaths(sandbox.WorkspaceMetadataPaths(root)...)
	policy.AddProtectedPaths(sandbox.WorkspaceMetadataPaths(root)...)

	if protected := policy.ProtectedPaths(); len(protected) != 0 {
		t.Fatalf("ProtectedPaths() = %v, want empty after DSH parity", protected)
	}
	if deny := policy.WriteDenyPaths(); len(deny) != 0 {
		t.Fatalf("WriteDenyPaths() = %v, want empty after DSH parity", deny)
	}
	if err := policy.CheckWritablePath(filepath.Join(gitDir, "config")); err != nil {
		t.Fatalf("CheckWritablePath(.git/config) = %v, want nil（保护路径已停用）", err)
	}
}

func TestWorkspaceMetadataPathsToleratesMissingRoot(t *testing.T) {
	if paths := sandbox.WorkspaceMetadataPaths(""); len(paths) != 0 {
		t.Fatalf("paths = %v, want empty for an empty root", paths)
	}
}
