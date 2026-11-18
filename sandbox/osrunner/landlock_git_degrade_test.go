package osrunner

// 断言 WithLandlockGitDegrade 开关真的改变 ruleset：默认把 .git 整根降级为 rw
// （git commit 可用），关掉之后回到严格展开（.git 连同控制面只读）。

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

func TestLandlockGitDegradeSwitch(t *testing.T) {
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	for _, dir := range []string{gitDir, filepath.Join(gitDir, "hooks"), filepath.Join(gitDir, "objects")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %q: %v", dir, err)
		}
	}
	for _, file := range []string{"config", "HEAD", "hooks/pre-commit"} {
		if err := os.WriteFile(filepath.Join(gitDir, file), []byte("x"), 0o600); err != nil {
			t.Fatalf("write fixture %q: %v", file, err)
		}
	}
	// ProtectGitMetadata 把 .git 声明成"可降级可写根"（带控制面只读子路径）。
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.ProtectGitMetadata(root),
	)
	wantGit := canonicalPath(t, gitDir)
	wantHooks := canonicalPath(t, filepath.Join(gitDir, "hooks"))

	// --- 默认：降级打开 → .git 整根 rw（git commit 可用），控制面不在只读集 ----
	if !LandlockGitDegradeEnabled() {
		t.Fatal("默认应为 true（保持既有行为）")
	}
	readOnly, readWrite := LandlockGrants(policy)
	if !contains(readWrite, wantGit) {
		t.Fatalf("默认 read-write = %v, want the whole .git %q（否则 git commit 不可用）", readWrite, wantGit)
	}
	// 注意：默认模式下 .git/hooks、.git/config 仍会出现在 --ro 列表里（它们本来就在
	// Protected 里），但 Landlock 是**并集**语义——父目录 .git 已经 rw，更深的 ro
	// 规则收窄不了它，所以它们在 OS 层实际可写。这正是"降级面"，也正是必须由命令
	// 风险层（write.git.control）兜住的原因。
	t.Logf("默认模式：--rw 含 .git=%v；--ro 含 .git/hooks=%v（并集语义下并不生效）",
		contains(readWrite, wantGit), contains(readOnly, wantHooks))

	// --- 关掉降级：严格展开 → .git 只读，控制面仍然只读 -----------------------
	WithLandlockGitDegrade(false)
	t.Cleanup(func() { WithLandlockGitDegrade(true) })
	strictReadOnly, strictReadWrite := LandlockGrants(policy)
	if contains(strictReadWrite, wantGit) {
		t.Fatalf("严格模式 read-write = %v, must not grant .git as a whole", strictReadWrite)
	}
	if contains(strictReadWrite, wantHooks) {
		t.Fatalf("严格模式 read-write = %v, must not grant .git/hooks", strictReadWrite)
	}
	if !contains(strictReadOnly, wantGit) {
		t.Fatalf("严格模式 read-only = %v, want %q", strictReadOnly, wantGit)
	}
	// 工作区里 .git 之外的子项仍然可写（否则连正常开发都做不了）。
	if !contains(strictReadWrite, canonicalPath(t, root)+"/") && !contains(strictReadWrite, canonicalPath(t, root)) {
		t.Logf("严格模式 read-write = %v（根本身可以只读，子项授权见下）", strictReadWrite)
	}
}
