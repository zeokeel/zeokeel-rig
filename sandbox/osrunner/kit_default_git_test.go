package osrunner

// 锁住"kit 默认保护 + bwrap → git commit 成功、.git/config 仍然只读"。
//
// 这条来自一次**过期审计结论**的纠正：审计说"`.git` 保护 + 挂 runner → git
// commit 必失败"。用 kit 的真实默认保护复跑（AddGitProtection +
// WorkspaceNonGitMetadataPaths + bwrap）时提交是成功的——因为 `.git` 不再是
// "整体只读"，而是"数据面可写、控制面只读"（config/hooks/info 在
// WriteDenyPaths 里，index/objects/refs 可写）。这条用例把两半都钉住，防止以后
// 有人按旧结论把 `.git` 从保护名单里删掉（那会退回整体放开）。
//
// 门控：RIG_SANDBOX_TOOLCHAIN_TEST=1（依赖真实 git 与 bwrap）。

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// kitDefaultProtectionPolicy 复刻 kit.addDefaultProtection 的默认保护形状。
func kitDefaultProtectionPolicy(t *testing.T, ws string) *sandbox.Policy {
	t.Helper()
	policy := sandbox.New(
		sandbox.WithRoots(ws),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
	)
	// 与 kit.addDefaultProtection 一致：
	policy.AddGitProtection(ws)
	policy.AddProtectedPaths(sandbox.WorkspaceNonGitMetadataPaths(ws)...)
	return policy
}

func TestKitDefaultProtectionDoesNotBreakGitCommitUnderBwrap(t *testing.T) {
	toolchainSkipUnlessGated(t)
	runner := localBwrap(t)
	ws := landlockGitRepo(t)

	policy := kitDefaultProtectionPolicy(t, ws)

	// 提交必须成功。
	out, err := runBwrap(t, runner, policy, "/bin/sh", "-c",
		"cd "+ws+" && printf 'more\\n' >> a.txt && git add a.txt && git commit -q -m second && git log --oneline | head -2")
	if err != nil || !strings.Contains(out, "second") {
		t.Fatalf("kit 默认保护 + bwrap 下 git commit 失败：err=%v\n%s", err, out)
	}

	// 控制面必须仍然只读。
	configPath := filepath.Join(ws, ".git", "config")
	hookPath := filepath.Join(ws, ".git", "hooks", "pre-commit")
	for _, target := range []string{configPath, hookPath} {
		out, _ := runBwrap(t, runner, policy, "/bin/sh", "-c", "printf 'x\\n' >> "+target+" 2>&1; echo END")
		if !strings.Contains(out, "Read-only file system") && !strings.Contains(out, "Permission denied") {
			t.Fatalf("%s 在沙箱内可写（保护失效）：%q", target, strings.TrimSpace(out))
		}
	}

	// 数据面必须可写（这是提交的前提，反向锁住"别把 .git 整体剔掉"）。
	out, err = runBwrap(t, runner, policy, "/bin/sh", "-c",
		"touch "+filepath.Join(ws, ".git", "probe.data")+" && echo DATA_OK")
	if err != nil || !strings.Contains(out, "DATA_OK") {
		t.Fatalf(".git 数据面不可写：err=%v\n%s", err, out)
	}
	_ = os.Remove(filepath.Join(ws, ".git", "probe.data"))
}

// 反向：`.git` 不能再回到"整体保护"（那会让提交失败）。
func TestKitDefaultPolicyDoesNotProtectWholeGitDir(t *testing.T) {
	ws := landlockGitRepo(t)
	policy := kitDefaultProtectionPolicy(t, ws)
	gitDir := filepath.Join(ws, ".git")
	for _, protected := range policy.ProtectedPaths() {
		if sandbox.PathEqual(protected, gitDir) {
			t.Fatalf(".git 又被整体放进 ProtectedPaths：%v（会退回 git commit 必失败）", policy.ProtectedPaths())
		}
	}
	if err := policy.CheckWritablePath(filepath.Join(gitDir, "index")); err != nil {
		t.Fatalf(".git/index 不可写（提交会失败）：%v", err)
	}
	if err := policy.CheckWritablePath(filepath.Join(gitDir, "config")); err == nil {
		t.Fatal(".git/config 在进程内可写（控制面保护失效）")
	}
	_ = exec.Command
}
