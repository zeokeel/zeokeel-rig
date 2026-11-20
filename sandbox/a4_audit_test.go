package sandbox_test

// A4 审计回归：read-only 档的"可写面"口径必须在三处一致。
//
// 同一件事有三个消费者：`Policy.WritableRootsFor`（策略层唯一口径）、
// `Preflight`（给用户看的一句话）与 `sandbox_status.writableRoots`（给模型看）。
// read-only 档**只有**私有 scratch 可写（没有 scratch 就是"没有可写目录"），
// 工作区与 /tmp 都不可写。以前 Preflight 仍然说"只能改 <workspace>（以及私有临时
// 目录 scratch）"，把工作区说成可写——宿主照这句话判断边界，等于把沙箱的承诺
// 说大了。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// readOnlyPreflightSummary 跑一次 read-only 的 Preflight，返回拼好的 summary 文本。
func readOnlyPreflightSummary(t *testing.T, policy *sandbox.Policy) string {
	t.Helper()
	runner := &fakeReporter{name: "bwrap", enforcement: sandbox.EnforcementFull, coverage: "full"}
	report := sandbox.Preflight(policy, runner, true)
	if len(report.Summaries) == 0 {
		t.Fatalf("Preflight 没有给出 summary")
	}
	return strings.Join(report.Summaries, " ")
}

// TestPreflightReadOnlyWithoutScratchSaysNothingIsWritable 断言 read-only 且没有
// scratch 时，文案明说"没有可写目录"，且不再把工作区说成可写。
func TestPreflightReadOnlyWithoutScratchSaysNothingIsWritable(t *testing.T) {
	root := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeReadOnly))

	summary := readOnlyPreflightSummary(t, policy)
	if !strings.Contains(summary, "没有可写目录") {
		t.Fatalf("read-only 且无 scratch 时必须明说「没有可写目录」：%q", summary)
	}
	if strings.Contains(summary, root) {
		t.Fatalf("read-only 下工作区不可写，文案不该出现工作区路径 %q：%q", root, summary)
	}
}

// TestPreflightReadOnlyWithScratchNamesOnlyScratch 断言 read-only + scratch 时文案
// 只点名 scratch（工作区仍然不可写）。
func TestPreflightReadOnlyWithScratchNamesOnlyScratch(t *testing.T) {
	root := t.TempDir()
	scratch := filepath.Join(t.TempDir(), "scratch")
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeReadOnly),
		sandbox.WithScratchDir(scratch),
	)

	summary := readOnlyPreflightSummary(t, policy)
	if !strings.Contains(summary, scratch) {
		t.Fatalf("文案必须点名唯一可写的 scratch %q：%q", scratch, summary)
	}
	if strings.Contains(summary, root) {
		t.Fatalf("read-only 下工作区不可写，文案不该出现工作区路径 %q：%q", root, summary)
	}
}

// TestPreflightWorkspaceWriteStillNamesWorkspace 反向守护：workspace-write 下工作区
// 是可写的，文案必须照旧点名它（别为了修 read-only 把这一档改坏）。
func TestPreflightWorkspaceWriteStillNamesWorkspace(t *testing.T) {
	root := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	runner := &fakeReporter{name: "bwrap", enforcement: sandbox.EnforcementFull, coverage: "full"}
	report := sandbox.Preflight(policy, runner, true)
	summary := strings.Join(report.Summaries, " ")
	if !strings.Contains(summary, root) {
		t.Fatalf("workspace-write 下文案必须点名可写的工作区 %q：%q", root, summary)
	}
}

// TestWritableRootsForEmptyModeUsesPolicyMode 断言 `WritableRootsFor("")` 用**策略
// 自己的模式**，而不是被当成 read-only。
//
// 以前 `case "", ModeReadOnly` 把空串归到只读档，于是任何一个"没传模式"的调用方
// （UI 初期、诊断脚本）都会把 workspace-write 会话报成"只有 /dev/null 可写"。
func TestWritableRootsForEmptyModeUsesPolicyMode(t *testing.T) {
	root := t.TempDir()
	workspaceWrite := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	got := workspaceWrite.WritableRootsFor("")
	containsRoot := false
	for _, path := range got {
		if path == root {
			containsRoot = true
		}
	}
	if !containsRoot {
		t.Fatalf("WritableRootsFor(\"\") = %v, want it to contain the workspace root %q", got, root)
	}

	readOnly := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeReadOnly))
	for _, path := range readOnly.WritableRootsFor("") {
		if path == root {
			t.Fatalf("read-only 策略的 WritableRootsFor(\"\") 不该包含工作区：%v", readOnly.WritableRootsFor(""))
		}
	}
}

// TestScratchDirExistingDirIsTightenedTo0700 断言"私有"口径对**已存在**目录同样
// 成立：scratch 目录已存在时也要 chmod 0700，而不是沿用它的 0755。
func TestScratchDirExistingDirIsTightenedTo0700(t *testing.T) {
	root := t.TempDir()
	scratch := filepath.Join(root, "scratch")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithScratchDir(scratch))
	if err := policy.ScratchDirError(); err != nil {
		t.Fatalf("ScratchDirError() = %v, want nil", err)
	}
	info, err := os.Stat(scratch)
	if err != nil {
		t.Fatalf("stat scratch: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("已存在的 scratch 目录权限 = %o, want 700", got)
	}
}

// TestWithScratchEnvKeepsUnrelatedDuplicates 断言 scratch 只覆盖三个临时键：
// 其它键（包括重复出现的 PATH/FOO）必须原样保留。
//
// 走公开表面验证：CommandEnv 是 withScratchEnv 的唯一调用方。
func TestWithScratchEnvKeepsUnrelatedDuplicates(t *testing.T) {
	scratch := filepath.Join(t.TempDir(), "scratch")
	policy := sandbox.New(sandbox.WithScratchDir(scratch))

	base := []string{"PATH=/a", "PATH=/b", "TMPDIR=/old", "FOO=1", "FOO=2", "TMP=/old-tmp"}
	got := policy.CommandEnv(base)

	counts := map[string]int{}
	for _, entry := range got {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		counts[name]++
		switch name {
		case "TMPDIR", "TMP", "TEMP":
			if value != scratch {
				t.Fatalf("%s = %q, want %q", name, value, scratch)
			}
		}
	}
	for _, name := range []string{"PATH", "FOO"} {
		if counts[name] != 2 {
			t.Fatalf("%s 出现 %d 次，want 2（其它键必须原样保留，含重复）：%v", name, counts[name], got)
		}
	}
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		if counts[name] != 1 {
			t.Fatalf("%s 出现 %d 次，want 1（只允许一个 scratch 值）：%v", name, counts[name], got)
		}
	}
}
