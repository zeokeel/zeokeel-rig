package osrunner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// 本文件是 darwin 后端里“不依赖 macOS 二进制”的纯逻辑测试：Seatbelt profile 与
// argv 构造都必须在 Linux 上可测，功能探测则用假 sandbox-exec 脚本模拟。真正的
// sandbox-exec 行为测试在 darwin_test.go（//go:build darwin）。

// testSeatbeltExec 是纯 argv 测试使用的假 sandbox-exec 路径（不会真的执行）。
const testSeatbeltExec = "/usr/bin/sandbox-exec"

// seatbeltTestPolicy 构造指定模式的策略；workspace-write 的根是临时目录。
func seatbeltTestPolicy(t *testing.T, mode sandbox.Mode) *sandbox.Policy {
	t.Helper()
	if mode == sandbox.ModeWorkspaceWrite {
		return sandbox.New(sandbox.WithRoots(t.TempDir()), sandbox.WithMode(mode))
	}
	return sandbox.New(sandbox.WithMode(mode))
}

// 只读模式：argv 必须是 sandbox-exec -p <profile> -- <原 argv>，且 profile 拒绝写。
func TestSeatbeltRunnerWrapsReadOnlyArgv(t *testing.T) {
	policy := seatbeltTestPolicy(t, sandbox.ModeReadOnly)
	runner := &seatbeltRunner{execPath: testSeatbeltExec}
	argv := []string{"/bin/sh", "-c", "echo hi > /tmp/zeokeel-seatbelt-probe"}

	confined, err := runner.Confine(argv, policy)
	if err != nil {
		t.Fatalf("Confine: %v", err)
	}

	profile := SeatbeltProfile(policy)
	want := []string{testSeatbeltExec, "-p", profile, "--"}
	want = append(want, argv...)
	if !slices.Equal(confined.Argv, want) {
		t.Fatalf("Argv = %q, want %q", confined.Argv, want)
	}
	if confined.Enforcement != sandbox.EnforcementFull {
		t.Fatalf("Enforcement = %q, want %q", confined.Enforcement, sandbox.EnforcementFull)
	}
	if !slices.Equal(confined.DenialSignatures, DenialSignatures(RunnerSeatbelt)) {
		t.Fatalf("DenialSignatures = %q, want %q", confined.DenialSignatures, DenialSignatures(RunnerSeatbelt))
	}
	for _, form := range []string{"(version 1)", "(allow default)", "(deny file-write*)"} {
		if !strings.Contains(profile, form) {
			t.Fatalf("SeatbeltProfile = %q, missing %q", profile, form)
		}
	}
	if strings.Contains(profile, "(subpath ") {
		t.Fatalf("SeatbeltProfile(read-only) = %q, must not grant a writable subpath", profile)
	}
}

// workspace-write 模式：profile 必须放行策略根的 subpath，且 argv 分隔符正确。
func TestSeatbeltRunnerWorkspaceWriteGrantsRoot(t *testing.T) {
	root := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	runner := &seatbeltRunner{execPath: testSeatbeltExec}
	// 故意使用看起来像 sandbox-exec 自己参数的 argv，验证它们在 "--" 之后按原样传递。
	argv := []string{"sh", "-p", "-c", "echo -- -p > inside.txt"}

	confined, err := runner.Confine(argv, policy)
	if err != nil {
		t.Fatalf("Confine: %v", err)
	}

	profile := SeatbeltProfile(policy)
	wantRoot := root
	if resolved, resolveErr := filepath.EvalSymlinks(root); resolveErr == nil {
		wantRoot = resolved
	}
	if !strings.Contains(profile, "(subpath "+sbplString(wantRoot)+")") {
		t.Fatalf("SeatbeltProfile = %q, want subpath %q", profile, wantRoot)
	}

	want := []string{testSeatbeltExec, "-p", profile, "--"}
	want = append(want, argv...)
	if !slices.Equal(confined.Argv, want) {
		t.Fatalf("Argv = %q, want %q", confined.Argv, want)
	}
}

// nil 策略没有可强制的效果集：必须 fail closed，不能返回未受限 argv。
func TestSeatbeltRunnerNilPolicyFailsClosed(t *testing.T) {
	runner := &seatbeltRunner{execPath: testSeatbeltExec}
	confined, err := runner.Confine([]string{"/bin/true"}, nil)
	if err == nil {
		t.Fatal("Confine(nil policy) succeeded, want fail closed")
	}
	if !errors.Is(err, sandbox.ErrSandboxUnavailable) {
		t.Fatalf("Confine(nil policy) error = %v, want ErrSandboxUnavailable", err)
	}
	if len(confined.Argv) != 0 {
		t.Fatalf("Confine(nil policy) Argv = %q, want empty", confined.Argv)
	}
}

