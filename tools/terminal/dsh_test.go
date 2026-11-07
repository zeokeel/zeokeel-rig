package terminal_test

// DSH-compatibility tests for the terminal tool set: camelCase argument
// aliases, type/name on open, submit, paginated terminal_read, DSH signal
// names and background sends wired to a real jobs.Manager.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/jobs"
	"github.com/zeokeel/zeokeel-rig/tools/terminal"
)

// newDSHTerminalTools builds a pipe-transport manager with a real jobs manager
// attached for background sends.
func newDSHTerminalTools(t *testing.T) (*terminal.Manager, map[string]rig.Tool, *jobs.Manager, map[string]rig.Tool) {
	t.Helper()
	background := jobs.NewManager()
	t.Cleanup(func() { _ = background.CloseAll() })
	manager := terminal.NewManager(
		terminal.WithTransport(terminal.TransportPipe),
		terminal.WithBackgroundJobs(background),
	)
	t.Cleanup(func() { _ = manager.CloseAll() })
	byName := make(map[string]rig.Tool)
	for _, tool := range manager.Tools() {
		byName[tool.Name()] = tool
	}
	jobTools := make(map[string]rig.Tool)
	for _, jobTool := range background.Tools() {
		jobTools[jobTool.Name()] = jobTool
	}
	return manager, byName, background, jobTools
}

func invokeJobTool(t *testing.T, tools map[string]rig.Tool, name, arguments string) rig.ToolResult {
	t.Helper()
	tool, ok := tools[name]
	if !ok {
		t.Fatalf("job tool %q not found", name)
	}
	result, err := tool.Invoke(context.Background(), rig.ToolCall{ID: "call-1", Name: name, Arguments: arguments})
	if err != nil {
		t.Fatalf("%s.Invoke returned Go error: %v", name, err)
	}
	return result
}

func TestTerminalDSHAliases(t *testing.T) {
	_, tools := newTerminalTools(t)
	// open accepts type/name (DSH) and still reports session_id.
	result := invokeTerminal(t, tools, terminal.ToolTerminalOpen, `{"type":"pipe","name":"main"}`)
	if result.IsError {
		t.Fatalf("terminal_open(type/name): %q", result.Content)
	}
	for _, fragment := range []string{"session_id: ", "name: main", "transport: pipe"} {
		if !strings.Contains(result.Content, fragment) {
			t.Fatalf("terminal_open output missing %q: %q", fragment, result.Content)
		}
	}
	sessionID := openSession(t, tools, `{}`)
	_ = sessionID

	// send uses sessionId/text; the legacy session_id/input still works.
	sent := invokeTerminal(t, tools, terminal.ToolTerminalSend, `{"sessionId":"`+sessionID+`","text":"echo alias-ok"}`)
	if sent.IsError || !strings.Contains(sent.Content, "alias-ok") {
		t.Fatalf("terminal_send(sessionId/text) = %q (IsError=%v)", sent.Content, sent.IsError)
	}
	sent = invokeTerminal(t, tools, terminal.ToolTerminalSend, `{"session_id":"`+sessionID+`","input":"echo legacy-ok"}`)
	if sent.IsError || !strings.Contains(sent.Content, "legacy-ok") {
		t.Fatalf("terminal_send(session_id/input) = %q (IsError=%v)", sent.Content, sent.IsError)
	}

	// read/close/signal accept sessionId.
	read := invokeTerminal(t, tools, terminal.ToolTerminalRead, `{"sessionId":"`+sessionID+`","offset":0,"count":10}`)
	if read.IsError || !strings.Contains(read.Content, "[lines:") {
		t.Fatalf("terminal_read(sessionId, offset/count) = %q (IsError=%v)", read.Content, read.IsError)
	}
	signalResult := invokeTerminal(t, tools, terminal.ToolTerminalSignal, `{"sessionId":"`+sessionID+`","signal":"SIGINT"}`)
	if signalResult.IsError || !strings.Contains(signalResult.Content, "delivered ") {
		t.Fatalf("terminal_signal(SIGINT) = %q (IsError=%v)", signalResult.Content, signalResult.IsError)
	}
	closed := invokeTerminal(t, tools, terminal.ToolTerminalClose, `{"sessionId":"`+sessionID+`"}`)
	if closed.IsError || !strings.Contains(closed.Content, "closed") {
		t.Fatalf("terminal_close(sessionId) = %q (IsError=%v)", closed.Content, closed.IsError)
	}
}

