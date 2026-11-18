//go:build darwin

package osrunner

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// 本文件只在 macOS 上编译/运行：验证平台选择、功能探测与真实的 sandbox-exec
// 行为。Linux 上只能用 darwin_pure_test.go 的纯逻辑测试 + GOOS=darwin 交叉编译。

// seatbeltFakeExec 写入一个可执行的假 sandbox-exec，并把它的目录设为唯一 PATH。
//
// 注意：**不能**再指望"把假二进制放进 PATH 就会被选中"——`seatbeltExecutable`
// 优先固定系统路径（见 darwin.go 的说明），真机上 /usr/bin/sandbox-exec 永远存在，
// 所以假二进制必须通过 `sandboxExecSystemPath` 注入（见 useFakeSystemSandboxExec）。
func seatbeltFakeExec(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "sandbox-exec")
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake sandbox-exec: %v", err)
	}
	t.Setenv("PATH", dir)
	return path
}

// seatbeltFakeExecWorking 写一个**行为正确**的假 sandbox-exec：它能通过探测的
// 两个阶段（跑通子进程 + 拒绝自检时非零退出）。
//
// 只写 `exit 0` 的假二进制过不了自检：自检要求"命令失败"，退出 0 会被判成
// "Seatbelt 没生效"。
func seatbeltFakeExecWorking(t *testing.T) string {
	t.Helper()
	return seatbeltFakeExec(t, `while [ $# -gt 0 ] && [ "$1" != "--" ]; do shift; done
[ $# -gt 0 ] && shift
if [ "$1" = "/bin/sh" ]; then
  echo "sandbox-exec: Operation not permitted" >&2
  exit 1
fi
exec "$@"`)
}

// useFakeSystemSandboxExec 把"首选系统路径"指向给定二进制（测试可注入的解析点）。
func useFakeSystemSandboxExec(t *testing.T, path string) {
	t.Helper()
	previous := sandboxExecSystemPath
	sandboxExecSystemPath = path
	t.Cleanup(func() { sandboxExecSystemPath = previous })
}

// disableSystemSandboxExec 模拟"这台机器没有 sandbox-exec"：系统路径不存在，
// 且 LookPath 也找不到。
func disableSystemSandboxExec(t *testing.T) {
	t.Helper()
	useFakeSystemSandboxExec(t, filepath.Join(t.TempDir(), "absent-sandbox-exec"))
	previous := sandboxExecLookPath
	sandboxExecLookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	t.Cleanup(func() { sandboxExecLookPath = previous })
}

// 没有 sandbox-exec 时 newPlatformRunner 必须返回 ErrSandboxUnavailable。
func TestNewPlatformRunnerMissingSandboxExec(t *testing.T) {
	disableSystemSandboxExec(t)
	t.Setenv("PATH", t.TempDir())
	runner, err := New(Config{ProbeTimeout: time.Second})
	if err == nil {
		t.Fatalf("New = %v, want error when sandbox-exec is missing", runner)
	}
	if runner != nil {
		t.Fatalf("New returned runner %v with error %v, want nil runner", runner, err)
	}
	if !errors.Is(err, sandbox.ErrSandboxUnavailable) {
		t.Fatalf("New error = %v, want ErrSandboxUnavailable", err)
	}
}

// 探测失败（sandbox-exec 存在但不可用）也必须 fail closed。
func TestNewPlatformRunnerProbeFailure(t *testing.T) {
	useFakeSystemSandboxExec(t, seatbeltFakeExec(t, "exit 9"))
	if _, err := New(Config{ProbeTimeout: time.Second}); !errors.Is(err, sandbox.ErrSandboxUnavailable) {
		t.Fatalf("New error = %v, want ErrSandboxUnavailable", err)
	}
}

