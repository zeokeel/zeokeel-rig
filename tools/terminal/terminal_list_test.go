package terminal_test

// Tests for the legacy terminal_list tool: it lists registered sessions with
// their transport, cwd, running flag and exit code.

import (
	"context"
	"strings"
	"testing"

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
	"github.com/zeokeel/zeokeel-rig/tools/terminal"
)

func newToolsWithList(t *testing.T) (*terminal.Manager, map[string]v1.Tool) {
	t.Helper()
	manager := terminal.NewManager(terminal.WithTransport(terminal.TransportPipe))
	t.Cleanup(func() { _ = manager.CloseAll() })
	byName := make(map[string]v1.Tool)
	for _, tool := range manager.ToolsWithList() {
		byName[tool.Name()] = tool
	}
	return manager, byName
}

func TestToolsWithListShape(t *testing.T) {
	manager, tools := newToolsWithList(t)
	if len(tools) != 6 {
		t.Fatalf("ToolsWithList = %d tools, want 6", len(tools))
	}
	for _, name := range []string{
		terminal.ToolTerminalOpen, terminal.ToolTerminalSend, terminal.ToolTerminalRead,
		terminal.ToolTerminalSignal, terminal.ToolTerminalClose, terminal.ToolTerminalList,
	} {
		tool, ok := tools[name]
		if !ok {
			t.Fatalf("missing tool %q", name)
		}
		if tool.Description() == "" || tool.Schema() == nil {
			t.Fatalf("tool %q has empty description/schema", name)
		}
	}
	if terminal.NewTerminalListTool(manager) == nil {
		t.Fatal("NewTerminalListTool(manager) = nil")
	}
	if terminal.NewTerminalListTool(nil) != nil {
		t.Fatal("NewTerminalListTool(nil) should be nil")
	}
	if manager.TerminalListTool() == nil {
		t.Fatal("(*Manager).TerminalListTool() = nil")
	}
	if terminal.ToolsWithList(nil) != nil {
		t.Fatal("ToolsWithList(nil) should be nil")
	}
}

func TestTerminalListEmpty(t *testing.T) {
	_, tools := newToolsWithList(t)
	result := invokeTerminal(t, tools, terminal.ToolTerminalList, `{}`)
	if result.IsError || result.Content != "no terminal sessions" {
		t.Fatalf("terminal_list = %q (IsError=%v), want the empty note", result.Content, result.IsError)
	}
}

func TestTerminalListRunningSession(t *testing.T) {
	manager, tools := newToolsWithList(t)
	sessionID := openSession(t, tools, `{}`)
	if manager.Len() != 1 {
		t.Fatalf("manager.Len = %d, want 1", manager.Len())
	}
	result := invokeTerminal(t, tools, terminal.ToolTerminalList, `{}`)
	if result.IsError {
		t.Fatalf("terminal_list: %q", result.Content)
	}
	for _, want := range []string{
		"session_id: " + sessionID,
		"transport: pipe",
		"running: true",
		"exit: -",
		"cwd: ",
	} {
		if !strings.Contains(result.Content, want) {
			t.Fatalf("terminal_list = %q, want %q", result.Content, want)
		}
	}
}

func TestTerminalListShowsExitCode(t *testing.T) {
	_, tools := newToolsWithList(t)
	sessionID := openSession(t, tools, `{}`)
	// `exit 3` terminates the shell; the session stays registered until closed.
	output := sendCommand(t, tools, sessionID, "exit 3")
	if !strings.Contains(output, "shell exited") || !strings.Contains(output, "3") {
		t.Fatalf("exit output = %q", output)
	}
	result := invokeTerminal(t, tools, terminal.ToolTerminalList, `{}`)
	if result.IsError {
		t.Fatalf("terminal_list: %q", result.Content)
	}
	for _, want := range []string{"session_id: " + sessionID, "running: false", "exit: 3"} {
		if !strings.Contains(result.Content, want) {
			t.Fatalf("terminal_list after exit = %q, want %q", result.Content, want)
		}
	}
}

func TestTerminalListMultipleAndClosed(t *testing.T) {
	_, tools := newToolsWithList(t)
	first := openSession(t, tools, `{}`)
	second := openSession(t, tools, `{}`)
	result := invokeTerminal(t, tools, terminal.ToolTerminalList, `{}`)
	if got := strings.Count(result.Content, "session_id:"); got != 2 {
		t.Fatalf("terminal_list has %d sessions, want 2:\n%s", got, result.Content)
	}
	for _, id := range []string{first, second} {
		if !strings.Contains(result.Content, "session_id: "+id) {
			t.Fatalf("terminal_list missing %q:\n%s", id, result.Content)
		}
	}

	closed := invokeTerminal(t, tools, terminal.ToolTerminalClose, `{"session_id":"`+first+`"}`)
	if closed.IsError {
		t.Fatalf("terminal_close: %q", closed.Content)
	}
	result = invokeTerminal(t, tools, terminal.ToolTerminalList, `{}`)
	if strings.Count(result.Content, "session_id:") != 1 {
		t.Fatalf("terminal_list after close = %q, want one session", result.Content)
	}
	if strings.Contains(result.Content, first) {
		t.Fatalf("closed session still listed: %q", result.Content)
	}
}

func TestTerminalListCancelledContext(t *testing.T) {
	_, tools := newToolsWithList(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tools[terminal.ToolTerminalList].Invoke(ctx, v1.ToolCall{Arguments: `{}`}); err == nil {
		t.Fatal("cancelled terminal_list returned nil error")
	}
}
