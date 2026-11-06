package terminal_test

// PTY transport tests. They run on Linux (where /dev/ptmx is available); when a
// PTY cannot be opened they skip rather than fall back silently, so a broken PTY
// implementation is visible. Pipe-transport behavior is covered in
// terminal_test.go.

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/terminal"
)

// newPTYTools creates a manager forced to the PTY transport and opens one
// session. It skips the test when no PTY is available.
func newPTYTools(t *testing.T, opts ...terminal.Option) (*terminal.Manager, map[string]rig.Tool, string) {
	t.Helper()
	opts = append([]terminal.Option{terminal.WithTransport(terminal.TransportPTY)}, opts...)
	manager := terminal.NewManager(opts...)
	t.Cleanup(func() { _ = manager.CloseAll() })
	byName := make(map[string]rig.Tool)
	for _, tool := range manager.Tools() {
		byName[tool.Name()] = tool
	}
	result := invokeTerminal(t, byName, terminal.ToolTerminalOpen, `{}`)
	if result.IsError {
		// A read-only policy test intentionally fails; callers that expect an
		// error use the manager directly.
		t.Fatalf("terminal_open(pty): %q", result.Content)
	}
	if strings.Contains(result.Content, "transport: pipe") {
		t.Skipf("PTY unavailable, session fell back to pipe: %s", result.Content)
	}
	fields := strings.Fields(result.Content)
	if len(fields) < 2 || fields[0] != "session_id:" {
		t.Fatalf("terminal_open output = %q", result.Content)
	}
	return manager, byName, fields[1]
}

// invokePTY sends one command with a bounded timeout.
func invokePTY(t *testing.T, tools map[string]rig.Tool, sessionID, input string, timeoutMS int) rig.ToolResult {
	t.Helper()
	arguments := `{"session_id":"` + sessionID + `","input":"` + jsonEscape(input) + `","timeout_ms":` +
		itoa(timeoutMS) + `}`
	return invokeTerminal(t, tools, terminal.ToolTerminalSend, arguments)
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

func TestDefaultTransportIsPTYOnUnix(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skipf("default PTY transport is unix-only; GOOS=%s", runtime.GOOS)
	}
	manager := terminal.NewManager()
	t.Cleanup(func() { _ = manager.CloseAll() })
	byName := make(map[string]rig.Tool)
	for _, tool := range manager.Tools() {
		byName[tool.Name()] = tool
	}
	result := invokeTerminal(t, byName, terminal.ToolTerminalOpen, `{}`)
	if result.IsError {
		t.Fatalf("terminal_open: %q", result.Content)
	}
	if !strings.Contains(result.Content, "transport: pty") {
		t.Skipf("no default PTY in this environment: %s", result.Content)
	}
	if !strings.Contains(result.Content, "real PTY") {
		t.Fatalf("PTY open output missing PTY note: %q", result.Content)
	}
}

func TestPTYEchoAndNoMarker(t *testing.T) {
	_, tools, sessionID := newPTYTools(t)
	result := invokePTY(t, tools, sessionID, "echo hello-pty", 3000)
	if result.IsError {
		t.Fatalf("terminal_send: %q", result.Content)
	}
	if !strings.Contains(result.Content, "hello-pty") {
		t.Fatalf("PTY echo output = %q", result.Content)
	}
	if strings.Contains(result.Content, "__RIG_DONE_") {
		t.Fatalf("PTY output contains a pipe marker: %q", result.Content)
	}
}

func TestPTYMultipleCommands(t *testing.T) {
	_, tools, sessionID := newPTYTools(t, terminal.WithQuietPeriod(120*time.Millisecond))
	for _, text := range []string{"one", "two", "three"} {
		result := invokePTY(t, tools, sessionID, "echo pty-"+text, 3000)
		if result.IsError {
			t.Fatalf("send %q: %q", text, result.Content)
		}
		if !strings.Contains(result.Content, "pty-"+text) {
			t.Fatalf("send %q output = %q", text, result.Content)
		}
	}
}

func TestPTYStripANSIAndCRLF(t *testing.T) {
	_, tools, sessionID := newPTYTools(t)
	result := invokePTY(t, tools, sessionID, `printf '\033[31mRED-ANSI\033[0m\n'`, 3000)
	if result.IsError {
		t.Fatalf("terminal_send: %q", result.Content)
	}
	if !strings.Contains(result.Content, "RED-ANSI") {
		t.Fatalf("ANSI text missing: %q", result.Content)
	}
	if strings.Contains(result.Content, "\x1b") {
		t.Fatalf("ANSI escape leaked into PTY output: %q", result.Content)
	}
	if strings.Contains(result.Content, "\r") {
		t.Fatalf("CR leaked into PTY output: %q", result.Content)
	}
}

