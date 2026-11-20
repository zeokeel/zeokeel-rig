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
	if !strings.Contains(profile, `(subpath "`+scratch+`")`) {
		t.Fatalf("SeatbeltProfile = %q, want a subpath rule for %q", profile, scratch)
	}
	for _, temp := range platformTempRoots() {
		if strings.Contains(profile, `(subpath "`+temp+`")`) {
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
		sandbox.WithSlashTmp(false),
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