func TestTerminalOpenTypeAndNameValidation(t *testing.T) {
	_, tools := newTerminalTools(t)
	badType := invokeTerminal(t, tools, terminal.ToolTerminalOpen, `{"type":"serial"}`)
	if !badType.IsError || !strings.Contains(badType.Content, "unknown terminal type") {
		t.Fatalf("terminal_open(bad type) = %q (IsError=%v)", badType.Content, badType.IsError)
	}
	if result := invokeTerminal(t, tools, terminal.ToolTerminalOpen, `{"name":"main"}`); result.IsError {
		t.Fatalf("first named open = %q", result.Content)
	}
	dup := invokeTerminal(t, tools, terminal.ToolTerminalOpen, `{"name":"main"}`)
	if !dup.IsError || !strings.Contains(dup.Content, "already in use") {
		t.Fatalf("duplicate name open = %q (IsError=%v)", dup.Content, dup.IsError)
	}
}

func TestTerminalReadPagination(t *testing.T) {
	_, tools := newTerminalTools(t)
	sessionID := openSession(t, tools, `{}`)
	command := "i=1; while [ $i -le 10 ]; do echo line$i; i=$((i+1)); done"
	if sent := sendCommand(t, tools, sessionID, command); !strings.Contains(sent, "line10") {
		t.Fatalf("setup send = %q", sent)
	}
	// Newest page: lines 8-10, numbered newest-relative 0-3.
	newest := invokeTerminal(t, tools, terminal.ToolTerminalRead,
		`{"session_id":"`+sessionID+`","offset":0,"count":3}`)
	if newest.IsError {
		t.Fatalf("terminal_read(newest page): %q", newest.Content)
	}
	for _, fragment := range []string{"line10", "line9", "line8", "[lines: 0-3 of 10]"} {
		if !strings.Contains(newest.Content, fragment) {
			t.Fatalf("newest page missing %q:\n%s", fragment, newest.Content)
		}
	}
	// Next older page: lines 5-7, numbered 3-6.
	older := invokeTerminal(t, tools, terminal.ToolTerminalRead,
		`{"session_id":"`+sessionID+`","offset":3,"count":3}`)
	if older.IsError {
		t.Fatalf("terminal_read(older page): %q", older.Content)
	}
	for _, fragment := range []string{"line7", "line6", "line5", "[lines: 3-6 of 10]"} {
		if !strings.Contains(older.Content, fragment) {
			t.Fatalf("older page missing %q:\n%s", fragment, older.Content)
		}
	}
	if strings.Contains(older.Content, "line10") {
		t.Fatalf("older page leaked newest lines:\n%s", older.Content)
	}
	// An offset past the retained line count is an empty page.
	empty := invokeTerminal(t, tools, terminal.ToolTerminalRead,
		`{"session_id":"`+sessionID+`","offset":99,"count":5}`)
	if empty.IsError || !strings.Contains(empty.Content, "(no retained output)") {
		t.Fatalf("past-end page = %q (IsError=%v)", empty.Content, empty.IsError)
	}
	// Validation errors.
	for _, arguments := range []string{
		`{"session_id":"` + sessionID + `","offset":-1}`,
		`{"session_id":"` + sessionID + `","count":0}`,
	} {
		result := invokeTerminal(t, tools, terminal.ToolTerminalRead, arguments)
		if !result.IsError {
			t.Fatalf("terminal_read(%s) = %q, want an error", arguments, result.Content)
		}
	}
}

