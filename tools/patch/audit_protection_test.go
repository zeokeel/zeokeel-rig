package patch_test

// 保护路径 / 拒绝 glob 的**真实落点**判定（**已停用强制，DSH 对齐**）。
//
// 旧语义：词法路径（root/alias/config）不匹配 root/.git，但经目录链接后实际写入落在
// .git 里——旧版本要求策略按"真实落点"拒绝。DSH 对齐后策略层只做可写根围栏：
// 保护路径与拒绝 glob 都是记录型 no-op，所以这些形态现在**必须成功**（它们都在根内，
// 根围栏本来就允许）。
//
// 本文件是旧 audit_protection_test.go 的反向版本；仍然拒绝的形态（根外写入、`..`、
// 逃出根目录的符号链接）由包内其它用例与 sandbox 包覆盖。

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/patch"
)

// formerProtectedFixture 构造 root/.git/config 与 root/secrets/x.md，并**声明**
// 保护路径 + 拒绝 glob（声明只是记录，不产生任何限制）。
func formerProtectedFixture(t *testing.T, denied ...string) *fixture {
	t.Helper()
	root := t.TempDir()
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithDeniedGlobs(denied...),
	)
	policy.AddProtectedPaths(sandbox.WorkspaceMetadataPaths(root)...)
	f := &fixture{root: root}
	f.tool = patch.New(root, patch.WithPolicy(policy))
	return f
}

// TestFormerlyProtectedDirLinkIsWritable：alias -> .git，补丁写 alias/config。
//
// 旧用例 TestProtectionDirLinkIsRejected 的反向版本：现在必须成功，且真的落到
// .git/config。
func TestFormerlyProtectedDirLinkIsWritable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上创建符号链接需要额外权限")
	}
	f := formerProtectedFixture(t)
	f.write(t, ".git/config", "orig\n")
	if err := os.Symlink(".git", filepath.Join(f.root, "alias")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	result := f.invoke(t, "*** Begin Patch\n*** Update File: alias/config\n@@\n-orig\n+legit\n*** End Patch\n")
	if result.IsError {
		t.Fatalf("经目录链接写 .git/config 被拒绝（保护已停用）: %s", result.Content)
	}
	if got := f.read(t, ".git/config"); got != "legit\n" {
		t.Fatalf(".git/config = %q, want 写入生效", got)
	}
}

// TestFormerlyProtectedFinalLinkIsWritable：link -> alias/config（alias -> .git）。
//
// 旧用例 TestProtectionFinalLinkToLinkedPathIsRejected 的反向版本。
func TestFormerlyProtectedFinalLinkIsWritable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上创建符号链接需要额外权限")
	}
	f := formerProtectedFixture(t)
	f.write(t, ".git/config", "orig\n")
	if err := os.Symlink(".git", filepath.Join(f.root, "alias")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if err := os.Symlink("alias/config", filepath.Join(f.root, "link")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	result := f.invoke(t, "*** Begin Patch\n*** Update File: link\n@@\n-orig\n+legit\n*** End Patch\n")
	if result.IsError {
		t.Fatalf("经末段链接写 .git/config 被拒绝（保护已停用）: %s", result.Content)
	}
	if got := f.read(t, ".git/config"); got != "legit\n" {
		t.Fatalf(".git/config = %q, want 写入生效", got)
	}
}

// TestFormerlyProtectedDeleteIsAllowed：删除 .git 里的文件现在也允许。
//
// 旧用例 TestProtectionDeleteIsRejected 的反向版本。
func TestFormerlyProtectedDeleteIsAllowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上创建符号链接需要额外权限")
	}
	for _, target := range []string{".git/config", ".git/hooks/pre-commit"} {
		f := formerProtectedFixture(t)
		f.write(t, ".git/config", "orig\n")
		f.write(t, ".git/hooks/pre-commit", "#!/bin/sh\n")
		result := f.invoke(t, "*** Begin Patch\n*** Delete File: "+target+"\n*** End Patch\n")
		if result.IsError {
			t.Fatalf("删除 %s 被拒绝（保护已停用）: %s", target, result.Content)
		}
		if f.exists(t, target) {
			t.Fatalf("%s 仍然存在，删除没有生效", target)
		}
	}
	// 经目录链接删除同样允许。
	f := formerProtectedFixture(t)
	f.write(t, ".git/config", "orig\n")
	if err := os.Symlink(".git", filepath.Join(f.root, "alias")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if result := f.invoke(t, "*** Begin Patch\n*** Delete File: alias/config\n*** End Patch\n"); result.IsError {
		t.Fatalf("经目录链接删除被拒绝（保护已停用）: %s", result.Content)
	}
}

