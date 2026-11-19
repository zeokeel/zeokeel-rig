package osrunner

// Linux 探测层与 Reporter 面的单元测试。
//
// 这一层在 CI 容器里经常"跑不到"（没有 bwrap、Landlock 被 seccomp/LSM 挡住），所以
// 覆盖不能依赖"真的有可用后端"。这里只测**纯函数与失败分支**：分类逻辑、参数解析、
// 探测失败的诊断文案、以及 Reporter 的自述字符串。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// TestClassifyBwrapFailure 覆盖 bwrap 探测失败的分类（纯函数）。
func TestClassifyBwrapFailureTable(t *testing.T) {
	cases := []struct {
		name, output string
		contains     string
	}{
		{"user-ns-disabled", "bwrap: No permissions to creating new namespace, likely because the kernel does not allow non-privileged user namespaces", "cannot create a user namespace"},
		{"uid-map", "bwrap: setting up uid map: Permission denied", "cannot create a user namespace"},
		{"eperm", "bwrap: loopback: Failed RTM_NEWADDR: Operation not permitted", "cannot create a user namespace"},
		{"missing-path", "bwrap: Can't find source path /nope: No such file or directory", "rejected the read-only probe profile"},
		{"multiline", "bwrap: some other failure\nsecond line that must be dropped", "bwrap probe failed: bwrap: some other failure"},
	}
	for _, tc := range cases {
		got := classifyBwrapFailure(tc.output, os.ErrPermission)
		if !strings.Contains(got, tc.contains) {
			t.Fatalf("%s: classifyBwrapFailure = %q, want it to contain %q", tc.name, got, tc.contains)
		}
		if strings.Contains(got, "\n") {
			t.Fatalf("%s: 诊断文案必须只有一行：%q", tc.name, got)
		}
	}
	// 长输出要被裁到 200 字节附近（不分行时走截断分支）。
	long := strings.Repeat("x", 500)
	if got := classifyBwrapFailure(long, os.ErrPermission); len(got) > 260 {
		t.Fatalf("长输出没有被裁剪：%d 字节", len(got))
	}
	// 空输出退化到 runErr 的文案。
	if got := classifyBwrapFailure("", os.ErrPermission); !strings.Contains(got, "permission denied") {
		t.Fatalf("空输出的分类 = %q, want 回落到 runErr", got)
	}
}

// TestProbeBwrapFailurePaths 覆盖 bwrap 探测的两条"没装/不可信"分支。
//
// 用 PATH 注入而不是依赖宿主：这样在没有任何 bwrap 的 CI 上也能跑。
func TestProbeBwrapFailurePaths(t *testing.T) {
	// 1) PATH 里没有 bwrap。
	empty := t.TempDir()
	t.Setenv("PATH", empty)
	if _, reason := probeBwrapDetailed(0); !strings.Contains(reason, "not installed") {
		t.Fatalf("空 PATH 的探测原因 = %q, want 包含 not installed", reason)
	}
	if _, ok := probeBwrap(0); ok {
		t.Fatal("probeBwrap 在空 PATH 上报告可用")
	}

	// 2) PATH 里有一个 group/world 可写的 bwrap 候选：必须拒收（沙箱本体不能被替换）。
	bogus := filepath.Join(t.TempDir(), "bwrap")
	if err := os.WriteFile(bogus, []byte("#!/bin/sh\nexit 0\n"), 0o777); err != nil {
		t.Fatalf("write fake bwrap: %v", err)
	}
	if err := os.Chmod(bogus, 0o777); err != nil {
		t.Fatalf("chmod fake bwrap: %v", err)
	}
	t.Setenv("PATH", filepath.Dir(bogus))
	if _, reason := probeBwrapDetailed(0); !strings.Contains(reason, "refusing to trust") {
		t.Fatalf("世界可写候选的探测原因 = %q, want 包含 refusing to trust", reason)
	}
}

// TestParseLandlockProbeOutput 覆盖 helper --probe 的输出解析（纯函数）。
func TestParseLandlockProbeOutput(t *testing.T) {
	if abi, ok := parseLandlockProbeOutput(landlockProbePrefix + "4\n"); !ok || abi != 4 {
		t.Fatalf("正常输出 = (%d,%v), want (4,true)", abi, ok)
	}
	for _, bad := range []string{"", "no prefix here", landlockProbePrefix + "x", landlockProbePrefix + "0", landlockProbePrefix + "-1"} {
		if abi, ok := parseLandlockProbeOutput(bad); ok {
			t.Fatalf("畸形输出 %q 被接受为 abi=%d", bad, abi)
		}
	}
}

