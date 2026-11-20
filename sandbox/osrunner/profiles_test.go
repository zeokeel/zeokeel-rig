package osrunner

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// 本文件锁定 Profile（profile.go）与四个后端渲染（profiles.go、windows_plan.go）
// 的契约。语义变化见各处注释：read-only 不再“没有任何可写路径”，而是“只有设备
// 节点 + bwrap 私有 tmpfs /tmp”；workspace-write 的可写集是策略根 ∪ 显式追加根
// ∪（按开关过滤的）临时目录，并且保护路径在三个后端里都落在正确位置。

// workspacePolicy 构造一个 workspace-write 策略，根为 root。
func workspacePolicy(t *testing.T, root string) *sandbox.Policy {
	t.Helper()
	return sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
}

// contains 报告 args 是否包含 want。
func contains(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

// indexOf 返回 want 在 args 中的下标，不存在返回 -1。
func indexOf(args []string, want string) int {
	for i, arg := range args {
		if arg == want {
			return i
		}
	}
	return -1
}

func requireLinuxBackend(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("bwrap / Landlock 是 Linux 后端：这些断言验证的是 Linux 渲染")
	}
}

func canonicalPath(t *testing.T, path string) string {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("Abs(%q): %v", path, err)
	}
	abs = filepath.Clean(abs)
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = filepath.Clean(real)
	}
	return abs
}

// customTempDir 返回一个非 /tmp 且已存在的临时目录，用于验证 $TMPDIR 的渲染。
// 它把 TMPDIR 指向该目录，因此 os.TempDir()（非 Windows）就是它。
func customTempDir(t *testing.T) string {
	t.Helper()
	dir := canonicalPath(t, t.TempDir())
	// os.TempDir() 读哪些变量是平台语义：Unix 读 $TMPDIR，Windows 读 %TMP%/%TEMP%
	// （不读 TMPDIR）。两条都设上，这个 helper 就能在三个平台给出同一个"自定义
	// 临时目录"，不必为此跳过 Windows。
	t.Setenv("TMPDIR", dir)
	t.Setenv("TMP", dir)
	t.Setenv("TEMP", dir)
	return dir
}

// ---------------------------------------------------------------------------
// BuildProfile
// ---------------------------------------------------------------------------

func TestBuildProfileDangerFullAccessIsNil(t *testing.T) {
	profile, err := BuildProfile(sandbox.New(sandbox.WithMode(sandbox.ModeDangerFullAccess)))
	if err != nil {
		t.Fatalf("BuildProfile(danger-full-access) error = %v, want nil", err)
	}
	if profile != nil {
		t.Fatalf("BuildProfile(danger-full-access) = %+v, want nil profile (passthrough)", profile)
	}
}

func TestBuildProfileNilPolicyFailsClosed(t *testing.T) {
	if _, err := BuildProfile(nil); err == nil {
		t.Fatal("BuildProfile(nil) succeeded, want fail closed")
	} else if !errors.Is(err, sandbox.ErrSandboxUnavailable) {
		t.Fatalf("BuildProfile(nil) error = %v, want ErrSandboxUnavailable", err)
	}
}

