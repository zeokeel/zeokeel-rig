package terminal_test

// terminal 侧的沙箱归因（docs/SANDBOX-OS.md 记录的缺口）：交互式 shell 没有
// 逐命令退出码语义，因此只要会话的新输出命中 runner 的 DenialSignatures，就在
// terminal_send/terminal_read 的结果末尾追加结构化说明（被拒目标、命中方言、
// 可写面与处置顺序）。未命中方言时结果与“没有 runner”的同一命令逐字节一致。

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/terminal"
)

// sandboxNotePrefix 是结构化沙箱拒绝说明的稳定前缀（与 builtin/jobs 侧一致）。
const sandboxNotePrefix = "[sandbox: denied "

// attrPolicy 构造允许 shell 操作符（测试里要写 stderr 重定向）的管道终端策略。
func attrPolicy(root string) *sandbox.Policy {
	return sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithCommandAllowlist("sh", "echo", "sleep"),
		sandbox.WithAllowShellOperators(),
	)
}

// newAttrSession 打开一个 pipe 会话并返回 terminal_send/terminal_read 工具。
func newAttrSession(t *testing.T, root string, runner sandbox.Runner) (*terminal.Manager, map[string]rig.Tool, string) {
	t.Helper()
	manager := terminal.NewManager(
		terminal.WithTransport(terminal.TransportPipe),
		terminal.WithPolicy(attrPolicy(root)),
		terminal.WithRunner(runner),
	)
	t.Cleanup(func() { _ = manager.CloseAll() })
	info, err := manager.Open(terminal.OpenOptions{Cwd: root})
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	byName := make(map[string]rig.Tool)
	for _, tool := range manager.Tools() {
		byName[tool.Name()] = tool
	}
	return manager, byName, info.ID
}

// send 向会话发送一行输入并返回结果。
func send(t *testing.T, tools map[string]rig.Tool, sessionID, input string) rig.ToolResult {
	t.Helper()
	tool, ok := tools[terminal.ToolTerminalSend]
	if !ok {
		t.Fatalf("%s not found in %v", terminal.ToolTerminalSend, tools)
	}
	result, err := tool.Invoke(context.Background(), rig.ToolCall{
		ID:   "send",
		Name: terminal.ToolTerminalSend,
		Arguments: `{"sessionId":"` + sessionID + `","text":` +
			quoteJSON(input) + `}`,
	})
	if err != nil {
		t.Fatalf("terminal_send.Invoke returned Go error: %v", err)
	}
	return result
}

// quoteJSON 把输入编码成 JSON 字符串（只需处理测试里出现的引号与反斜杠）。
func quoteJSON(text string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + replacer.Replace(text) + `"`
}

// requireContains 断言子串。
func requireContains(t *testing.T, text string, fragments ...string) {
	t.Helper()
	for _, fragment := range fragments {
		if !strings.Contains(text, fragment) {
			t.Fatalf("text does not contain %q:\n%s", fragment, text)
		}
	}
}

// TestTerminalSendAttributesSandboxDenial 证明命中方言的输出追加结构化归因（不
// 依赖退出码：交互式 shell 里没有逐命令退出码）。
func TestTerminalSendAttributesSandboxDenial(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("/bin/sh unavailable: %v", err)
	}
	root := t.TempDir()
	runner := &fakeRunner{
		signatures:  []string{"read-only file system"},
		enforcement: sandbox.EnforcementFull,
	}
	_, tools, sessionID := newAttrSession(t, root, runner)

	result := send(t, tools, sessionID, `echo "Read-only file system" >&2`)
	if result.IsError {
		t.Fatalf("terminal_send = %q, want a normal result", result.Content)
	}
	requireContains(t, result.Content,
		"Read-only file system",
		sandboxNotePrefix,
		`matched "read-only file system"`,
		"mode workspace-write",
		"enforcement full",
	)
	lines := strings.Split(strings.TrimRight(result.Content, "\n"), "\n")
	if last := lines[len(lines)-1]; !strings.Contains(last, "sandbox") {
		t.Fatalf("attribution must terminate the result, last line = %q:\n%s", last, result.Content)
	}
}

// TestTerminalSendWithoutDialectIsByteIdentical 证明未命中方言时结果与没有 runner
// 的同一命令逐字节一致（成功路径不变）。
func TestTerminalSendWithoutDialectIsByteIdentical(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("/bin/sh unavailable: %v", err)
	}
	root := t.TempDir()
	const input = `echo all good`

	_, plainTools, plainSession := newAttrSession(t, root, nil)
	plain := send(t, plainTools, plainSession, input)

	noMatch := &fakeRunner{signatures: []string{"read-only file system"}}
	_, runnerTools, runnerSession := newAttrSession(t, root, noMatch)
	withRunner := send(t, runnerTools, runnerSession, input)

	if plain.Content != withRunner.Content {
		t.Fatalf("clean output changed with a runner:\nplain  %q\nrunner %q", plain.Content, withRunner.Content)
	}
	if strings.Contains(withRunner.Content, sandboxNotePrefix) {
		t.Fatalf("clean output must not carry attribution:\n%s", withRunner.Content)
	}
}

// TestTerminalReadAttributesSandboxDenial 证明后台发送产出的输出在 terminal_read
// 里同样被归因（后台路径没有 terminal_send 的结果可追加）。
func TestTerminalReadAttributesSandboxDenial(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("/bin/sh unavailable: %v", err)
	}
	root := t.TempDir()
	runner := &fakeRunner{signatures: []string{"permission denied"}}
	_, tools, sessionID := newAttrSession(t, root, runner)

	readTool, ok := tools[terminal.ToolTerminalRead]
	if !ok {
		t.Fatalf("%s not found", terminal.ToolTerminalRead)
	}
	// 让 shell 在 send 返回之后才打印方言：输出会留在缓冲里，只能由 read 取回
	// （这正是后台发送的收集路径）。
	send(t, tools, sessionID, `(sleep 0.2; echo "Permission denied" >&2) &`)
	result, err := readTool.Invoke(context.Background(), rig.ToolCall{
		ID:        "read",
		Name:      terminal.ToolTerminalRead,
		Arguments: `{"sessionId":"` + sessionID + `","wait_ms":1000}`,
	})
	if err != nil {
		t.Fatalf("terminal_read.Invoke returned Go error: %v", err)
	}
	if strings.TrimSpace(result.Content) == "" {
		t.Skip("the delayed output never reached the session ring on this platform")
	}
	requireContains(t, result.Content, "Permission denied", sandboxNotePrefix, "mode workspace-write")
}
