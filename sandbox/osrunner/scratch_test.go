package osrunner

// 私有 scratch 目录在 profile 渲染里的位置（A4）。
//
// read-only 档以前的可写面只有 /dev/null（bwrap 下是 `--dev` 提供的设备节点）。
// 策略带 scratch 时它必须真的被渲染成**目录**可写：bwrap 用 `--bind`、Landlock
// 用 `--rw`、Seatbelt 用 `(subpath ...)`，而 /dev/null 仍然是文件绑定/literal。

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// readOnlyScratchPolicy 构造带 scratch 的 read-only 策略，返回策略与 scratch 的
// 规范写法（各平台 temp 目录可能是符号链接/短名）。
func readOnlyScratchPolicy(t *testing.T) (*sandbox.Policy, string) {
	t.Helper()
	scratch := t.TempDir()
	if err := os.Remove(scratch); err != nil {
		t.Fatalf("remove temp dir: %v", err)
	}
	scratch = filepath.Join(scratch, "scratch")
	policy := sandbox.New(
		sandbox.WithRoots(t.TempDir()),
		sandbox.WithMode(sandbox.ModeReadOnly),
		sandbox.WithScratchDir(scratch),
	)
	return policy, canonicalForTest(scratch)
}

// TestBuildProfileReadOnlyWithScratchGrantsScratch 钉住 read-only 的可写面变成
// {/dev/null, scratch}，且工作区仍然不可写。
func TestBuildProfileReadOnlyWithScratchGrantsScratch(t *testing.T) {
	policy, scratch := readOnlyScratchPolicy(t)
	profile, err := BuildProfile(policy)
	if err != nil {
		t.Fatalf("BuildProfile: %v", err)
	}
	if profile.Mode != sandbox.ModeReadOnly {
		t.Fatalf("Mode = %q, want read-only", profile.Mode)
	}
	if profile.Scratch != scratch {
		t.Fatalf("Profile.Scratch = %q, want %q", profile.Scratch, scratch)
	}
	if !contains(profile.ReadWrite, "/dev/null") {
		t.Fatalf("ReadWrite = %v, want /dev/null", profile.ReadWrite)
	}
	if !contains(profile.ReadWrite, scratch) {
		t.Fatalf("ReadWrite = %v, want it to contain the scratch dir %q", profile.ReadWrite, scratch)
	}
	if got := WritableRoots(policy); !contains(got, scratch) {
		t.Fatalf("WritableRoots = %v, want it to contain %q", got, scratch)
	}
	for _, root := range policy.Roots() {
		if contains(profile.ReadWrite, canonicalForTest(root)) {
			t.Fatalf("ReadWrite = %v, must not grant the workspace", profile.ReadWrite)
		}
	}
}

// TestBuildProfileWorkspaceWriteIncludesScratch 钉住 workspace-write 也带 scratch。
func TestBuildProfileWorkspaceWriteIncludesScratch(t *testing.T) {
	scratch := filepath.Join(t.TempDir(), "scratch")
	policy := sandbox.New(
		sandbox.WithRoots(t.TempDir()),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithScratchDir(scratch),
	)
	profile, err := BuildProfile(policy)
	if err != nil {
		t.Fatalf("BuildProfile: %v", err)
	}
	want := canonicalForTest(scratch)
	if !contains(profile.ReadWrite, want) {
		t.Fatalf("ReadWrite = %v, want it to contain %q", profile.ReadWrite, want)
	}
}

// TestBwrapArgsReadOnlyWithScratchBindsScratch 钉住 Linux 分支：read-only 下
// /dev/null 由 `--dev` 提供（不 bind），scratch 必须是一条目录 `--bind`。
func TestBwrapArgsReadOnlyWithScratchBindsScratch(t *testing.T) {
	requireLinuxBackend(t)
	policy, scratch := readOnlyScratchPolicy(t)
	args := BwrapArgs(policy)

	found := false
	for index, arg := range args {
		if arg == "--bind" && index+2 < len(args) && args[index+1] == scratch && args[index+2] == scratch {
			found = true
		}
	}
	if !found {
		t.Fatalf("BwrapArgs(read-only+scratch) = %q, want --bind %q %q", args, scratch, scratch)
	}
	if contains(args, "/dev/null") {
		t.Fatalf("BwrapArgs(read-only+scratch) = %q, /dev/null 应由 --dev 提供，不是 bind", args)
	}
	if contains(args, "--tmpfs") {
		t.Fatalf("BwrapArgs(read-only+scratch) = %q, read-only 不得挂私有 tmpfs", args)
	}
}

// TestLandlockGrantsReadOnlyWithScratch 钉住 Landlock：read-only 的 --rw 集合
// 是 {/dev/null, scratch}。
func TestLandlockGrantsReadOnlyWithScratch(t *testing.T) {
	requireLinuxBackend(t)
	policy, scratch := readOnlyScratchPolicy(t)
	readOnly, readWrite := LandlockGrants(policy)
	if len(readOnly) != 1 || readOnly[0] != "/" {
		t.Fatalf("read-only grants = %v, want [/]", readOnly)
	}
	if !contains(readWrite, "/dev/null") {
		t.Fatalf("read-write grants = %v, want /dev/null", readWrite)
	}
	if !contains(readWrite, scratch) {
		t.Fatalf("read-write grants = %v, want %q", readWrite, scratch)
	}
}

