package sandbox_test

// CheckWritablePath 的**写侧**语义。
//
// 保护路径与拒绝 glob 已从策略层删除，因此 `root/.git/config` 与 `root/.env` 这类
// 落点**允许写**；仍然拒绝的是根外写入、只读模式，以及可写根内部**显式声明**的
// 只读子路径（.git/config、.git/hooks 走的是这条，见 writable_test.go 与
// gitpolicy.go）。

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
	return policy, root
}

// TestCheckWritablePathAllowsFormerProtectedPaths 断言"保护路径"不再拦写入。
//
// DSH 对齐：DSH 没有保护路径概念，`.git` 就是工作区里一个普通目录。这里显式把旧
// 版本会拒绝的四个落点都写出来，防止有人误以为保护还在。
func TestCheckWritablePathAllowsFormerProtectedPaths(t *testing.T) {
	policy, root := newProtectedPolicy(t)
	for _, target := range []string{
		filepath.Join(root, ".git"),
		filepath.Join(root, ".git", "config"),
		filepath.Join(root, ".git", "objects", "pack.idx"),
		filepath.Join(root, ".git", "not", "created", "yet.txt"),
	} {
		if err := policy.CheckWritablePath(target); err != nil {
			t.Fatalf("CheckWritablePath(%q) = %v, want nil（保护路径已停用）", target, err)
		}
	}
	// 普通文件照旧可写。
	if err := policy.CheckWritablePath(filepath.Join(root, "src", "main.go")); err != nil {
		t.Fatalf("普通路径被拒绝: %v", err)
	}
}

// TestCheckWritablePathSymlinks 区分两种链接：指向根内 → 放行；指向根外 → 仍然
// 拒绝（这是保留下来的"符号链接包含性检查"）。
func TestCheckWritablePathSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上创建符号链接需要额外权限")
	}
	policy, root := newProtectedPolicy(t)
	if err := os.Symlink(filepath.Join(root, ".git"), filepath.Join(root, "shortcut")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	// 以 .git 为目标的链接：旧版本借保护路径拒绝，现在按"根内"放行。
	if err := policy.CheckWritablePath(filepath.Join(root, "shortcut", "config")); err != nil {
		t.Fatalf("根内符号链接写入被拒绝: %v", err)
	}

	// 指向根外的链接仍然必须拒绝。
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "victim.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	err := policy.CheckWritablePath(filepath.Join(root, "escape", "victim.txt"))
	if err == nil {
		t.Fatal("经符号链接指向根外的写入没有被拒绝")
	}
	if !errors.Is(err, sandbox.ErrDenied) {
		t.Fatalf("err = %v, want sandbox.ErrDenied", err)
	}
}

// TestCheckWritablePathStillEnforcesRoots 断言没有削弱保留的围栏：根外路径与只读
// 模式仍然拒绝。
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
	// 文件名不是判据：`app.env` 在可写根内就是可写。
	writable := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	if err := writable.CheckWritablePath(filepath.Join(root, "app.env")); err != nil {
		t.Fatalf("app.env 应当可写: %v", err)
	}
}

// TestCheckWritablePathNilPolicy 断言 nil 策略不 panic（工具在遗留模式下会传 nil）。
func TestCheckWritablePathNilPolicy(t *testing.T) {
	var policy *sandbox.Policy
	if err := policy.CheckWritablePath("/tmp/x"); err == nil {
		t.Fatal("nil 策略应当拒绝（没有配置根）")
	}
}
