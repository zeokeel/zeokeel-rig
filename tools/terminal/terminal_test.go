package terminal_test

// Tests for the stateful terminal tool set: persistent shell sessions,
// marker-based completion, signal/close lifecycle, sandbox policy integration,
// timeouts, truncation and concurrency.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/terminal"
)

func newTerminalTools(t *testing.T, opts ...terminal.Option) (*terminal.Manager, map[string]v1.Tool) {
	t.Helper()
	manager := terminal.NewManager(opts...)
	t.Cleanup(func() { _ = manager.CloseAll() })
	byName := make(map[string]v1.Tool)
	for _, tool := range manager.Tools() {
		byName[tool.Name()] = tool
	}
	return manager, byName
}

func invokeTerminal(t *testing.T, tools map[string]v1.Tool, name, arguments string) v1.ToolResult {
	t.Helper()
	tool, ok := tools[name]
	if !ok {
		t.Fatalf("tool %q not found", name)
	}
	result, err := tool.Invoke(context.Background(), v1.ToolCall{ID: "call-1", Name: name, Arguments: arguments})
	if err != nil {
		t.Fatalf("%s.Invoke returned Go error: %v", name, err)
	}
	return result
}

func invokeTerminalError(t *testing.T, tools map[string]v1.Tool, name, arguments string) string {
	t.Helper()
	result := invokeTerminal(t, tools, name, arguments)
	if !result.IsError {
		t.Fatalf("%s: result = %q, want IsError", name, result.Content)
	}
	return result.Content
}

// openSession opens a terminal and returns its session id.
func openSession(t *testing.T, tools map[string]v1.Tool, arguments string) string {
	t.Helper()
	result := invokeTerminal(t, tools, terminal.ToolTerminalOpen, arguments)
	if result.IsError {
		t.Fatalf("terminal_open: %q", result.Content)
	}
	fields := strings.Fields(result.Content)
	if len(fields) < 2 || fields[0] != "session_id:" {
		t.Fatalf("terminal_open output = %q", result.Content)
	}
	return fields[1]
}

func sendCommand(t *testing.T, tools map[string]v1.Tool, sessionID, input string) string {
	t.Helper()
	arguments := `{"session_id":"` + sessionID + `","input":"` + jsonEscape(input) + `"}`
	result := invokeTerminal(t, tools, terminal.ToolTerminalSend, arguments)
	if result.IsError {
		t.Fatalf("terminal_send(%q): %q", input, result.Content)
	}
	return result.Content
}

