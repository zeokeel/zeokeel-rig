//go:build linux

package osrunner

// Landlock 后端下的 git 能力回归（TDD：先写失败用例，再修展开逻辑）。
//
// 背景：Landlock 的 path-beneath 规则是**并集**语义——只要某条祖先规则授予了某个
// 访问权，更深、更严的子路径规则不会收窄它。因此"根可写、但根里某个子路径只读"
// 无法直接表达，实现的做法是把写授权下移到子项。代价是：受保护路径的**父目录**
// 本身没有 MAKE_*，于是 `git commit` 要写 `.git/index.lock`（在 .git 里新建条目）
// 就失败了——同一份 Policy 在 bwrap 下能提交、在 Landlock 下不能，属于能力不一致。

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// landlockGitRepo 建一个真实 git 仓库（一次提交），返回仓库路径。
func landlockGitRepo(t *testing.T) string {
	t.Helper()
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	for _, argv := range [][]string{
		{"git", "-C", ws, "init", "-q"},
		{"sh", "-c", "printf 'hi\n' > " + filepath.Join(ws, "a.txt")},
		{"git", "-C", ws, "add", "a.txt"},
		{"git", "-C", ws, "commit", "-q", "-m", "first"},
	} {
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Env = env
		if out, runErr := cmd.CombinedOutput(); runErr != nil {
			t.Fatalf("setup %v: %v %s", argv, runErr, out)
		}
	}
	return ws
}

// landlockRunnerForTest 构造一个只走 Landlock 的 runner（跳过 bwrap 的平台链）。
func landlockRunnerForTest(t *testing.T) *landlockRunner {
	t.Helper()
	abi, ok := probeLandlock(DefaultProbeTimeout)
	if !ok {
		t.Skip("本机 Landlock 不可用")
	}
	exe, err := os.Executable()
	if err != nil || exe == "" {
		t.Skip("拿不到 helper 可执行文件")
	}
	return &landlockRunner{exe: exe, abi: abi}
}

