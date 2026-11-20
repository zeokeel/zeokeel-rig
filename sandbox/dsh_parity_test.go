package sandbox_test

// DSH 对齐的语义钉死（policy 层）。
//
// 目标语义：策略层**只做可写根围栏**——可写面 = 工作区根 ∪ 显式追加的可写根 ∪
// 运行期授权，此外只有 `..` / 绝对路径 / 符号链接的包含性检查。
//
// 本文件把"已删除"的强制（保护路径、拒绝 glob、硬链接守卫）与四条"必须继续生效"
// 的围栏（根外写入、只读模式、根内只读子路径、符号链接逃逸）放在一起断言：
// 前者证明工作区根内不再有隐藏的额外拒绝，后者防止围栏被误删。

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// TestDSHParityFormerlyForbiddenPathsAreWritable 以前被"保护路径 / 拒绝 glob"挡住
// 的落点现在都允许写——策略层已经完全没有这两层。
func TestDSHParityFormerlyForbiddenPathsAreWritable(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
	)

	for _, name := range []string{
		".git/config", // 旧：保护路径（.git 控制面）
		".env",        // 旧：拒绝 glob
		"secrets/db.pem",
	} {
		target := filepath.Join(root, filepath.FromSlash(name))
		if err := policy.CheckWritablePath(target); err != nil {
			t.Errorf("CheckWritablePath(%q) = %v, want nil（DSH 对齐：允许写）", name, err)
		}
		if !policy.AllowsWrite(target) {
			t.Errorf("AllowsWrite(%q) = false, want true（DSH 对齐：允许写）", name)
		}
	}
}

// TestDSHParityHardlinkInsideRootIsWritable 硬链接守卫已从判定路径里摘掉：根内一个
// 指向根外文件的硬链接，策略层既不拒绝也不特殊处理（与 DSH 一致——它只做路径
// 包含性检查，不做 inode 级判定）。
func TestDSHParityHardlinkInsideRootIsWritable(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "ws")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(outside, "victim.txt")
	if err := os.WriteFile(target, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "hl")
	if err := os.Link(target, link); err != nil {
		t.Skipf("hard link unsupported here: %v", err)
	}
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	if err := policy.CheckWritablePath(link); err != nil {
		t.Fatalf("CheckWritablePath(根内硬链接) = %v, want nil（硬链接守卫已摘掉）", err)
	}
	if !policy.AllowsWrite(link) {
		t.Fatal("AllowsWrite(根内硬链接) = false, want true")
	}
	// 对照：根外路径本身仍然拒绝（保留的围栏）。
	if err := policy.CheckWritablePath(target); err == nil {
		t.Fatal("根外路径应当仍被拒绝")
	}
}

// TestDSHParityRetainedFences 四条保留下来的围栏一条都不能松。
func TestDSHParityRetainedFences(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))

	// 1) 根外写入。
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := policy.CheckWritablePath(outside); err == nil {
		t.Error("根外写入没有被拒绝")
	}
	// 2) `..` 分量（ResolvePath 层面）。
	if _, err := policy.ResolvePath("../escape.txt"); err == nil {
		t.Error("`..` 没有被拒绝")
	}
	if _, err := policy.ResolvePath(filepath.Join(root, "sub", "..", "..", "escape.txt")); err == nil {
		t.Error("中段 `..` 没有被拒绝")
	}
	// 3) 符号链接逃逸（已存在的目标）。
	outsideDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outsideDir, "secret.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink(outsideDir, filepath.Join(root, "out")); err == nil {
			if _, err := policy.ResolvePath("out/secret.txt"); err == nil {
				t.Error("符号链接逃逸没有被拒绝")
			}
			if err := policy.CheckWritablePath(filepath.Join(root, "out", "secret.txt")); err == nil {
				t.Error("经符号链接的根外写入没有被拒绝")
			}
		}
	}
	// 4) 只读模式。
	readOnly := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeReadOnly))
	if err := readOnly.CheckWritablePath(filepath.Join(root, "a.txt")); err == nil {
		t.Error("只读模式下写入没有被拒绝")
	}
	if !errors.Is(readOnly.CheckWritablePath(filepath.Join(root, "a.txt")), sandbox.ErrDenied) {
		t.Error("只读拒绝没有包装 ErrDenied")
	}
}

// TestDSHParityReadOnlySubpathsStillEnforced 根内只读子路径是唯一保留的"局部只读"
// 机制（例如 gitpolicy 的 .git/config 与 .git/hooks），必须继续生效。
func TestDSHParityReadOnlySubpathsStillEnforced(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithReadOnlySubpaths(root, ".git/config", ".git/hooks"),
	)
	for _, name := range []string{".git/config", ".git/hooks/pre-commit"} {
		target := filepath.Join(root, filepath.FromSlash(name))
		if err := policy.CheckWritablePath(target); err == nil {
			t.Errorf("CheckWritablePath(%q) = nil, want 拒绝（根内只读子路径）", name)
		}
		if policy.AllowsWrite(target) {
			t.Errorf("AllowsWrite(%q) = true, want false（根内只读子路径）", name)
		}
	}
	// 同一根里其他子路径照旧可写。
	if err := policy.CheckWritablePath(filepath.Join(root, ".git", "index")); err != nil {
		t.Errorf(".git/index 应当可写: %v", err)
	}
}