// TestLandlockHelperFail 覆盖 helper 失败的退出码与文案前缀。
func TestLandlockHelperFail(t *testing.T) {
	if got := landlockHelperFail(os.ErrPermission); got != landlockHelperExitCode {
		t.Fatalf("landlockHelperFail 退出码 = %d, want %d", got, landlockHelperExitCode)
	}
}

// TestReporterSurfaceLinuxBackends 覆盖 bwrap/Landlock 的 Reporter 面。
func TestReporterSurfaceLinuxBackends(t *testing.T) {
	bwrap := &bwrapRunner{path: "/usr/bin/bwrap"}
	if got := bwrap.RunnerName(); got != RunnerBwrap {
		t.Fatalf("bwrap RunnerName = %q", got)
	}
	if got := bwrap.RunnerEnforcement(); got != sandbox.EnforcementFull {
		t.Fatalf("bwrap RunnerEnforcement = %q, want full", got)
	}
	if got := bwrap.RunnerLimits(); !strings.Contains(got, "NOT governed") {
		t.Fatalf("bwrap RunnerLimits = %q，应当说明网络与 daemon socket 不受治理", got)
	}
	if got := bwrap.NetworkCoverage(); got != "none" {
		t.Fatalf("bwrap NetworkCoverage = %q, want none", got)
	}

	landlock := &landlockRunner{exe: "/opt/rig/host", abi: 4}
	if got := landlock.RunnerName(); got != RunnerLandlock {
		t.Fatalf("landlock RunnerName = %q", got)
	}
	if got := landlock.RunnerEnforcement(); got != sandbox.EnforcementPartial {
		t.Fatalf("landlock RunnerEnforcement = %q, want partial", got)
	}
	if got := landlock.RunnerLimits(); !strings.Contains(got, "network is NOT governed") {
		t.Fatalf("landlock RunnerLimits = %q，应当说明网络不受治理", got)
	}
	if got := landlock.NetworkCoverage(); got != "none" {
		t.Fatalf("landlock NetworkCoverage = %q, want none", got)
	}
	var nilLandlock *landlockRunner
	if got := nilLandlock.NetworkCoverage(); got != "none" {
		t.Fatalf("nil landlock NetworkCoverage = %q", got)
	}
	var nilBwrap *bwrapRunner
	if got := nilBwrap.NetworkCoverage(); got != "none" {
		t.Fatalf("nil bwrap NetworkCoverage = %q", got)
	}
}

// TestQueryLandlockABIIfSupported 在不施加任何规则的前提下查询内核 ABI
// （LANDLOCK_CREATE_RULESET_VERSION 只读，不会限制当前进程）。
func TestQueryLandlockABIIfSupported(t *testing.T) {
	abi, err := queryLandlockABI()
	if err != nil {
		t.Skipf("本机内核没有 Landlock（或不允许查询）：%v", err)
	}
	if abi < 1 {
		t.Fatalf("ABI = %d, want >= 1", abi)
	}
	if abi > 0 && handledAccessMask(abi) == 0 {
		t.Fatalf("ABI %d 的 handledAccessMask 为空", abi)
	}
}

// TestHandledMasksByABI 覆盖 ABI → 访问位映射的每个档位。
func TestHandledMasksByABI(t *testing.T) {
	if mask := handledAccessMask(0); mask != 0 {
		t.Fatalf("handledAccessMask(0) = %#x, want 0", mask)
	}
	base := handledAccessMask(1)
	if base == 0 || base&landlockAccessFSRefer != 0 || base&landlockAccessFSTruncate != 0 {
		t.Fatalf("ABI1 mask = %#x", base)
	}
	if handledAccessMask(2)&landlockAccessFSRefer == 0 {
		t.Fatal("ABI2 缺少 REFER")
	}
	if handledAccessMask(3)&landlockAccessFSTruncate == 0 {
		t.Fatal("ABI3 缺少 TRUNCATE")
	}
	if handledAccessMask(4) != handledAccessMask(3) {
		t.Fatal("ABI4 只增加网络位，文件系统掩码应与 ABI3 相同")
	}
	if handledAccessMask(5)&landlockAccessFSIoctlDev == 0 {
		t.Fatal("ABI5 缺少 IOCTL_DEV")
	}
}