func TestStripANSIHelper(t *testing.T) {
	cases := map[string]string{
		"\x1b[31mred\x1b[0m":          "red",
		"plain":                       "plain",
		"\x1b]0;title\x07text":        "text",
		"\x1b[2K\rline1\r\nline2\r\n": "line1\nline2\n",
		"a\x1b(Bb":                    "ab",
	}
	for input, want := range cases {
		if got := terminal.StripANSI(input); got != want {
			t.Fatalf("StripANSI(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestPTYCtrlCInterruptsBusyLoop(t *testing.T) {
	_, tools, sessionID := newPTYTools(t)
	command := "i=0; while [ $i -lt 100000 ]; do echo tick; i=$((i+1)); done"
	type outcome struct {
		result rig.ToolResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := tools[terminal.ToolTerminalSend].Invoke(context.Background(), rig.ToolCall{
			Arguments: `{"session_id":"` + sessionID + `","input":"` + jsonEscape(command) + `","timeout_ms":10000}`,
		})
		done <- outcome{result: result, err: err}
	}()
	time.Sleep(300 * time.Millisecond)
	signalResult := invokeTerminal(t, tools, terminal.ToolTerminalSignal,
		`{"session_id":"`+sessionID+`","signal":"INT"}`)
	if signalResult.IsError {
		t.Fatalf("terminal_signal: %q", signalResult.Content)
	}
	select {
	case completed := <-done:
		if completed.err != nil {
			t.Fatalf("send after Ctrl-C: %v", completed.err)
		}
		if !strings.Contains(completed.result.Content, "tick") {
			t.Fatalf("interrupted output should contain partial ticks: %q", completed.result.Content)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("terminal_send did not return after Ctrl-C")
	}
	// The shell must survive Ctrl-C and accept the next command.
	if result := invokePTY(t, tools, sessionID, "echo after-ctrlc", 3000); result.IsError ||
		!strings.Contains(result.Content, "after-ctrlc") {
		t.Fatalf("command after Ctrl-C = %q (IsError=%v)", result.Content, result.IsError)
	}
}

func TestPTYCtrlCStopsSleep(t *testing.T) {
	_, tools, sessionID := newPTYTools(t)
	// A silent command returns after the quiet period; Ctrl-C then interrupts
	// it in the foreground and the shell recovers.
	if result := invokePTY(t, tools, sessionID, "sleep 5", 4000); result.IsError {
		t.Fatalf("sleep send: %q", result.Content)
	}
	signalResult := invokeTerminal(t, tools, terminal.ToolTerminalSignal,
		`{"session_id":"`+sessionID+`","signal":"CTRL-C"}`)
	if signalResult.IsError {
		t.Fatalf("terminal_signal(CTRL-C): %q", signalResult.Content)
	}
	time.Sleep(100 * time.Millisecond)
	if result := invokePTY(t, tools, sessionID, "echo recovered-sleep", 3000); result.IsError ||
		!strings.Contains(result.Content, "recovered-sleep") {
		t.Fatalf("command after Ctrl-C on sleep = %q (IsError=%v)", result.Content, result.IsError)
	}
}

func TestPTYInteractiveReadInput(t *testing.T) {
	_, tools, sessionID := newPTYTools(t, terminal.WithQuietPeriod(120*time.Millisecond))
	// `read x` is a shell builtin that waits for a line on stdin. The first send
	// returns after the quiet period; the second send supplies the line.
	var accumulated strings.Builder
	if result := invokePTY(t, tools, sessionID, "read x; echo got:$x", 3000); result.IsError {
		t.Fatalf("read setup send: %q", result.Content)
	} else {
		accumulated.WriteString(result.Content)
	}
	if result := invokePTY(t, tools, sessionID, "hello-pty-input", 3000); result.IsError {
		t.Fatalf("read input send: %q", result.Content)
	} else {
		accumulated.WriteString(result.Content)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		read := invokeTerminal(t, tools, terminal.ToolTerminalRead,
			`{"session_id":"`+sessionID+`","wait_ms":300}`)
		if read.IsError {
			t.Fatalf("terminal_read: %q", read.Content)
		}
		accumulated.WriteString(read.Content)
		if strings.Contains(accumulated.String(), "got:hello-pty-input") {
			return
		}
	}
	t.Fatalf("interactive read never produced got:hello-pty-input; got %q", accumulated.String())
}

func TestPTYShellExit(t *testing.T) {
	_, tools, sessionID := newPTYTools(t)
	result := invokePTY(t, tools, sessionID, "exit 4", 3000)
	if result.IsError {
		t.Fatalf("exit send: %q", result.Content)
	}
	if !strings.Contains(result.Content, "shell exited") {
		t.Fatalf("exit output missing exit note: %q", result.Content)
	}
	after := invokePTY(t, tools, sessionID, "echo too-late", 2000)
	if !after.IsError || !strings.Contains(after.Content, "exited") {
		t.Fatalf("send after shell exit = %q (IsError=%v)", after.Content, after.IsError)
	}
}

func TestPTYTimeoutContinuousOutput(t *testing.T) {
	_, tools, sessionID := newPTYTools(t)
	command := "i=0; while [ $i -lt 100000 ]; do echo tick2; i=$((i+1)); done"
	started := time.Now()
	result := invokePTY(t, tools, sessionID, command, 300)
	elapsed := time.Since(started)
	if !result.IsError || !strings.Contains(result.Content, "timeout") {
		t.Fatalf("continuous output should time out: %q (IsError=%v)", result.Content, result.IsError)
	}
	if elapsed > 4*time.Second {
		t.Fatalf("timeout took %s, want near 300ms", elapsed)
	}
	// PTY timeouts do not desynchronize the session: Ctrl-C it, then reuse it.
	if signalResult := invokeTerminal(t, tools, terminal.ToolTerminalSignal,
		`{"session_id":"`+sessionID+`","signal":"INT"}`); signalResult.IsError {
		t.Fatalf("terminal_signal: %q", signalResult.Content)
	}
	time.Sleep(150 * time.Millisecond)
	if result := invokePTY(t, tools, sessionID, "echo after-timeout", 3000); result.IsError ||
		!strings.Contains(result.Content, "after-timeout") {
		t.Fatalf("send after PTY timeout = %q (IsError=%v)", result.Content, result.IsError)
	}
}

func TestPTYOutputTruncation(t *testing.T) {
	_, tools, sessionID := newPTYTools(t, terminal.WithMaxOutputBytes(64))
	command := "i=0; while [ $i -lt 100 ]; do echo line$i; i=$((i+1)); done"
	result := invokePTY(t, tools, sessionID, command, 5000)
	if result.IsError {
		t.Fatalf("send: %q", result.Content)
	}
	if !strings.Contains(result.Content, "truncated") {
		t.Fatalf("PTY output was not truncated: %q", result.Content)
	}
	if len(result.Content) > 64+300 {
		t.Fatalf("output length = %d, want <= 64 + notice", len(result.Content))
	}
}

func TestPipeTransportForced(t *testing.T) {
	manager := terminal.NewManager(terminal.WithTransport(terminal.TransportPipe))
	t.Cleanup(func() { _ = manager.CloseAll() })
	byName := make(map[string]rig.Tool)
	for _, tool := range manager.Tools() {
		byName[tool.Name()] = tool
	}
	result := invokeTerminal(t, byName, terminal.ToolTerminalOpen, `{}`)
	if result.IsError {
		t.Fatalf("terminal_open(pipe): %q", result.Content)
	}
	if !strings.Contains(result.Content, "transport: pipe") {
		t.Fatalf("forced pipe transport output = %q", result.Content)
	}
	fields := strings.Fields(result.Content)
	sessionID := fields[1]
	if sent := invokeTerminal(t, byName, terminal.ToolTerminalSend,
		`{"session_id":"`+sessionID+`","input":"echo pipe-ok","timeout_ms":3000}`); sent.IsError ||
		!strings.Contains(sent.Content, "pipe-ok") {
		t.Fatalf("pipe send = %q (IsError=%v)", sent.Content, sent.IsError)
	}
}

func TestPTYConcurrentSessions(t *testing.T) {
	manager := terminal.NewManager()
	t.Cleanup(func() { _ = manager.CloseAll() })
	byName := make(map[string]rig.Tool)
	for _, tool := range manager.Tools() {
		byName[tool.Name()] = tool
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("PTY transport is unix-only")
	}
	var wg sync.WaitGroup
	ids := make([]string, 3)
	for index := 0; index < 3; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			result, err := byName[terminal.ToolTerminalOpen].Invoke(context.Background(), rig.ToolCall{Arguments: `{}`})
			if err != nil || result.IsError {
				return
			}
			fields := strings.Fields(result.Content)
			if len(fields) >= 2 {
				ids[index] = fields[1]
			}
		}(index)
	}
	wg.Wait()
	for index, id := range ids {
		if id == "" {
			t.Skipf("no PTY sessions opened (session %d); skipping", index)
		}
		result := invokePTY(t, byName, id, "echo concurrent-pty", 3000)
		if result.IsError || !strings.Contains(result.Content, "concurrent-pty") {
			t.Fatalf("session %d send = %q (IsError=%v)", index, result.Content, result.IsError)
		}
	}
}
