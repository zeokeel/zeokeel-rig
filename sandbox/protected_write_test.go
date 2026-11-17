package sandbox_test

// 保护路径的**写侧**语义：ProtectedPaths 声称"即使在可写根之内也必须保持只读"，
// 因此写工具必须能在落盘前问出"这个绝对路径能不能写"。CheckWritablePath 就是
// 那个判定（读路径不受影响：读 .git/HEAD 仍然合法）。

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

func newProtectedPolicy(t *testing.T) (*sandbox.Policy, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git", "objects"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "config"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	policy.AddProtectedPaths(sandbox.WorkspaceMetadataPaths(root)...)
	return policy, root
}

// TestCheckWritablePathRejectsProtected 断言保护路径内部的写入点被拒，且错误可读、
// 可判定（errors.Is(err, sandbox.ErrDenied)）。
func TestCheckWritablePathRejectsProtected(t *testing.T) {
	policy, root := newProtectedPolicy(t)
	for _, target := range []string{
		filepath.Join(root, ".git"),
		filepath.Join(root, ".git", "config"),
		filepath.Join(root, ".git", "objects", "pack.idx"),
		filepath.Join(root, ".git", "not", "created", "yet.txt"),
	} {
		err := policy.CheckWritablePath(target)
		if err == nil {
			t.Fatalf("CheckWritablePath(%q) = nil，期望被拒绝", target)
		}
		if !errors.Is(err, sandbox.ErrDenied) {
			t.Fatalf("CheckWritablePath(%q) err = %v，期望 sandbox.ErrDenied", target, err)
		}
	}
	// 普通文件不受影响。
	if err := policy.CheckWritablePath(filepath.Join(root, "src", "main.go")); err != nil {
		t.Fatalf("普通路径被拒绝: %v", err)
	}
}

// TestCheckWritablePathFollowsSymlinks 断言绕过形态：建一个指向 .git 的目录链接，
// 经它写子路径必须同样被拒（词法路径看不出来的那种）。
func TestCheckWritablePathFollowsSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上创建符号链接需要额外权限")
	}
	policy, root := newProtectedPolicy(t)
	if err := os.Symlink(filepath.Join(root, ".git"), filepath.Join(root, "shortcut")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if err := policy.CheckWritablePath(filepath.Join(root, "shortcut", "config")); err == nil {
		t.Fatal("经符号链接指向保护路径的写入没有被拒绝")
	}
	// 相对链接 + 更深一层同样要拦。
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink("../.git", filepath.Join(root, "src", "up")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if err := policy.CheckWritablePath(filepath.Join(root, "src", "up", "objects", "x")); err == nil {
		t.Fatal("经相对符号链接指向保护路径的写入没有被拒绝")
	}
}

// TestCheckWritablePathStillEnforcesRoots 断言它没有削弱原有围栏：根外路径、只读
// 模式、拒绝 glob 都照旧拒绝。
func TestCheckWritablePathStillEnforcesRoots(t *testing.T) {
	policy, root := newProtectedPolicy(t)
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := policy.CheckWritablePath(outside); err == nil {
		t.Fatalf("根外路径 %q 没有被拒绝", outside)
	}
	readOnly := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeReadOnly))
	if err := readOnly.CheckWritablePath(filepath.Join(root, "a.txt")); err == nil {
		t.Fatal("只读模式下写入没有被拒绝")
	}
	globs := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithDeniedGlobs("**/*.env"))
	if err := globs.CheckWritablePath(filepath.Join(root, "app.env")); err == nil {
		t.Fatal("拒绝 glob 命中的写入没有被拒绝")
	}
}

// TestCheckWritablePathNilPolicy 断言 nil 策略不 panic（工具在遗留模式下会传 nil）。
func TestCheckWritablePathNilPolicy(t *testing.T) {
	var policy *sandbox.Policy
	if err := policy.CheckWritablePath("/tmp/x"); err == nil {
		t.Fatal("nil 策略应当拒绝（没有配置根）")
	}
}