// TestFormerlyProtectedMoveIsAllowed：从 .git 移出、以及把普通文件移进 .git 都允许。
//
// 旧用例 TestProtectionMoveOutOfProtectedIsRejected 的反向版本。
func TestFormerlyProtectedMoveIsAllowed(t *testing.T) {
	f := formerProtectedFixture(t)
	f.write(t, ".git/config", "orig\n")
	result := f.invoke(t, "*** Begin Patch\n*** Update File: .git/config\n*** Move to: moved.txt\n*** End Patch\n")
	if result.IsError {
		t.Fatalf("从 .git 移出被拒绝（保护已停用）: %s", result.Content)
	}
	if f.exists(t, ".git/config") {
		t.Fatal(".git/config 还在，移动没有生效")
	}
	if !f.exists(t, "moved.txt") {
		t.Fatal("moved.txt 没有出现")
	}

	// 目标落在 .git 内同样允许。
	f = formerProtectedFixture(t)
	f.write(t, "plain.txt", "x\n")
	result = f.invoke(t, "*** Begin Patch\n*** Update File: plain.txt\n*** Move to: .git/moved-here.txt\n*** End Patch\n")
	if result.IsError {
		t.Fatalf("移入 .git 被拒绝（保护已停用）: %s", result.Content)
	}
	if !f.exists(t, ".git/moved-here.txt") {
		t.Fatal(".git/moved-here.txt 没有出现")
	}
}

// TestDeniedGlobIsNoLongerEnforced：拒绝 glob（secrets/**）三种形态都允许写。
//
// 旧用例 TestDenyGlobThroughDirLinkAndFinalLink 的反向版本。
func TestDeniedGlobIsNoLongerEnforced(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上创建符号链接需要额外权限")
	}
	f := formerProtectedFixture(t, "secrets/**")
	f.write(t, "secrets/x.md", "token\n")
	// 直接写。
	if result := f.invoke(t, "*** Begin Patch\n*** Update File: secrets/x.md\n@@\n-token\n+LEGIT\n*** End Patch\n"); result.IsError {
		t.Fatalf("直接写 secrets/x.md 被拒绝（glob 已停用）: %s", result.Content)
	}
	// 目录链接。
	if err := os.Symlink("secrets", filepath.Join(f.root, "shared")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if result := f.invoke(t, "*** Begin Patch\n*** Update File: shared/x.md\n@@\n-LEGIT\n+VIA-LINK\n*** End Patch\n"); result.IsError {
		t.Fatalf("经目录链接写被拒绝（glob 已停用）: %s", result.Content)
	}
	// 末段链接指向"经链接的路径"。
	f.write(t, "secrets/y.md", "token\n")
	if err := os.Symlink("shared/y.md", filepath.Join(f.root, "link2")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if result := f.invoke(t, "*** Begin Patch\n*** Update File: link2\n@@\n-token\n+VIA-FINAL-LINK\n*** End Patch\n"); result.IsError {
		t.Fatalf("经末段链接写被拒绝（glob 已停用）: %s", result.Content)
	}
	if got := f.read(t, "secrets/x.md"); got != "VIA-LINK\n" {
		t.Fatalf("secrets/x.md = %q, want 目录链接写入生效", got)
	}
	if got := f.read(t, "secrets/y.md"); got != "VIA-FINAL-LINK\n" {
		t.Fatalf("secrets/y.md = %q, want 末段链接写入生效", got)
	}
}
