package builtin_test

// bash 子进程的临时目录环境（A4）。
//
// 策略配了私有 scratch 时，bash 拿到的 TMPDIR/TMP/TEMP 必须都指向它——否则
// 可写面虽然在策略里、命令却仍然往 /tmp 写，等于 scratch 白配。

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/builtin"
)

// TestBashSeesScratchTempEnv 钉住 bash 收到的 TMPDIR/TMP/TEMP 都是 scratch。
func TestBashSeesScratchTempEnv(t *testing.T) {
	root := policyRoot(t)
	scratch := filepath.Join(t.TempDir(), "scratch")
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithScratchDir(scratch),
		sandbox.WithAnyCommandAllowed(true),
	)
	tools := policyToolMap(t, builtin.NewWithPolicyOptions(policy))

	result := invokePolicy(t, tools, builtin.ToolBash,
		`{"command":"printf '%s|%s|%s' \"$TMPDIR\" \"$TMP\" \"$TEMP\""}`)
	if result.IsError {
		t.Fatalf("bash 失败: %q", result.Content)
	}
	if got := strings.TrimSpace(result.Content); got != scratch+"|"+scratch+"|"+scratch {
		t.Fatalf("TMPDIR|TMP|TEMP = %q, want %q", got, scratch+"|"+scratch+"|"+scratch)
	}
}

// TestBashSeesScratchThroughRunner 钉住同样的事实经过 OS runner 包装后仍成立
// （环境挂在 exec 上，与 runner 的 argv 重写无关）。
func TestBashSeesScratchThroughRunner(t *testing.T) {
	root := policyRoot(t)
	scratch := filepath.Join(t.TempDir(), "scratch")
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithScratchDir(scratch),
		sandbox.WithAnyCommandAllowed(true),
	)
	runner := &fakeRunner{}
	tools := policyToolMap(t, builtin.NewWithPolicyOptions(policy, builtin.WithRunner(runner)))
	bash := tools[builtin.ToolBash]

	result := invokePolicy(t, map[string]rig.Tool{builtin.ToolBash: bash}, builtin.ToolBash,
		`{"command":"printf '%s' \"$TMPDIR\""}`)
	if result.IsError {
		t.Fatalf("bash 失败: %q", result.Content)
	}
	if got := strings.TrimSpace(result.Content); got != scratch {
		t.Fatalf("TMPDIR = %q, want %q", got, scratch)
	}
	if runner.callCount() == 0 {
		t.Fatal("runner 没被调用：这条断言要求命令真的经过 OS 沙箱")
	}
}