// 系统路径不可用时回退到 PATH 查找（回退分支本身也要被测到）。
func TestNewPlatformRunnerFallsBackToPath(t *testing.T) {
	fake := seatbeltFakeExecWorking(t)
	useFakeSystemSandboxExec(t, filepath.Join(t.TempDir(), "absent-sandbox-exec"))
	runner, err := New(Config{ProbeTimeout: time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	seatbelt, ok := runner.(*seatbeltRunner)
	if !ok {
		t.Fatalf("runner 类型 = %T, want *seatbeltRunner", runner)
	}
	if seatbelt.execPath != fake {
		t.Fatalf("execPath = %q, want 回退到 PATH 里的 %q", seatbelt.execPath, fake)
	}
}

// 探测成功后 runner 被缓存（New 只选择一次），Confine 用探测到的可执行路径包装。
func TestNewPlatformRunnerSelectsFakeSandboxExec(t *testing.T) {
	fake := seatbeltFakeExecWorking(t)
	useFakeSystemSandboxExec(t, fake)
	runner, err := New(Config{ProbeTimeout: time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if runner == nil {
		t.Fatal("New returned a nil runner")
	}

	policy := sandbox.New(sandbox.WithRoots(t.TempDir()), sandbox.WithMode(sandbox.ModeReadOnly))
	confined, err := runner.Confine([]string{"/bin/true"}, policy)
	if err != nil {
		t.Fatalf("Confine: %v", err)
	}
	if got, want := confined.Argv[0], fake; got != want {
		t.Fatalf("Argv[0] = %q, want %q", got, want)
	}
	want := []string{fake, "-p", SeatbeltProfile(policy), "--", "/bin/true"}
	if strings.Join(confined.Argv, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("Argv = %q, want %q", confined.Argv, want)
	}
}

// 真机可用性自检：**只对真缺陷失败**。
//
// 先用一段"最小基线"（不经过我们的渲染与参数拼装）确认这台机器的 sandbox-exec
// 本身能跑：跑不动 → 环境/系统限制（skip 并打印原因）；基线能跑而 osrunner.New
// 失败 → 这是本 SDK 的缺陷（fail）。
func TestSeatbeltAvailabilityOnHost(t *testing.T) {
	if _, err := os.Stat(sandboxExecSystemPath); err != nil {
		t.Skipf("本机没有 %s（环境差异，不是缺陷）", sandboxExecSystemPath)
	}
	baseline := exec.Command(sandboxExecSystemPath, "-p", "(version 1) (allow default)", "--", "/usr/bin/true")
	if output, err := baseline.CombinedOutput(); err != nil {
		t.Skipf("这台机器的 sandbox-exec 连最小基线都跑不动（系统限制/环境差异）: %v: %s", err, output)
	}
	runner, err := New(Config{ProbeTimeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("最小基线可用，但 osrunner.New 失败——这是本 SDK 的缺陷: %v", err)
	}
	seatbelt, ok := runner.(*seatbeltRunner)
	if !ok {
		t.Fatalf("runner 类型 = %T", runner)
	}
	t.Logf("Seatbelt 可用：execPath=%s", seatbelt.execPath)
}

// seatbeltRuntimeRunner 返回本机真实可用的 Seatbelt runner；不可用时跳过测试。
func seatbeltRuntimeRunner(t *testing.T) sandbox.Runner {
	t.Helper()
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		t.Skip("sandbox-exec is not installed on this host")
	}
	runner, err := New(Config{ProbeTimeout: 10 * time.Second})
	if err != nil {
		t.Skipf("sandbox-exec is not usable on this host: %v", err)
	}
	return runner
}

// runSeatbeltConfined 用 Confine 的结果真正 spawn 子进程并收集输出。
func runSeatbeltConfined(t *testing.T, runner sandbox.Runner, policy *sandbox.Policy, argv []string) ([]byte, error) {
	t.Helper()
	confined, err := runner.Confine(argv, policy)
	if err != nil {
		t.Fatalf("Confine(%q): %v", argv, err)
	}
	return exec.Command(confined.Argv[0], confined.Argv[1:]...).CombinedOutput()
}

// 真实运行：只读模式允许读，拒绝写。
func TestSeatbeltRunnerRuntimeReadOnly(t *testing.T) {
	runner := seatbeltRuntimeRunner(t)
	policy := sandbox.New(sandbox.WithRoots(t.TempDir()), sandbox.WithMode(sandbox.ModeReadOnly))

	target := filepath.Join(t.TempDir(), "read-only-denied.txt")
	out, err := runSeatbeltConfined(t, runner, policy, []string{"/bin/sh", "-c", `echo hi > "$1"`, "sh", target})
	if err == nil {
		t.Fatalf("read-only write succeeded, output: %s", out)
	}
	if !strings.Contains(strings.ToLower(string(out)), "operation not permitted") {
		t.Fatalf("read-only write output = %q, want a Seatbelt denial", out)
	}
	if _, statErr := os.Stat(target); statErr == nil {
		t.Fatalf("%s exists despite read-only enforcement", target)
	}

	if out, err := runSeatbeltConfined(t, runner, policy, []string{"/bin/ls", "/"}); err != nil {
		t.Fatalf("read-only read failed: %v, output: %s", err, out)
	}
}

// 真实运行：workspace-write 允许写策略根，但拒绝写根之外（HOME）。
func TestSeatbeltRunnerRuntimeWorkspaceWrite(t *testing.T) {
	runner := seatbeltRuntimeRunner(t)
	root := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))

	inside := filepath.Join(root, "inside.txt")
	out, err := runSeatbeltConfined(t, runner, policy, []string{"/bin/sh", "-c", `echo inside > "$1"`, "sh", inside})
	if err != nil {
		t.Fatalf("workspace write failed: %v, output: %s", err, out)
	}
	data, err := os.ReadFile(inside)
	if err != nil {
		t.Fatalf("read workspace file: %v", err)
	}
	if got, want := string(data), "inside\n"; got != want {
		t.Fatalf("workspace file = %q, want %q", got, want)
	}

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home directory to test write denial outside the workspace")
	}
	outside := filepath.Join(home, ".zeokeel-rig-seatbelt-"+strconv.Itoa(os.Getpid()))
	defer os.Remove(outside)
	out, err = runSeatbeltConfined(t, runner, policy, []string{"/bin/sh", "-c", `echo outside > "$1"`, "sh", outside})
	if err == nil {
		t.Fatalf("write outside the workspace succeeded, output: %s", out)
	}
	if !strings.Contains(strings.ToLower(string(out)), "operation not permitted") {
		t.Fatalf("write outside output = %q, want a Seatbelt denial", out)
	}
	if _, statErr := os.Stat(outside); statErr == nil {
		t.Fatalf("%s exists despite workspace-write enforcement", outside)
	}
}