func jsonEscape(input string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`)
	return replacer.Replace(input)
}

func TestToolsShape(t *testing.T) {
	_, tools := newTerminalTools(t)
	if len(tools) != 5 {
		t.Fatalf("terminal tools = %d, want 5", len(tools))
	}
	for _, name := range []string{
		terminal.ToolTerminalOpen, terminal.ToolTerminalSend, terminal.ToolTerminalRead,
		terminal.ToolTerminalSignal, terminal.ToolTerminalClose,
	} {
		tool, ok := tools[name]
		if !ok {
			t.Fatalf("missing tool %q", name)
		}
		if tool.Description() == "" || tool.Schema() == nil {
			t.Fatalf("tool %q has empty description/schema", name)
		}
		if tool.Schema()["type"] != "object" {
			t.Fatalf("tool %q schema type = %v", name, tool.Schema()["type"])
		}
	}
}

func TestOpenSendCloseEcho(t *testing.T) {
	manager, tools := newTerminalTools(t)
	sessionID := openSession(t, tools, `{}`)
	if !strings.HasPrefix(sessionID, "term-") {
		t.Fatalf("session id = %q", sessionID)
	}
	if manager.Len() != 1 {
		t.Fatalf("manager.Len = %d, want 1", manager.Len())
	}

	if output := sendCommand(t, tools, sessionID, "echo hello"); !strings.Contains(output, "hello") {
		t.Fatalf("echo output = %q", output)
	}
	// stdout and stderr are merged.
	output := sendCommand(t, tools, sessionID, "echo out; echo err >&2")
	if !strings.Contains(output, "out") || !strings.Contains(output, "err") {
		t.Fatalf("merged output = %q", output)
	}

	result := invokeTerminal(t, tools, terminal.ToolTerminalClose, `{"session_id":"`+sessionID+`"}`)
	if result.IsError || !strings.Contains(result.Content, "closed") {
		t.Fatalf("terminal_close = %q (IsError=%v)", result.Content, result.IsError)
	}
	if manager.Len() != 0 {
		t.Fatalf("manager.Len after close = %d, want 0", manager.Len())
	}
}

func TestMultipleCommandsKeepOrder(t *testing.T) {
	_, tools := newTerminalTools(t)
	sessionID := openSession(t, tools, `{}`)
	for index, text := range []string{"one", "two", "three"} {
		output := sendCommand(t, tools, sessionID, "echo marker-"+text)
		if !strings.Contains(output, "marker-"+text) {
			t.Fatalf("command %d output = %q", index, output)
		}
	}
	// Nothing is left for terminal_read after sends drained the output.
	read := invokeTerminal(t, tools, terminal.ToolTerminalRead, `{"session_id":"`+sessionID+`"}`)
	if read.IsError || strings.TrimSpace(read.Content) != "" {
		t.Fatalf("terminal_read after sends = %q (IsError=%v), want empty", read.Content, read.IsError)
	}
}

func TestCwd(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	_, tools := newTerminalTools(t)
	sessionID := openSession(t, tools, `{"cwd":"`+jsonEscape(sub)+`"}`)
	output := sendCommand(t, tools, sessionID, "pwd")
	if !strings.Contains(output, sub) {
		t.Fatalf("pwd = %q, want %q", output, sub)
	}
	// An initial directory that does not exist is a tool error.
	result := invokeTerminal(t, tools, terminal.ToolTerminalOpen, `{"cwd":"`+jsonEscape(filepath.Join(root, "missing"))+`"}`)
	if !result.IsError {
		t.Fatalf("terminal_open(missing cwd) = %q, want IsError", result.Content)
	}
}

func TestStderrAndExitInsideShell(t *testing.T) {
	_, tools := newTerminalTools(t)
	sessionID := openSession(t, tools, `{}`)
	output := sendCommand(t, tools, sessionID, "false; echo after-false")
	if !strings.Contains(output, "after-false") {
		t.Fatalf("output = %q", output)
	}
	// A body-level `exit` terminates the shell; the pending marker never runs.
	output = sendCommand(t, tools, sessionID, "exit 3")
	if !strings.Contains(output, "shell exited") || !strings.Contains(output, "3") {
		t.Fatalf("exit output = %q", output)
	}
	result := invokeTerminal(t, tools, terminal.ToolTerminalSend, `{"session_id":"`+sessionID+`","input":"echo hi"}`)
	if !result.IsError || !strings.Contains(result.Content, "exited") {
		t.Fatalf("send after shell exit = %q (IsError=%v)", result.Content, result.IsError)
	}
}

func TestSendTimeout(t *testing.T) {
	_, tools := newTerminalTools(t, terminal.WithTimeout(2*time.Second))
	sessionID := openSession(t, tools, `{}`)
	started := time.Now()
	result := invokeTerminal(t, tools, terminal.ToolTerminalSend,
		`{"session_id":"`+sessionID+`","input":"sleep 5","timeout_ms":200}`)
	elapsed := time.Since(started)
	if !result.IsError || !strings.Contains(result.Content, "timeout") {
		t.Fatalf("timeout result = %q (IsError=%v)", result.Content, result.IsError)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("timeout took %s, want it near 200ms", elapsed)
	}
	if !strings.Contains(result.Content, "desynchronized") {
		t.Fatalf("timeout result missing desync guidance: %q", result.Content)
	}
	// A desynchronized session refuses further commands; close it to clean up.
	text := invokeTerminalError(t, tools, terminal.ToolTerminalSend,
		`{"session_id":"`+sessionID+`","input":"echo alive"}`)
	if !strings.Contains(text, "desynchronized") {
		t.Fatalf("send after timeout = %q", text)
	}
	closeResult := invokeTerminal(t, tools, terminal.ToolTerminalClose, `{"session_id":"`+sessionID+`"}`)
	if closeResult.IsError {
		t.Fatalf("close after timeout: %q", closeResult.Content)
	}
}

func TestMarkerIsolation(t *testing.T) {
	_, tools := newTerminalTools(t)
	sessionID := openSession(t, tools, `{}`)
	// Command output that contains the marker prefix must not fool the
	// completion scanner (the real marker is random per send).
	output := sendCommand(t, tools, sessionID, `echo "__RIG_DONE_fake__"`)
	if !strings.Contains(output, "__RIG_DONE_fake__") {
		t.Fatalf("output = %q", output)
	}
	output = sendCommand(t, tools, sessionID, "echo still-working")
	if !strings.Contains(output, "still-working") {
		t.Fatalf("output after fake marker = %q", output)
	}
}

func TestSignalInterruptsSleep(t *testing.T) {
	_, tools := newTerminalTools(t, terminal.WithTimeout(10*time.Second))
	sessionID := openSession(t, tools, `{}`)

	type sendResult struct {
		result v1.ToolResult
		err    error
	}
	done := make(chan sendResult, 1)
	go func() {
		tool := tools[terminal.ToolTerminalSend]
		result, err := tool.Invoke(context.Background(), v1.ToolCall{
			Arguments: `{"session_id":"` + sessionID + `","input":"sleep 5"}`,
		})
		done <- sendResult{result: result, err: err}
	}()
	time.Sleep(150 * time.Millisecond)
	signalResult := invokeTerminal(t, tools, terminal.ToolTerminalSignal,
		`{"session_id":"`+sessionID+`","signal":"INT"}`)
	if signalResult.IsError {
		t.Fatalf("terminal_signal: %q", signalResult.Content)
	}
	select {
	case completed := <-done:
		if completed.err != nil {
			t.Fatalf("send after SIGINT: %v", completed.err)
		}
		_ = completed.result
	case <-time.After(3 * time.Second):
		t.Fatal("terminal_send did not return after SIGINT")
	}
	// Depending on the /bin/sh implementation a group SIGINT also terminates
	// the shell itself, so accept either "session still usable" or a clean
	// "shell has exited" error.
	result := invokeTerminal(t, tools, terminal.ToolTerminalSend,
		`{"session_id":"`+sessionID+`","input":"echo after-signal"}`)
	if result.IsError {
		if !strings.Contains(result.Content, "exited") && !strings.Contains(result.Content, "closed") {
			t.Fatalf("send after signal = %q", result.Content)
		}
	} else if !strings.Contains(result.Content, "after-signal") {
		t.Fatalf("session after signal output = %q", result.Content)
	}
}

func TestReadWaitForOutput(t *testing.T) {
	_, tools := newTerminalTools(t, terminal.WithTimeout(5*time.Second))
	sessionID := openSession(t, tools, `{}`)

	done := make(chan v1.ToolResult, 1)
	go func() {
		result, err := tools[terminal.ToolTerminalSend].Invoke(context.Background(), v1.ToolCall{
			Arguments: `{"session_id":"` + sessionID + `","input":"sleep 0.2; echo later"}`,
		})
		if err != nil {
			done <- v1.ToolResult{Content: err.Error(), IsError: true}
			return
		}
		done <- result
	}()
	result := invokeTerminal(t, tools, terminal.ToolTerminalRead,
		`{"session_id":"`+sessionID+`","wait_ms":2000}`)
	if result.IsError || !strings.Contains(result.Content, "later") {
		t.Fatalf("terminal_read(wait) = %q (IsError=%v)", result.Content, result.IsError)
	}
	select {
	case sent := <-done:
		if sent.IsError {
			t.Fatalf("send = %q", sent.Content)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("send did not finish")
	}
	// After all output is consumed, a non-waiting read returns empty.
	empty := invokeTerminal(t, tools, terminal.ToolTerminalRead, `{"session_id":"`+sessionID+`"}`)
	if empty.IsError || strings.TrimSpace(empty.Content) != "" {
		t.Fatalf("empty read = %q", empty.Content)
	}
}

func TestOutputTruncation(t *testing.T) {
	_, tools := newTerminalTools(t, terminal.WithMaxOutputBytes(64))
	sessionID := openSession(t, tools, `{}`)
	command := "i=0; while [ $i -lt 100 ]; do echo line$i; i=$((i+1)); done"
	result := invokeTerminal(t, tools, terminal.ToolTerminalSend,
		`{"session_id":"`+sessionID+`","input":"`+jsonEscape(command)+`"}`)
	if result.IsError {
		t.Fatalf("send: %q", result.Content)
	}
	if !strings.Contains(result.Content, "truncated") {
		t.Fatalf("output was not truncated: %q", result.Content)
	}
	if len(result.Content) > 64+200 {
		t.Fatalf("output length = %d, want <= 64 + notice", len(result.Content))
	}
}

func TestUnknownSessions(t *testing.T) {
	_, tools := newTerminalTools(t)
	for name, arguments := range map[string]string{
		terminal.ToolTerminalSend:   `{"session_id":"nope","input":"echo hi"}`,
		terminal.ToolTerminalRead:   `{"session_id":"nope"}`,
		terminal.ToolTerminalSignal: `{"session_id":"nope","signal":"INT"}`,
		terminal.ToolTerminalClose:  `{"session_id":"nope"}`,
	} {
		text := invokeTerminalError(t, tools, name, arguments)
		if !strings.Contains(text, "unknown session") {
			t.Fatalf("%s unknown session error = %q", name, text)
		}
	}
}

func TestCloseAllAndStaleSession(t *testing.T) {
	manager, tools := newTerminalTools(t)
	first := openSession(t, tools, `{}`)
	second := openSession(t, tools, `{}`)
	if manager.Len() != 2 {
		t.Fatalf("Len = %d, want 2", manager.Len())
	}
	if err := manager.CloseAll(); err != nil {
		t.Fatalf("CloseAll: %v", err)
	}
	if manager.Len() != 0 {
		t.Fatalf("Len after CloseAll = %d", manager.Len())
	}
	for _, sessionID := range []string{first, second} {
		text := invokeTerminalError(t, tools, terminal.ToolTerminalSend,
			`{"session_id":"`+sessionID+`","input":"echo hi"}`)
		if !strings.Contains(text, "unknown session") {
			t.Fatalf("stale session error = %q", text)
		}
	}
}

func TestConcurrentSessions(t *testing.T) {
	manager, tools := newTerminalTools(t)
	var wg sync.WaitGroup
	ids := make([]string, 4)
	errs := make([]error, 4)
	for index := 0; index < 4; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			result, err := tools[terminal.ToolTerminalOpen].Invoke(context.Background(), v1.ToolCall{Arguments: `{}`})
			if err != nil {
				errs[index] = err
				return
			}
			if result.IsError {
				errs[index] = nil
				ids[index] = result.Content
				return
			}
			fields := strings.Fields(result.Content)
			if len(fields) >= 2 {
				ids[index] = fields[1]
			}
		}(index)
	}
	wg.Wait()
	for index, err := range errs {
		if err != nil {
			t.Fatalf("open %d: %v", index, err)
		}
	}
	for index := range ids {
		if !strings.HasPrefix(ids[index], "term-") {
			t.Fatalf("session %d id = %q", index, ids[index])
		}
	}
	if manager.Len() != 4 {
		t.Fatalf("Len = %d, want 4", manager.Len())
	}
	for index, sessionID := range ids {
		output := sendCommand(t, tools, sessionID, "echo session-"+string(rune('a'+index)))
		if !strings.Contains(output, "session-"+string(rune('a'+index))) {
			t.Fatalf("session %d output = %q", index, output)
		}
	}
	for _, sessionID := range ids {
		invokeTerminal(t, tools, terminal.ToolTerminalClose, `{"session_id":"`+sessionID+`"}`)
	}
}

func TestConcurrentSendsSameSession(t *testing.T) {
	_, tools := newTerminalTools(t, terminal.WithTimeout(5*time.Second))
	sessionID := openSession(t, tools, `{}`)
	var wg sync.WaitGroup
	outputs := make([]v1.ToolResult, 2)
	errs := make([]error, 2)
	for index, text := range []string{"alpha", "beta"} {
		wg.Add(1)
		go func(index int, text string) {
			defer wg.Done()
			result, err := tools[terminal.ToolTerminalSend].Invoke(context.Background(), v1.ToolCall{
				Arguments: `{"session_id":"` + sessionID + `","input":"echo ` + text + `"}`,
			})
			outputs[index], errs[index] = result, err
		}(index, text)
	}
	wg.Wait()
	for index, err := range errs {
		if err != nil {
			t.Fatalf("send %d: %v", index, err)
		}
		if outputs[index].IsError {
			t.Fatalf("send %d result = %q", index, outputs[index].Content)
		}
	}
	for index, text := range []string{"alpha", "beta"} {
		if !strings.Contains(outputs[index].Content, text) {
			t.Fatalf("concurrent send %d output = %q", index, outputs[index].Content)
		}
	}
}

func TestPolicyReadOnlyDisablesTerminal(t *testing.T) {
	root := t.TempDir()
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	policy := sandbox.New(sandbox.WithRoots(realRoot), sandbox.WithReadOnly(true))
	_, tools := newTerminalTools(t, terminal.WithPolicy(policy))
	text := invokeTerminalError(t, tools, terminal.ToolTerminalOpen, `{}`)
	if !strings.Contains(text, "read-only") {
		t.Fatalf("read-only open error = %q", text)
	}
}

func TestPolicyCommandAllowlist(t *testing.T) {
	root := t.TempDir()
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	policy := sandbox.New(sandbox.WithRoots(realRoot), sandbox.WithCommandAllowlist("echo"))
	_, tools := newTerminalTools(t, terminal.WithPolicy(policy))
	sessionID := openSession(t, tools, `{}`)
	if output := sendCommand(t, tools, sessionID, "echo allowed"); !strings.Contains(output, "allowed") {
		t.Fatalf("allowed command = %q", output)
	}
	text := invokeTerminalError(t, tools, terminal.ToolTerminalSend,
		`{"session_id":"`+sessionID+`","input":"ls /"}`)
	if !strings.Contains(text, "not in the allowlist") {
		t.Fatalf("denied command error = %q", text)
	}
}

func TestPolicyEnvWhitelist(t *testing.T) {
	root := t.TempDir()
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	t.Setenv("RIG_TERM_VISIBLE", "visible-value")
	t.Setenv("RIG_TERM_HIDDEN", "hidden-value")
	policy := sandbox.New(
		sandbox.WithRoots(realRoot),
		sandbox.WithCommandAllowlist("echo"),
		sandbox.WithEnvAllowlist("RIG_TERM_VISIBLE"),
	)
	_, tools := newTerminalTools(t, terminal.WithPolicy(policy))
	sessionID := openSession(t, tools, `{}`)
	output := sendCommand(t, tools, sessionID, `echo "v=$RIG_TERM_VISIBLE h=$RIG_TERM_HIDDEN"`)
	if !strings.Contains(output, "v=visible-value") {
		t.Fatalf("allowed env missing: %q", output)
	}
	if strings.Contains(output, "hidden-value") || !strings.Contains(output, "h=") {
		t.Fatalf("hidden env leaked: %q", output)
	}
}

func TestPolicyCwdOutsideRoot(t *testing.T) {
	root := t.TempDir()
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	policy := sandbox.New(sandbox.WithRoots(realRoot))
	_, tools := newTerminalTools(t, terminal.WithPolicy(policy))
	text := invokeTerminalError(t, tools, terminal.ToolTerminalOpen, `{"cwd":".."}`)
	if !strings.Contains(text, "sandbox: denied") {
		t.Fatalf("outside cwd error = %q", text)
	}
}