func TestTerminalSendSubmitFalse(t *testing.T) {
	_, tools := newTerminalTools(t)
	sessionID := openSession(t, tools, `{}`)
	// submit=false writes the raw text (a trailing newline still executes the
	// line in the pipe shell) and returns after output settles.
	result := invokeTerminal(t, tools, terminal.ToolTerminalSend,
		`{"session_id":"`+sessionID+`","input":"echo no-submit-ok\n","submit":false}`)
	if result.IsError || !strings.Contains(result.Content, "no-submit-ok") {
		t.Fatalf("terminal_send(submit=false) = %q (IsError=%v)", result.Content, result.IsError)
	}
}

// openDSHSession opens one pipe session and returns its id.
func openDSHSession(t *testing.T, tools map[string]rig.Tool) string {
	t.Helper()
	result := invokeTerminal(t, tools, terminal.ToolTerminalOpen, `{"type":"pipe","name":"bg"}`)
	if result.IsError {
		t.Fatalf("terminal_open: %q", result.Content)
	}
	fields := strings.Fields(result.Content)
	if len(fields) < 2 || fields[0] != "session_id:" {
		t.Fatalf("terminal_open output = %q", result.Content)
	}
	return fields[1]
}

func TestTerminalBackgroundSend(t *testing.T) {
	_, tools, background, jobTools := newDSHTerminalTools(t)
	sessionID := openDSHSession(t, tools)
	result := invokeTerminal(t, tools, terminal.ToolTerminalSend,
		`{"sessionId":"`+sessionID+`","text":"sleep 0.2; echo background-send-ok","run_in_background":true}`)
	if result.IsError {
		t.Fatalf("terminal_send(run_in_background) = %q", result.Content)
	}
	if !strings.Contains(result.Content, "started background job pty-send-") {
		t.Fatalf("background send ack = %q", result.Content)
	}
	jobID := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(result.Content), "started background job "))
	output := invokeJobTool(t, jobTools, jobs.ToolJobOutput, `{"job_id":"`+jobID+`","wait":true,"timeout_ms":5000}`)
	if output.IsError {
		t.Fatalf("job_output: %q", output.Content)
	}
	for _, fragment := range []string{"background-send-ok", "state: exited", "status: completed"} {
		if !strings.Contains(output.Content, fragment) {
			t.Fatalf("background send output missing %q:\n%s", fragment, output.Content)
		}
	}
	if writes := background.PendingNotices(); len(writes) != 0 {
		t.Fatalf("awaited background send queued notices: %+v", writes)
	}
	invokeTerminal(t, tools, terminal.ToolTerminalClose, `{"sessionId":"`+sessionID+`"}`)
}

func TestTerminalBackgroundSendKill(t *testing.T) {
	_, tools, _, jobTools := newDSHTerminalTools(t)
	sessionID := openDSHSession(t, tools)
	result := invokeTerminal(t, tools, terminal.ToolTerminalSend,
		`{"sessionId":"`+sessionID+`","text":"sleep 30","run_in_background":true}`)
	if result.IsError {
		t.Fatalf("terminal_send(run_in_background) = %q", result.Content)
	}
	jobID := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(result.Content), "started background job "))
	time.Sleep(100 * time.Millisecond)
	killed := invokeJobTool(t, jobTools, jobs.ToolJobKill, `{"job_id":"`+jobID+`","reason":"no longer needed"}`)
	if killed.IsError {
		t.Fatalf("job_kill: %q", killed.Content)
	}
	for _, fragment := range []string{"state: killed", "no longer needed"} {
		if !strings.Contains(killed.Content, fragment) {
			t.Fatalf("killed background send missing %q:\n%s", fragment, killed.Content)
		}
	}
	// The terminal session may have been interrupted; close it either way.
	invokeTerminal(t, tools, terminal.ToolTerminalClose, `{"sessionId":"`+sessionID+`"}`)
}

func TestTerminalBackgroundSendWithoutJobsManager(t *testing.T) {
	_, tools := newTerminalTools(t)
	sessionID := openSession(t, tools, `{}`)
	result := invokeTerminal(t, tools, terminal.ToolTerminalSend,
		`{"session_id":"`+sessionID+`","input":"echo hi","run_in_background":true}`)
	if !result.IsError || !strings.Contains(result.Content, "background sends unavailable") {
		t.Fatalf("background send without manager = %q (IsError=%v)", result.Content, result.IsError)
	}
}
