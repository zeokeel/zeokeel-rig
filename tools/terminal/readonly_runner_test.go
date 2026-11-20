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
// terminal_open 能开出来、能执行命令，且**会话的 shell 就是 runner 返回的 argv**。
//
// 只断言 "Confine 被调用过" 强度不够：变异证明"调了 Confine 但 spawn 未受限的
// argv"仍会绿。所以让假 runner 把 shell 改写成"先打一行标记、再 exec 回原 shell"，
// 断言标记出现在会话输出里（管道与 PTY 两条 spawn 路径都是同一个判据）。
func TestTerminalOpenReadOnlyWithRunnerRunsConfined(t *testing.T) {
	root := t.TempDir()
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	policy := sandbox.New(sandbox.WithRoots(realRoot), sandbox.WithCommandAllowlist("sh", "echo"), sandbox.WithReadOnly(true))
	runner := &fakeRunner{rewrite: func(argv []string) ([]string, error) {
		return []string{argv[0], "-c", "printf 'runner-wrapped\\n'; exec " + argv[0]}, nil
	}}
	_, tools := newTerminalTools(t, terminal.WithPolicy(policy), terminal.WithRunner(runner))

	sessionID := openSession(t, tools, fmt.Sprintf(`{"cwd":%q}`, realRoot))
	startup := invokeTerminal(t, tools, terminal.ToolTerminalRead,
		`{"session_id":"`+sessionID+`","wait_ms":5000}`)
	if startup.IsError {
		t.Fatalf("terminal_read = %q", startup.Content)
	}
	if !strings.Contains(startup.Content, "runner-wrapped") {
		t.Fatalf("terminal 启动输出 = %q，want runner-wrapped（spawn 的必须是 runner 返回的受限 argv）", startup.Content)
	}
	if output := sendCommand(t, tools, sessionID, "echo confined"); !strings.Contains(output, "confined") {
		t.Fatalf("read-only terminal output = %q, want confined", output)
	}
	if runner.callCount() != 1 {
		t.Fatalf("runner calls = %d, want 1（read-only 下 shell 必须经 runner 包装）", runner.callCount())
	}
}

// TestTerminalOpenReadOnlyWithRunnerConfinesPTYSpawn 覆盖 PTY 那条 spawn 站点：
// 与管道站点同一个判据——会话里能看到的启动标记只能来自 runner 返回的 argv。
// PTY 不可用时 open 会回退到管道（同样经 runner），断言依然成立。
func TestTerminalOpenReadOnlyWithRunnerConfinesPTYSpawn(t *testing.T) {
	root := t.TempDir()
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	policy := sandbox.New(sandbox.WithRoots(realRoot), sandbox.WithCommandAllowlist("sh", "echo"), sandbox.WithReadOnly(true))
	runner := &fakeRunner{rewrite: func(argv []string) ([]string, error) {
		return []string{argv[0], "-c", "printf 'pty-runner-wrapped\\n'; exec " + argv[0] + " -i"}, nil
	}}
	_, tools := newTerminalTools(t,
		terminal.WithTransport(terminal.TransportPTY),
		terminal.WithPolicy(policy), terminal.WithRunner(runner))

	sessionID := openSession(t, tools, fmt.Sprintf(`{"cwd":%q}`, realRoot))
	startup := invokeTerminal(t, tools, terminal.ToolTerminalRead,
		`{"session_id":"`+sessionID+`","wait_ms":5000}`)
	if startup.IsError {
		t.Fatalf("terminal_read = %q", startup.Content)
	}
	if !strings.Contains(startup.Content, "pty-runner-wrapped") {
		t.Fatalf("PTY 启动输出 = %q，want pty-runner-wrapped（PTY 站点的 spawn 必须用受限 argv）", startup.Content)
	}
	if runner.callCount() != 1 {
		t.Fatalf("runner calls = %d, want 1（read-only 下 PTY shell 必须经 runner 包装）", runner.callCount())
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
