package sandbox_test

// 可写面模型（WritableRoot）与"单一判定"（AllowsWrite）的行为契约。
//
// 这些用例锁的是三条不变量：
//  1. 可写面只有一份模型（根 + 根内只读子路径），策略、四个 OS 后端与进程内围栏
//     都从它派生；
//  2. 只读子路径在 AllowsWrite 里必须挡住写入（对根内的任何深度）；
//  3. 工具链缓存默认开启，但**目录里的凭据文件**永远只读。

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// newWorkspace 建一个临时工作区并返回它的真实路径。
func newWorkspace(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("eval tmpdir: %v", err)
	}
	return root
}

// writeFileAt 在测试里直接建一个文件（不经过策略）。
func writeFileAt(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestWritableRootsIncludesPolicyRootsWithSubpaths(t *testing.T) {
	root := newWorkspace(t)
	gitConfig := filepath.Join(root, ".git", "config")
	writeFileAt(t, gitConfig, "[core]\n")

	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithReadOnlySubpaths(root, filepath.Join(".git", "config")),
	)
	roots := policy.WritableRoots()
	if len(roots) != 1 {
		t.Fatalf("WritableRoots() = %d entries, want 1: %+v", len(roots), roots)
	}
	if !sandbox.PathEqual(roots[0].Root, root) {
		t.Fatalf("Root = %q, want %q", roots[0].Root, root)
	}
	if len(roots[0].ReadOnlySubpaths) != 1 {
		t.Fatalf("ReadOnlySubpaths = %v, want the .git/config entry", roots[0].ReadOnlySubpaths)
	}
	if !sandbox.PathEqual(roots[0].ReadOnlySubpaths[0], gitConfig) {
		t.Fatalf("ReadOnlySubpaths[0] = %q, want the absolute %q", roots[0].ReadOnlySubpaths[0], gitConfig)
	}
}

func TestWithWritableRootAcceptsRelativeSubpaths(t *testing.T) {
	root := newWorkspace(t)
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithWritableRoot(root, ".git/hooks", ".git/config"),
	)
	roots := policy.WritableRoots()
	if len(roots) != 1 || len(roots[0].ReadOnlySubpaths) != 2 {
		t.Fatalf("WritableRoots() = %+v, want one root with two read-only subpaths", roots)
	}
	for _, sub := range roots[0].ReadOnlySubpaths {
		if !filepath.IsAbs(sub) {
			t.Fatalf("subpath %q is not absolute after normalization", sub)
		}
	}
}

// 旧的 WithWritableRoots([]string) 必须继续工作（转成 {Root}）。
func TestWithWritableRootsStaysBackwardCompatible(t *testing.T) {
	root := newWorkspace(t)
	extra := newWorkspace(t)
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithWritableRoots(extra),
	)
	roots := policy.WritableRoots()
	if len(roots) != 2 {
		t.Fatalf("WritableRoots() = %+v, want the workspace root and the extra root", roots)
	}
	if len(roots[1].ReadOnlySubpaths) != 0 {
		t.Fatalf("legacy entry gained subpaths: %+v", roots[1])
	}
	if !sandbox.PathEqual(roots[1].Root, extra) {
		t.Fatalf("extra root = %q, want %q", roots[1].Root, extra)
	}
}

// AllowsWrite 是唯一的可写面判定：根内可写、只读子路径（任意深度）不可写、
// 根外不可写。
func TestAllowsWriteHonoursReadOnlySubpaths(t *testing.T) {
	root := newWorkspace(t)
	writeFileAt(t, filepath.Join(root, ".git", "config"), "[core]\n")
	writeFileAt(t, filepath.Join(root, ".git", "objects", "ab", "cdef"), "blob")
	writeFileAt(t, filepath.Join(root, "src", "main.go"), "package main\n")

	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithReadOnlySubpaths(root, ".git/config"),
	)

	cases := []struct {
		path string
		want bool
		note string
	}{
		{filepath.Join(root, "src", "main.go"), true, "普通文件可写"},
		{filepath.Join(root, "new", "file.txt"), true, "根内新文件可写"},
		{filepath.Join(root, ".git", "objects", "ab", "cdef"), true, "git 对象可写"},
		{filepath.Join(root, ".git", "config"), false, "只读子路径本身"},
		{filepath.Join(root, ".git", "config.lock"), true, "只读子路径的兄弟可写"},
		{filepath.Join(root, "..", "outside.txt"), false, "根外不可写"},
		{filepath.Join(root, ".git"), true, "只含只读子路径的父目录本身可写"},
	}
	for _, tc := range cases {
		if got := policy.AllowsWrite(tc.path); got != tc.want {
			t.Errorf("AllowsWrite(%q) = %v, want %v（%s）", tc.path, got, tc.want, tc.note)
		}
	}
}

