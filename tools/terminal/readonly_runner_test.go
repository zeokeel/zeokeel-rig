package terminal_test

// read-only + runner 的口径：terminal_open 与前台 bash 一致——挂了 OS runner 时
// 允许（shell 仍由 runner 包装、逐行命令校验照旧），没挂 runner 时仍然拒绝，且
// 拒绝文案要说清"缺的是 runner"。

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/terminal"
)

// TestTerminalOpenReadOnlyWithRunnerRunsConfined 钉住 read-only + runner 下
// terminal_open 能开出来、能执行命令，且 shell 真的经过了 runner 包装。
func TestTerminalOpenReadOnlyWithRunnerRunsConfined(t *testing.T) {
	root := t.TempDir()
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	policy := sandbox.New(sandbox.WithRoots(realRoot), sandbox.WithCommandAllowlist("sh", "echo"), sandbox.WithReadOnly(true))
	runner := &fakeRunner{}
	_, tools := newTerminalTools(t, terminal.WithPolicy(policy), terminal.WithRunner(runner))

	sessionID := openSession(t, tools, fmt.Sprintf(`{"cwd":%q}`, realRoot))
	if output := sendCommand(t, tools, sessionID, "echo confined"); !strings.Contains(output, "confined") {
		t.Fatalf("read-only terminal output = %q, want confined", output)
	}
	if runner.callCount() != 1 {
		t.Fatalf("runner calls = %d, want 1（read-only 下 shell 必须经 runner 包装）", runner.callCount())
	}
}

// TestTerminalOpenReadOnlyWithoutRunnerIsDenied 钉住没有 runner 时 read-only 仍然
// 拒绝，并且错误文案要提 runner（与 bash 的措辞一致）。
func TestTerminalOpenReadOnlyWithoutRunnerIsDenied(t *testing.T) {
	root := t.TempDir()
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	policy := sandbox.New(sandbox.WithRoots(realRoot), sandbox.WithReadOnly(true))
	_, tools := newTerminalTools(t, terminal.WithPolicy(policy))

	text := invokeTerminalError(t, tools, terminal.ToolTerminalOpen, `{}`)
	for _, want := range []string{"read-only", "runner"} {
		if !strings.Contains(text, want) {
			t.Fatalf("read-only open error = %q, want it to mention %q", text, want)
		}
	}
}