// 未配置（nil receiver 或空 execPath）的 runner 必须 fail closed。
func TestSeatbeltRunnerUnconfiguredFailsClosed(t *testing.T) {
	policy := seatbeltTestPolicy(t, sandbox.ModeReadOnly)
	var nilRunner *seatbeltRunner
	cases := map[string]*seatbeltRunner{
		"nil receiver": nilRunner,
		"empty path":   {},
	}
	for name, runner := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := runner.Confine([]string{"/bin/true"}, policy); err == nil {
				t.Fatal("Confine succeeded on an unconfigured runner, want fail closed")
			} else if !errors.Is(err, sandbox.ErrSandboxUnavailable) {
				t.Fatalf("Confine error = %v, want ErrSandboxUnavailable", err)
			}
		})
	}
}

// 空 argv 无法 spawn，必须 fail closed 而不是返回只有 runner 的 argv。
func TestSeatbeltRunnerEmptyArgvFailsClosed(t *testing.T) {
	runner := &seatbeltRunner{execPath: testSeatbeltExec}
	if _, err := runner.Confine(nil, seatbeltTestPolicy(t, sandbox.ModeReadOnly)); err == nil {
		t.Fatal("Confine(empty argv) succeeded, want fail closed")
	} else if !errors.Is(err, sandbox.ErrSandboxUnavailable) {
		t.Fatalf("Confine(empty argv) error = %v, want ErrSandboxUnavailable", err)
	}
}

// danger-full-access 原样放行：不得多包一层 sandbox-exec，且返回拷贝。
func TestSeatbeltRunnerDangerFullAccessPassesThrough(t *testing.T) {
	policy := sandbox.New(sandbox.WithMode(sandbox.ModeDangerFullAccess))
	runner := &seatbeltRunner{execPath: testSeatbeltExec}
	argv := []string{"/bin/sh", "-c", "echo hi"}

	confined, err := runner.Confine(argv, policy)
	if err != nil {
		t.Fatalf("Confine: %v", err)
	}
	if !slices.Equal(confined.Argv, argv) {
		t.Fatalf("Argv = %q, want passthrough %q", confined.Argv, argv)
	}
	if confined.Enforcement != sandbox.EnforcementFull {
		t.Fatalf("Enforcement = %q, want %q", confined.Enforcement, sandbox.EnforcementFull)
	}
	if len(confined.DenialSignatures) != 0 {
		t.Fatalf("DenialSignatures = %q, want none for passthrough", confined.DenialSignatures)
	}
	confined.Argv[0] = "mutated"
	if argv[0] != "/bin/sh" {
		t.Fatal("Confine result aliases the caller argv")
	}
}

// writeSeatbeltFakeExec 写入一个可执行的假 sandbox-exec shell 脚本。
func writeSeatbeltFakeExec(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("假 sandbox-exec 需要 POSIX shell")
	}
	path := filepath.Join(t.TempDir(), "sandbox-exec")
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake sandbox-exec: %v", err)
	}
	return path
}

// 功能探测必须传入只读 profile、"--" 与 "true"。
func TestProbeSeatbeltPassesReadOnlyProfileAndCommand(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "probe-args")
	fake := writeSeatbeltFakeExec(t, `printf '%s\n' "$@" > "$SEATBELT_PROBE_MARKER"`)
	t.Setenv("SEATBELT_PROBE_MARKER", marker)

	if err := probeSeatbelt(context.Background(), fake); err != nil {
		t.Fatalf("probeSeatbelt: %v", err)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read probe args: %v", err)
	}
	got := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	want := []string{"-p", SeatbeltProfile(sandbox.New(sandbox.WithMode(sandbox.ModeReadOnly))), "--", "true"}
	if !slices.Equal(got, want) {
		t.Fatalf("probe args = %q, want %q", got, want)
	}
}

// 探测进程非零退出：探测失败并带上 stderr 便于诊断。
func TestProbeSeatbeltFailureIncludesStderr(t *testing.T) {
	fake := writeSeatbeltFakeExec(t, `echo "seatbelt probe boom" >&2; exit 7`)
	err := probeSeatbelt(context.Background(), fake)
	if err == nil {
		t.Fatal("probeSeatbelt succeeded despite a failing sandbox-exec")
	}
	if !strings.Contains(err.Error(), "seatbelt probe boom") {
		t.Fatalf("probeSeatbelt error = %v, want it to include stderr", err)
	}
}

// 探测超时：必须返回包装 context.DeadlineExceeded 的错误（newPlatformRunner 再
// 把它包装成 ErrSandboxUnavailable）。
func TestProbeSeatbeltTimeout(t *testing.T) {
	fake := writeSeatbeltFakeExec(t, "exec sleep 5")
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	err := probeSeatbelt(ctx, fake)
	if err == nil {
		t.Fatal("probeSeatbelt succeeded despite a hanging sandbox-exec")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("probeSeatbelt error = %v, want context.DeadlineExceeded", err)
	}
}

// 空 execPath 的探测也必须 fail closed。
func TestProbeSeatbeltEmptyPathFailsClosed(t *testing.T) {
	if err := probeSeatbelt(context.Background(), ""); err == nil {
		t.Fatal("probeSeatbelt accepted an empty sandbox-exec path")
	} else if !errors.Is(err, sandbox.ErrSandboxUnavailable) {
		t.Fatalf("probeSeatbelt error = %v, want ErrSandboxUnavailable", err)
	}
}
