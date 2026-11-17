package patch_test

// 保护路径/拒绝 glob 的**真实落点**判定。
//
// 词法路径（root/alias/config）不匹配 root/.git，但经目录链接后实际写入落在
// .git 里——只做词法比较就是一条策略绕过。这些用例把三种形态钉死：
//   1. 目录链接：alias -> .git，patch alias/config；
//   2. 末段链接指向"链接路径"：link -> alias/config（alias -> .git）；
//   3. 同族：alias -> secrets + deny glob secrets/**。
// 并覆盖 opDelete（删除也是修改）与纯移动的目标。

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/patch"
)

// protectedFixture 构造 root/.git/config 与 root/secrets/x.md 并开启保护/glob。
func protectedFixture(t *testing.T, denied ...string) *fixture {
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

// TestProtectionDirLinkIsRejected：alias -> .git，词法路径是 alias/config。
func TestProtectionDirLinkIsRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上创建符号链接需要额外权限")
	}
	f := protectedFixture(t)
	f.write(t, ".git/config", "orig\n")
	if err := os.Symlink(".git", filepath.Join(f.root, "alias")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	result := f.invoke(t, "*** Begin Patch\n*** Update File: alias/config\n@@\n-orig\n+hijacked\n*** End Patch\n")
	requireError(t, result)
	if got := f.read(t, ".git/config"); got != "orig\n" {
		t.Fatalf(".git/config 被改写: %q", got)
	}
}

// TestProtectionFinalLinkToLinkedPathIsRejected：link -> alias/config，其中
// alias -> .git。目录链在 link 之前没有链接，只有**末段**是链接，且它的目标本身
// 又是一条经目录链接的路径——只解析末段、不重新解析目标就会漏判。
func TestProtectionFinalLinkToLinkedPathIsRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上创建符号链接需要额外权限")
	}
	f := protectedFixture(t)
	f.write(t, ".git/config", "orig\n")
	if err := os.Symlink(".git", filepath.Join(f.root, "alias")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if err := os.Symlink("alias/config", filepath.Join(f.root, "link")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	result := f.invoke(t, "*** Begin Patch\n*** Update File: link\n@@\n-orig\n+hijacked\n*** End Patch\n")
	requireError(t, result)
	if got := f.read(t, ".git/config"); got != "orig\n" {
		t.Fatalf(".git/config 被改写: %q", got)
	}
}

// TestProtectionDeleteIsRejected：删除也是修改，保护路径必须拦住 rm。
func TestProtectionDeleteIsRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上创建符号链接需要额外权限")
	}
	for _, target := range []string{".git/config", ".git/hooks/pre-commit"} {
		f := protectedFixture(t)
		f.write(t, ".git/config", "orig\n")
		f.write(t, ".git/hooks/pre-commit", "#!/bin/sh\n")
		result := f.invoke(t, "*** Begin Patch\n*** Delete File: "+target+"\n*** End Patch\n")
		requireError(t, result)
		if !f.exists(t, ".git/config") {
			t.Fatalf("%s: .git/config 被删掉了", target)
		}
		if !f.exists(t, ".git/hooks/pre-commit") {
			t.Fatalf("%s: .git/hooks/pre-commit 被删掉了", target)
		}
	}
	// 经目录链接删除同样拒绝。
	f := protectedFixture(t)
	f.write(t, ".git/config", "orig\n")
	if err := os.Symlink(".git", filepath.Join(f.root, "alias")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	result := f.invoke(t, "*** Begin Patch\n*** Delete File: alias/config\n*** End Patch\n")
	requireError(t, result)
	if !f.exists(t, ".git/config") {
		t.Fatal("经目录链接删除了保护文件")
	}
}

// TestProtectionMoveOutOfProtectedIsRejected：移动源在保护区、目标在区外，同样是
// 对保护目录的修改。
func TestProtectionMoveOutOfProtectedIsRejected(t *testing.T) {
	f := protectedFixture(t)
	f.write(t, ".git/config", "orig\n")
	result := f.invoke(t, "*** Begin Patch\n*** Update File: .git/config\n*** Move to: stolen.txt\n*** End Patch\n")
	requireError(t, result)
	if !f.exists(t, ".git/config") {
		t.Fatal(".git/config 被移走了")
	}
	if f.exists(t, "stolen.txt") {
		t.Fatal("保护文件被复制/移动到区外")
	}
	// 目标落在保护区内同样拒绝。
	f = protectedFixture(t)
	f.write(t, "plain.txt", "x\n")
	result = f.invoke(t, "*** Begin Patch\n*** Update File: plain.txt\n*** Move to: .git/plants.txt\n*** End Patch\n")
	requireError(t, result)
	if f.exists(t, ".git/plants.txt") {
		t.Fatal("移动目标写进了保护区")
	}
}

// TestDenyGlobThroughDirLinkAndFinalLink：拒绝 glob 也必须按真实落点判定。
func TestDenyGlobThroughDirLinkAndFinalLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上创建符号链接需要额外权限")
	}
	f := protectedFixture(t, "secrets/**")
	f.write(t, "secrets/x.md", "token\n")
	// 对照组：直接写被拒。
	requireError(t, f.invoke(t, "*** Begin Patch\n*** Update File: secrets/x.md\n@@\n-token\n+LEAK\n*** End Patch\n"))
	// 目录链接。
	if err := os.Symlink("secrets", filepath.Join(f.root, "shared")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	requireError(t, f.invoke(t, "*** Begin Patch\n*** Update File: shared/x.md\n@@\n-token\n+LEAK\n*** End Patch\n"))
	// 末段链接指向"经链接的路径"。
	f.write(t, "secrets/y.md", "token\n")
	if err := os.Symlink("shared/y.md", filepath.Join(f.root, "link2")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	requireError(t, f.invoke(t, "*** Begin Patch\n*** Update File: link2\n@@\n-token\n+LEAK\n*** End Patch\n"))
	for _, path := range []string{"secrets/x.md", "secrets/y.md"} {
		if got := f.read(t, path); strings.Contains(got, "LEAK") {
			t.Fatalf("%s 被改写成 %q", path, got)
		}
	}
}