// 目录形式的只读子路径要挡住整棵子树（hooks/、modules/*/config）。
func TestAllowsWriteReadOnlySubpathCoversSubtree(t *testing.T) {
	root := newWorkspace(t)
	hook := filepath.Join(root, ".git", "hooks", "pre-commit")
	writeFileAt(t, hook, "#!/bin/sh\nexit 0\n")
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithReadOnlySubpaths(root, ".git/hooks"),
	)
	if policy.AllowsWrite(hook) {
		t.Fatalf("AllowsWrite(%q) = true, want false (hooks 整棵子树只读)", hook)
	}
	if policy.AllowsWrite(filepath.Join(root, ".git", "objects", "x")) != true {
		t.Fatal("objects 应当可写")
	}
}

// 全局保护路径（ProtectedPaths）同样进 AllowsWrite——它是"更硬"的那一层。
func TestAllowsWriteHonoursProtectedPaths(t *testing.T) {
	root := newWorkspace(t)
	secret := filepath.Join(root, ".aws", "credentials")
	writeFileAt(t, secret, "x\n")
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithProtectedPaths(filepath.Join(root, ".aws")),
	)
	if policy.AllowsWrite(secret) {
		t.Fatalf("AllowsWrite(%q) = true, want false (protected path)", secret)
	}
}

// 只读模式一个都不许写；danger-full-access 全放行。
func TestAllowsWriteRespectsMode(t *testing.T) {
	root := newWorkspace(t)
	readOnly := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeReadOnly))
	if readOnly.AllowsWrite(filepath.Join(root, "a.txt")) {
		t.Fatal("read-only 模式下 AllowsWrite 必须为 false")
	}
	full := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeDangerFullAccess))
	if !full.AllowsWrite(filepath.Join(root, "..", "anything.txt")) {
		t.Fatal("danger-full-access 下 AllowsWrite 必须放行")
	}
}

// 工具链缓存：默认开启；关掉后回到纯白名单。
func TestToolchainCachesDefaultOnAndSwitchable(t *testing.T) {
	root := newWorkspace(t)
	on := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	if !on.IncludeToolchainCaches() {
		t.Fatal("默认必须开启工具链缓存（否则 go build/npm ci/pip install 在沙箱里全失败）")
	}
	off := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithToolchainCaches(false),
	)
	if off.IncludeToolchainCaches() {
		t.Fatal("WithToolchainCaches(false) 之后必须关闭")
	}
}

// .git 的只读子路径清单：config/hooks/info 只读，index/objects/refs 可写。
func TestWorkspaceGitPolicyShape(t *testing.T) {
	root := newWorkspace(t)
	gitDir := filepath.Join(root, ".git")
	for _, dir := range []string{"objects/ab", "refs/heads", "hooks", "info", "modules/sub"} {
		if err := os.MkdirAll(filepath.Join(gitDir, dir), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	writeFileAt(t, filepath.Join(gitDir, "config"), "[core]\n")
	writeFileAt(t, filepath.Join(gitDir, "index"), "idx")
	writeFileAt(t, filepath.Join(gitDir, "modules", "sub", "config"), "[core]\n")

	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.ProtectGitMetadata(root),
	)

	writable := []string{
		filepath.Join(gitDir, "index"),
		filepath.Join(gitDir, "objects", "ab", "cdef"),
		filepath.Join(gitDir, "refs", "heads", "main"),
		filepath.Join(gitDir, "HEAD"),
		filepath.Join(gitDir, "logs", "HEAD"),
		filepath.Join(gitDir, "COMMIT_EDITMSG"),
		filepath.Join(gitDir, "packed-refs"),
	}
	for _, path := range writable {
		if !policy.AllowsWrite(path) {
			t.Errorf("AllowsWrite(%q) = false；git commit 需要它可写", path)
		}
	}
	readOnly := []string{
		filepath.Join(gitDir, "config"),
		filepath.Join(gitDir, "config.worktree"),
		filepath.Join(gitDir, "hooks", "pre-commit"),
		filepath.Join(gitDir, "info", "exclude"),
		filepath.Join(gitDir, "modules", "sub", "config"),
	}
	for _, path := range readOnly {
		if policy.AllowsWrite(path) {
			t.Errorf("AllowsWrite(%q) = true；config/hooks/info 必须只读（可执行代码或凭据外泄面）", path)
		}
	}
}

// ProtectGitMetadata 在非 git 工作区也必须安全（没有 .git 时什么都不加）。
func TestProtectGitMetadataOnNonRepo(t *testing.T) {
	root := newWorkspace(t)
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.ProtectGitMetadata(root),
	)
	if len(policy.WritableRoots()) != 1 {
		t.Fatalf("WritableRoots() = %+v, want just the workspace root", policy.WritableRoots())
	}
	if !policy.AllowsWrite(filepath.Join(root, "a.txt")) {
		t.Fatal("非 git 工作区里普通文件必须可写")
	}
}