// runConfined 在给定策略下经 Landlock 执行一条 shell 命令，返回输出与错误。
func runConfined(t *testing.T, runner *landlockRunner, policy *sandbox.Policy, script string) (string, error) {
	t.Helper()
	// 目标命令必须是绝对路径：helper 用 syscall.Exec 直接 exec argv[0]，不做 PATH 查找。
	confined, err := runner.Confine([]string{"/bin/sh", "-c", script}, policy)
	if err != nil {
		t.Fatalf("Confine: %v", err)
	}
	cmd := exec.Command(confined.Argv[0], confined.Argv[1:]...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, runErr := cmd.CombinedOutput()
	return string(out), runErr
}

// 核心回归：Landlock 下 `git commit` 必须成功（与 bwrap 能力一致）。
func TestLandlockGitCommitWorks(t *testing.T) {
	ws := landlockGitRepo(t)
	runner := landlockRunnerForTest(t)
	policy := sandbox.New(
		sandbox.WithRoots(ws),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.ProtectGitMetadata(ws),
	)
	out, err := runConfined(t, runner, policy,
		"cd "+ws+" && printf 'more\n' >> a.txt && git add a.txt && git commit -q -m second && echo COMMIT_OK")
	if err != nil || !strings.Contains(out, "COMMIT_OK") {
		t.Fatalf("Landlock 下 git commit 失败（能力与 bwrap 不一致）：err=%v\n输出：\n%s", err, out)
	}
}

// 关键不变量：能在 .git 里新建条目（index.lock 的创建是提交的前提）。
func TestLandlockGitDirAcceptsNewEntries(t *testing.T) {
	ws := landlockGitRepo(t)
	runner := landlockRunnerForTest(t)
	policy := sandbox.New(
		sandbox.WithRoots(ws),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.ProtectGitMetadata(ws),
	)
	lock := filepath.Join(ws, ".git", "index.lock")
	out, err := runConfined(t, runner, policy, "touch "+lock+" && echo TOUCH_OK")
	if err != nil || !strings.Contains(out, "TOUCH_OK") {
		t.Fatalf("Landlock 下无法在 .git 里新建条目（git 无法提交）：err=%v\n输出：\n%s", err, out)
	}
}

// 如实上报：如果 Landlock 只能降级（.git 整体可写），RunnerLimits 必须说出来。
func TestLandlockLimitsMentionsGitDegradation(t *testing.T) {
	runner := landlockRunnerForTest(t)
	limits := runner.RunnerLimits()
	if !strings.Contains(limits, "git") && !strings.Contains(limits, ".git") {
		t.Fatalf("Landlock 的 RunnerLimits 没有说明 .git 的降级语义：%q", limits)
	}
}

// 降级必须是**如实**的：Landlock 下 `.git/config` 确实可写（这是降级的代价），
// 而 RunnerLimits 必须把这条说清楚。用例锁住"事实"和"上报"两者，避免以后有人
// 改成"假装还保护着"。
func TestLandlockGitControlPlaneDegradesAndIsReported(t *testing.T) {
	ws := landlockGitRepo(t)
	runner := landlockRunnerForTest(t)
	policy := sandbox.New(
		sandbox.WithRoots(ws),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.ProtectGitMetadata(ws),
	)
	configPath := filepath.Join(ws, ".git", "config")
	out, err := runConfined(t, runner, policy, "printf 'x\\n' >> "+configPath+" && echo CONFIG_WRITTEN")
	if err == nil && strings.Contains(out, "CONFIG_WRITTEN") {
		limits := runner.RunnerLimits()
		if !strings.Contains(limits, ".git/config") {
			t.Fatalf("Landlock 下 .git/config 可写，但 RunnerLimits 没有如实上报：%q", limits)
		}
		return
	}
	// 若将来内核/Landlock 支持否定规则而不再降级，这条用例也要跟着变；
	// 此时必须保证"保护是真的"，而不是默默失败。
	if !strings.Contains(out, "Permission denied") && !strings.Contains(out, "Read-only") {
		t.Fatalf("写入 .git/config 既没成功也没被拒绝：err=%v\n输出：\n%s", err, out)
	}
}

// 非降级根必须保持严格：缓存根里的凭据文件在 Landlock 下仍然只读。
func TestLandlockKeepsNonDegradableSubpathsReadOnly(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	cred := filepath.Join(root, "credentials.toml")
	if writeErr := os.WriteFile(cred, []byte("token\n"), 0o600); writeErr != nil {
		t.Fatalf("prep: %v", writeErr)
	}
	runner := landlockRunnerForTest(t)
	policy := sandbox.New(
		sandbox.WithRoots(t.TempDir()),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithToolchainCaches(false),
		// 显式追加一个**不可降级**的缓存根，凭据文件是它的只读子路径。
		sandbox.WithWritableRoot(root, "credentials.toml"),
	)
	out, runErr := runConfined(t, runner, policy, "printf 'stolen\\n' >> "+cred+" && echo CRED_WRITTEN")
	if runErr == nil && strings.Contains(out, "CRED_WRITTEN") {
		t.Fatalf("Landlock 下凭据文件被写穿了（不可降级根没有严格展开）：\n%s", out)
	}
}

// 展开不能把工作区根本身变成只读：否则 `echo x > ws/new.txt`、`mkdir ws/sub`
// 全部失败（比 git commit 更严重的可用性缺陷）。保护项若已由可降级根覆盖，
// 就不该再驱动父目录展开。
func TestLandlockWorkspaceRootAcceptsNewEntries(t *testing.T) {
	ws := landlockGitRepo(t)
	runner := landlockRunnerForTest(t)
	policy := sandbox.New(
		sandbox.WithRoots(ws),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.ProtectGitMetadata(ws),
	)
	for _, tc := range []struct{ name, script string }{
		{"新建文件", "echo new > " + filepath.Join(ws, "newfile.txt") + " && echo FILE_OK"},
		{"新建目录", "mkdir -p " + filepath.Join(ws, "sub", "dir") + " && echo DIR_OK"},
	} {
		out, err := runConfined(t, runner, policy, tc.script)
		if err != nil {
			t.Fatalf("%s 在 Landlock 下失败：err=%v\n输出：\n%s", tc.name, err, out)
		}
	}
}