// TestSeatbeltProfileReadOnlyWithScratchAllowsScratchSubpath 钉住 Seatbelt：
// read-only 下 scratch 是一条 subpath 放行规则，且**不**放行任何临时目录。
func TestSeatbeltProfileReadOnlyWithScratchAllowsScratchSubpath(t *testing.T) {
	policy, scratch := readOnlyScratchPolicy(t)
	profile := SeatbeltProfile(policy)
	// SBPL 字符串字面量会转义 `\` 与 `"`：Windows 路径里全是反斜杠，所以断言必须
	// 用同一套转义，否则这条用例在 Windows 上会因为"路径被转义"而假红。
	rule := func(path string) string {
		escaped := strings.ReplaceAll(path, `\`, `\\`)
		escaped = strings.ReplaceAll(escaped, `"`, `\"`)
		return `(subpath "` + escaped + `")`
	}
	if !strings.Contains(profile, rule(scratch)) {
		t.Fatalf("SeatbeltProfile = %q, want a subpath rule for %q", profile, scratch)
	}
	for _, temp := range platformTempRoots() {
		if strings.Contains(profile, rule(temp)) {
			t.Fatalf("SeatbeltProfile = %q, read-only 不得放行临时目录 %q", profile, temp)
		}
	}
	if !strings.Contains(profile, `(literal "/dev/null")`) {
		t.Fatalf("SeatbeltProfile = %q, want the /dev/null literal rule", profile)
	}
}

// TestBwrapArgsHonorsSlashTmpSwitch 钉住死旋钮收口在渲染层也生效：
// workspace-write + WithSlashTmp(false)/WithTmpdirEnv(false) 不再挂私有 /tmp。
func TestBwrapArgsHonorsSlashTmpSwitch(t *testing.T) {
	requireLinuxBackend(t)
	if runtime.GOOS != "linux" {
		t.Skip("bwrap 是 Linux 后端")
	}
	workspace := t.TempDir()
	strict := sandbox.New(
		sandbox.WithRoots(workspace),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		// 用例故意用两个已弃用的旧开关：它们仍受支持，并且正是这条用例要钉住的
		// "关掉私有 /tmp"路径。改成 WithScratchDir 就测不到被测行为了。
		//lint:ignore SA1019 这里就是要覆盖旧开关 WithSlashTmp(false) 的渲染路径
		sandbox.WithSlashTmp(false),
		//lint:ignore SA1019 同上：WithTmpdirEnv(false) 的渲染路径必须继续被钉住
		sandbox.WithTmpdirEnv(false),
	)
	args := BwrapArgs(strict)
	if contains(args, "--tmpfs") {
		t.Fatalf("BwrapArgs = %q, WithSlashTmp(false) 之后不得再挂 --tmpfs /tmp", args)
	}
	if !contains(args, canonicalForTest(workspace)) {
		t.Fatalf("BwrapArgs = %q, want the workspace bind %q", args, canonicalForTest(workspace))
	}
}

// TestWindowsReadOnlyScratchIsActuallyGranted 钉住 Windows 的 read-only + scratch。
//
// 审计发现（A4）：read-only 下 helper 收到的 `--workspace` 只做**校验**、不做写
// 授权，而策略的 scratch 正是从这条路下发的——于是 Profile/Preflight/
// sandbox_status 都宣称"scratch 可写"，helper 侧却一条写权限都没给，模型在
// scratch 里创建临时文件会被 ACL 拒绝。
//
// 修法：read-only 且有 scratch 时，把 scratch 作为**唯一的工作区**下发，并让
// helper 以 workspace-write 形状运行——即"除 scratch 之外一切只读"，与 Linux
// （`--ro-bind / / --rw <scratch>`）和 macOS（只加 scratch 的 subpath）同义。
func TestWindowsReadOnlyScratchIsActuallyGranted(t *testing.T) {
	scratch := filepath.Join(t.TempDir(), "scratch")
	policy := sandbox.New(
		sandbox.WithRoots(t.TempDir()),
		sandbox.WithMode(sandbox.ModeReadOnly),
		sandbox.WithScratchDir(scratch),
	)
	confined, err := buildWindowsConfinedCommand("rig.exe", []string{"cmd.exe", "/c", "echo hi"}, policy)
	if err != nil {
		t.Fatalf("buildWindowsConfinedCommand: %v", err)
	}
	workspaces := windowsFlagValues(confined.Argv, winFlagWorkspace)
	if len(workspaces) != 1 {
		t.Fatalf("--workspace = %v, want exactly the scratch dir（工作区不得被授权）", workspaces)
	}
	if !samePathForTest(workspaces[0], scratch) && !strings.EqualFold(filepath.Clean(workspaces[0]), filepath.Clean(scratch)) {
		t.Fatalf("--workspace = %q, want the scratch dir %q", workspaces[0], scratch)
	}
	if got := windowsFlagValues(confined.Argv, winFlagMode); len(got) != 1 || got[0] != winModeWorkspaceWrite {
		t.Fatalf("--mode = %v, want [%s]（read-only 的 --workspace 只是校验，授权必须走工作区写形状）", got, winModeWorkspaceWrite)
	}
}

// TestWindowsReadOnlyWithoutScratchStaysReadOnly 反向守护：没有 scratch 时不能因为
// 上面那条修法把 read-only 悄悄升级成 workspace-write。
func TestWindowsReadOnlyWithoutScratchStaysReadOnly(t *testing.T) {
	root := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeReadOnly))
	confined, err := buildWindowsConfinedCommand("rig.exe", []string{"cmd.exe", "/c", "echo hi"}, policy)
	if err != nil {
		t.Fatalf("buildWindowsConfinedCommand: %v", err)
	}
	if got := windowsFlagValues(confined.Argv, winFlagMode); len(got) != 1 || got[0] != winModeReadOnly {
		t.Fatalf("--mode = %v, want [%s]", got, winModeReadOnly)
	}
}
