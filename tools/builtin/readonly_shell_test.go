package builtin_test

// read-only 档的 shell（A4 收尾）：有 OS runner 时允许执行，且必须真的经过 runner。
//
// read-only 以前在工具层就整体拒绝 shell，于是"装了 runner 的 read-only 会话"也
// 一条命令都跑不了——而这条组合恰恰是最需要能跑命令的：RUNNER 负责把写挡在工作区
// 之外，唯一可写的是私有 scratch（$TMPDIR）。

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/builtin"
)

// readonlyBash 构造 read-only 策略 + 注入的 runner 的 bash 工具。
func readonlyBash(t *testing.T, root string, runner sandbox.Runner, opts ...builtin.Option) rig.Tool {
	t.Helper()
	opts = append(opts, builtin.WithRunner(runner))
	return policyToolMap(t, builtin.NewWithPolicyOptions(sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeReadOnly),
		sandbox.WithAllowShellOperators(),
		sandbox.WithAnyCommandAllowed(true),
	), opts...))[builtin.ToolBash]
}

// TestReadOnlyWithRunnerRunsConfinedShell 钉住 read-only + runner：命令被执行，
// 且**确实**经过 runner 包装（read-only 不会因此变成"跳过沙箱"）。
func TestReadOnlyWithRunnerRunsConfinedShell(t *testing.T) {
	root := policyRoot(t)
	runner := &fakeRunner{}
	bash := readonlyBash(t, root, runner)

	result := invokePolicy(t, map[string]rig.Tool{builtin.ToolBash: bash}, builtin.ToolBash,
		`{"command":"echo readonly-ok"}`)
	if result.IsError {
		t.Fatalf("read-only + runner 下 bash 应当允许执行: %q", result.Content)
	}
	if !strings.Contains(result.Content, "readonly-ok") {
		t.Fatalf("bash 输出 = %q, want readonly-ok", result.Content)
	}
	if runner.callCount() == 0 {
		t.Fatal("read-only 下命令没有经过 OS runner（不许跳过沙箱）")
	}
}

// TestReadOnlyWithRunnerAndScratchWritesToTmpdir 钉住"read-only + scratch"这条
// 真实用法：往 $TMPDIR 写能成功（scratch 就是那一档唯一的可写目录）。
func TestReadOnlyWithRunnerAndScratchWritesToTmpdir(t *testing.T) {
	root := policyRoot(t)
	scratch := filepath.Join(t.TempDir(), "scratch")
	bash := policyToolMap(t, builtin.NewWithPolicyOptions(sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeReadOnly),
		sandbox.WithScratchDir(scratch),
		sandbox.WithAllowShellOperators(),
		sandbox.WithAnyCommandAllowed(true),
	), builtin.WithRunner(&fakeRunner{})))[builtin.ToolBash]

	result := invokePolicy(t, map[string]rig.Tool{builtin.ToolBash: bash}, builtin.ToolBash,
		`{"command":"echo scratch-ok > \"$TMPDIR/f\"; cat \"$TMPDIR/f\""}`)
	if result.IsError {
		t.Fatalf("read-only + scratch 下写 $TMPDIR 应当成功: %q", result.Content)
	}
	if !strings.Contains(result.Content, "scratch-ok") {
		t.Fatalf("bash 输出 = %q, want scratch-ok", result.Content)
	}
}

// TestReadOnlyWithoutRunnerStillRejectsShell 钉住"没有 runner 就没有边界"：
// read-only 无 runner 时仍然拒绝，并且文案要说明缺的是 runner。
func TestReadOnlyWithoutRunnerStillRejectsShell(t *testing.T) {
	root := policyRoot(t)
	bash := policyToolMap(t, builtin.NewWithPolicyOptions(sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeReadOnly),
		sandbox.WithAnyCommandAllowed(true),
	)))[builtin.ToolBash]

	result := invokePolicy(t, map[string]rig.Tool{builtin.ToolBash: bash}, builtin.ToolBash,
		`{"command":"echo should-not-run"}`)
	if !result.IsError {
		t.Fatalf("read-only 无 runner 时 bash 必须拒绝: %q", result.Content)
	}
	if !strings.Contains(result.Content, "runner") {
		t.Fatalf("拒绝文案要说明缺 runner（要么挂 runner，要么用 workspace-write）: %q", result.Content)
	}
	if strings.Contains(result.Content, "should-not-run") {
		t.Fatalf("命令竟然被执行了: %q", result.Content)
	}
}

// 说明：这里没有"真实 bwrap 下 sh -c 'echo x > $TMPDIR/f' 成功、写工作区失败"的
// 集成断言——osrunner 的测试只覆盖 profile 渲染（bwrap/Landlock/Seatbelt 参数），
// 不 spawn 真实受限进程；CI 环境里 bubblewrap 是否可用/能否 userns 也不稳定。
// 那条端到端路径由 profile 渲染断言（sandbox/osrunner/scratch_test.go 的
// TestBwrapArgsReadOnlyWithScratchBindsScratch）与上面的工具级断言共同覆盖。