// 新契约（旧测试断言 WritableRoots(read-only) 为空）：
// read-only 仍然不授予 workspace、/tmp、$TMPDIR，但不再是“没有任何可写路径”——
// ReadWrite 只剩设备节点（/dev/null 必备）。私有 tmpfs /tmp 是 bwrap 的运行时
// 机制，不是宿主路径，因此不会出现在 ReadWrite 里。
func TestBuildProfileReadOnlyGrantsOnlyDevices(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX 设备节点（/dev/*）在 Windows 上不存在；这些断言验证的是 POSIX 渲染")
	}
	root := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeReadOnly))
	profile, err := BuildProfile(policy)
	if err != nil {
		t.Fatalf("BuildProfile: %v", err)
	}
	if profile.Mode != sandbox.ModeReadOnly {
		t.Fatalf("Mode = %q, want read-only", profile.Mode)
	}
	if len(profile.ReadOnly) != 1 || profile.ReadOnly[0] != "/" {
		t.Fatalf("ReadOnly = %v, want [/]", profile.ReadOnly)
	}
	if !contains(profile.ReadWrite, "/dev/null") {
		t.Fatalf("ReadWrite = %v, want /dev/null", profile.ReadWrite)
	}
	workspace := canonicalPath(t, root)
	for _, path := range profile.ReadWrite {
		if !strings.HasPrefix(path, "/dev/") {
			t.Fatalf("ReadWrite = %v, contains non-device path %q", profile.ReadWrite, path)
		}
		if path == workspace {
			t.Fatalf("ReadWrite = %v, must not grant the workspace", profile.ReadWrite)
		}
	}
	if profile.PrivateTmp {
		t.Fatal("PrivateTmp = true, want false for read-only (DSH 不在只读模式挂私有 /tmp)")
	}
}

func TestBuildProfileWorkspaceWritableSet(t *testing.T) {
	tempDir := customTempDir(t)
	root := t.TempDir()
	extra := canonicalPath(t, t.TempDir())
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithWritableRoots(extra),
	)
	profile, err := BuildProfile(policy)
	if err != nil {
		t.Fatalf("BuildProfile: %v", err)
	}
	if profile.Mode != sandbox.ModeWorkspaceWrite {
		t.Fatalf("Mode = %q, want workspace-write", profile.Mode)
	}
	want := []string{canonicalPath(t, root), extra, tempDir}
	if runtime.GOOS != "windows" {
		// 字面 /tmp 与 POSIX 设备节点只在 POSIX 上存在；Windows 的临时目录就是
		// 上面那个 tempDir，它已经在外层断言里了。
		// 用 canonicalPath 取形态：macOS 的 /tmp 是 /private/tmp 的符号链接，
		// profile 里出现的是解析后的真实路径。
		want = append(want, canonicalPath(t, "/tmp"), canonicalPath(t, "/dev/null"))
	}
	for _, entry := range want {
		if !contains(profile.ReadWrite, entry) {
			t.Fatalf("ReadWrite = %v, want it to contain %q", profile.ReadWrite, entry)
		}
	}
	if profile.PrivateTmp {
		t.Fatal("PrivateTmp = true, want false (私有 /tmp 由 BwrapArgs 的 --tmpfs 渲染，不是 Profile 的开关)")
	}
	if profile.Network {
		t.Fatal("Network = true, want false by default")
	}
	if len(profile.Dropped) != 0 {
		t.Fatalf("Dropped = %v, want empty", profile.Dropped)
	}
}

// BuildProfile 不再静默丢弃宿主显式配置的可写根：被丢弃的项从 ReadWrite 里排除，
// 同时出现在 Profile.Dropped 里，让调用方能报警。
func TestBuildProfileReportsDroppedWritableRoots(t *testing.T) {
	root := t.TempDir()
	gone := filepath.Join(t.TempDir(), "gone")
	if err := os.MkdirAll(gone, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// 策略构造期会解析符号链接/短名，所以期望值也要用规范形式（Windows CI 的
	// Temp 是 8.3 短名）。
	goneCanonical := canonicalForTest(gone)
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithWritableRoots(gone),
	)
	// 策略构造之后再删除：模拟“配置了但运行期不存在”。
	if err := os.RemoveAll(gone); err != nil {
		t.Fatalf("remove: %v", err)
	}
	profile, err := BuildProfile(policy)
	if err != nil {
		t.Fatalf("BuildProfile: %v", err)
	}
	if contains(profile.ReadWrite, goneCanonical) {
		t.Fatalf("ReadWrite = %v, contains the removed root %q", profile.ReadWrite, goneCanonical)
	}
	if !contains(profile.Dropped, goneCanonical) {
		t.Fatalf("Dropped = %v, want it to report %q", profile.Dropped, goneCanonical)
	}
}

func TestBwrapArgsDangerFullAccessRendersNothing(t *testing.T) {
	requireLinuxBackend(t)
	if args := BwrapArgs(sandbox.New(sandbox.WithMode(sandbox.ModeDangerFullAccess))); args != nil {
		t.Fatalf("BwrapArgs(danger) = %v, want nil", args)
	}
}

// ---------------------------------------------------------------------------
// Landlock
// ---------------------------------------------------------------------------

func TestLandlockGrantsReadOnlyIsDevicesOnly(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bwrap / Landlock 是 Linux 后端")
	}
	root := t.TempDir()
	readOnly, readWrite := LandlockGrants(sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeReadOnly)))
	if len(readOnly) != 1 || readOnly[0] != "/" {
		t.Fatalf("read-only grants = %v, want [/]", readOnly)
	}
	if !contains(readWrite, "/dev/null") {
		t.Fatalf("read-write grants = %v, want /dev/null", readWrite)
	}
	workspace := canonicalPath(t, root)
	for _, path := range readWrite {
		if !strings.HasPrefix(path, "/dev/") {
			t.Fatalf("read-write grants = %v, contains non-device %q", readWrite, path)
		}
		// 用规范化后的 /tmp 比较：macOS 上是 /private/tmp，字面比较会漏。
		if path == workspace || sandbox.PathEqual(path, canonicalPath(t, "/tmp")) {
			t.Fatalf("read-write grants = %v, must not grant %q", readWrite, path)
		}
	}
}

func TestSeatbeltProfileDangerFullAccessIsEmpty(t *testing.T) {
	if profile := SeatbeltProfile(sandbox.New(sandbox.WithMode(sandbox.ModeDangerFullAccess))); profile != "" {
		t.Fatalf("SeatbeltProfile(danger) = %q, want empty", profile)
	}
}

func TestSBPLStringEscapes(t *testing.T) {
	if got, want := sbplString(`/a"b\c`), `"/a\"b\\c"`; got != want {
		t.Fatalf("sbplString = %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// 四后端 × 三模式 的渲染矩阵
// ---------------------------------------------------------------------------

func TestDenialSignatures(t *testing.T) {
	for _, runner := range []string{RunnerBwrap, RunnerLandlock, RunnerSeatbelt, RunnerWindowsACL} {
		if signatures := DenialSignatures(runner); len(signatures) == 0 {
			t.Fatalf("DenialSignatures(%q) is empty", runner)
		}
	}
	if signatures := DenialSignatures("unknown"); signatures != nil {
		t.Fatalf("DenialSignatures(unknown) = %v, want nil", signatures)
	}
}

func TestCustomRunnerWrapsWithBwrapProfile(t *testing.T) {
	requireLinuxBackend(t)
	runner, err := New(Config{RunnerCommand: []string{"myns"}, RunnerFailureSignatures: []string{"myns: failed"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	confined, err := runner.Confine([]string{"/bin/sh", "-c", "echo hi"}, workspacePolicy(t, t.TempDir()))
	if err != nil {
		t.Fatalf("Confine: %v", err)
	}
	if got, want := confined.Argv[0], "myns"; got != want {
		t.Fatalf("Argv[0] = %q, want %q", got, want)
	}
	if !contains(confined.Argv, "--ro-bind") {
		t.Fatalf("Argv = %v, want bwrap profile args", confined.Argv)
	}
	if sep := indexOf(confined.Argv, "--"); sep < 0 || sep+1 >= len(confined.Argv) || confined.Argv[sep+1] != "/bin/sh" {
		t.Fatalf("Argv = %v, want -- /bin/sh ...", confined.Argv)
	}
	if confined.Enforcement != sandbox.EnforcementFull {
		t.Fatalf("Enforcement = %q, want full", confined.Enforcement)
	}
	// bwrap 的拒绝方言现在包含 seccomp 的 EPERM（connect 被拒）与守护进程 socket
	// 客户端的 "permission denied"；第一条仍是文件系统的只读报错。
	if len(confined.DenialSignatures) != 3 || confined.DenialSignatures[0] != "read-only file system" {
		t.Fatalf("DenialSignatures = %v", confined.DenialSignatures)
	}
}

func TestCustomRunnerDangerFullAccessPassesThrough(t *testing.T) {
	runner, err := New(Config{RunnerCommand: []string{"myns"}, RunnerFailureSignatures: []string{"myns: failed"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	policy := sandbox.New(sandbox.WithMode(sandbox.ModeDangerFullAccess))
	confined, err := runner.Confine([]string{"/bin/sh", "-c", "echo hi"}, policy)
	if err != nil {
		t.Fatalf("Confine: %v", err)
	}
	if got, want := strings.Join(confined.Argv, " "), "/bin/sh -c echo hi"; got != want {
		t.Fatalf("Argv = %q, want %q", got, want)
	}
	// 透传没有被任何 runner 包装。
	if confined.Enforcement != sandbox.EnforcementNone {
		t.Fatalf("Enforcement = %q, want %q", confined.Enforcement, sandbox.EnforcementNone)
	}
}

func TestCustomRunnerRequiresSignatures(t *testing.T) {
	if _, err := New(Config{RunnerCommand: []string{"myns"}}); err == nil {
		t.Fatal("New accepted RunnerCommand without RunnerFailureSignatures")
	}
	if _, err := New(Config{RunnerCommand: []string{"myns"}, RunnerFailureSignatures: []string{"bad\nline"}}); err == nil {
		t.Fatal("New accepted a multi-line failure signature")
	}
}

func TestCustomRunnerNilPolicyFailsClosed(t *testing.T) {
	runner, err := New(Config{RunnerCommand: []string{"myns"}, RunnerFailureSignatures: []string{"myns: failed"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := runner.Confine([]string{"/bin/sh"}, nil); err == nil {
		t.Fatal("Confine(nil policy) succeeded, want fail closed")
	} else if !errors.Is(err, sandbox.ErrSandboxUnavailable) {
		t.Fatalf("Confine(nil policy) error = %v, want ErrSandboxUnavailable", err)
	}
}

func TestCanonicalSplitDropsMissingAndDuplicates(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	kept, dropped := canonicalSplit([]string{dir, dir, missing})
	if len(kept) != 1 || kept[0] != canonicalPath(t, dir) {
		t.Fatalf("canonicalSplit kept = %v, want one existing path", kept)
	}
	if len(dropped) != 1 || dropped[0] != missing {
		t.Fatalf("canonicalSplit dropped = %v, want [%q]", dropped, missing)
	}
	if got := canonicalUnique([]string{dir, dir, missing}); len(got) != 1 {
		t.Fatalf("canonicalUnique = %v, want one existing path", got)
	}
}

func TestPlatformTempRootsIncludeOSTemp(t *testing.T) {
	roots := platformTempRoots()
	if len(roots) == 0 {
		t.Fatal("platformTempRoots is empty")
	}
	if runtime.GOOS == "windows" {
		return // Windows 只有 os.TempDir()，没有 POSIX 的 /tmp
	}
	// 断言"包含 os.TempDir() 的真实路径"，不硬编码 `/tmp`：macOS 上 /tmp 是
	// /private/tmp 的符号链接，`t.TempDir()` 也可能给出 /var 而非 /private/var。
	want := canonicalForTest(os.TempDir())
	found := false
	for _, root := range roots {
		if samePathForTest(root, want) || sandbox.PathEqual(root, want) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("platformTempRoots = %v, want it to contain os.TempDir() = %q", roots, want)
	}
}

// TestBuildProfileIncludesGrantedWritableRoots 证明运行期授权立即进入 profile：
// 被约束的 agent 申请一个尚不存在的目录后，BuildProfile 必须把它列为可写，否则
// "批准了却仍然写不进"——这正是审计中发现的竞态。
func TestBuildProfileIncludesGrantedWritableRoots(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "ws")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))

	grantPath := filepath.Join(base, "tool-creds")
	if _, err := policy.AddWritableRoot(grantPath, "the tool keeps credentials here"); err != nil {
		t.Fatalf("AddWritableRoot: %v", err)
	}
	profile, err := BuildProfile(policy)
	if err != nil {
		t.Fatalf("BuildProfile: %v", err)
	}
	found := false
	for _, path := range profile.ReadWrite {
		if real, realErr := filepath.EvalSymlinks(grantPath); realErr == nil && path == real {
			found = true
		}
		if path == grantPath {
			found = true
		}
	}
	if !found {
		t.Fatalf("granted root %q missing from ReadWrite %v", grantPath, profile.ReadWrite)
	}
	if _, err := os.Stat(grantPath); err != nil {
		t.Fatalf("granted directory was not created: %v", err)
	}
}

func TestSeatbeltProfileReadOnlyStillGrantsNoWorkspace(t *testing.T) {
	root := t.TempDir()
	profile := SeatbeltProfile(sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeReadOnly)))
	if strings.Contains(profile, `(subpath "`+root+`")`) {
		t.Fatalf("read-only profile granted the workspace:\n%s", profile)
	}
}

// TestUntrustedExecutableReason 覆盖"沙箱本体可以被替换"的三种情况：可写二进制、
// 世界可写目录、以及落在 agent 工作区里的候选。
func TestUntrustedExecutableReason(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bwrap")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		// Windows 没有 POSIX 的 group/world 位（Go 只映射只读位），"0755 即可信"
		// 这条前提不成立；Windows 上真正有意义的是下面的 cwd 检查。
		if reason := untrustedExecutableReason(bin); reason != "" {
			t.Fatalf("reason = %q, want trusted for a 0755 file in a 0700 dir", reason)
		}
	}

	// Windows 的权限模型没有 POSIX 的 group/world 位（Go 在 Windows 上只映射
	// 只读位），这两条断言只在 POSIX 上有意义。
	if runtime.GOOS != "windows" {
		if err := os.Chmod(bin, 0o777); err != nil {
			t.Fatal(err)
		}
		if reason := untrustedExecutableReason(bin); !strings.Contains(reason, "writable by group or others") {
			t.Fatalf("reason = %q, want the world-writable binary diagnostic", reason)
		}
		if err := os.Chmod(bin, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o777); err != nil {
			t.Fatal(err)
		}
		if reason := untrustedExecutableReason(bin); !strings.Contains(reason, "world-writable") {
			t.Fatalf("reason = %q, want the world-writable directory diagnostic", reason)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	// 落在当前工作目录里的候选一律不信任：那是 agent 可写的地方。
	t.Chdir(dir)
	if reason := untrustedExecutableReason(bin); !strings.Contains(reason, "current working directory") {
		t.Fatalf("reason = %q, want the cwd diagnostic", reason)
	}
}
