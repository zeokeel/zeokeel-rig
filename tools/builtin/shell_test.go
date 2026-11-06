package builtin_test

// Tests for the built-in shell tool: merged output, exit codes, workdir
// confinement, timeouts, context cancellation and output truncation.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
	"github.com/zeokeel/zeokeel-rig/tools/builtin"
)

func newBashForTest(t *testing.T, timeout time.Duration) (string, v1.Tool) {
	t.Helper()
	root := t.TempDir()
	return root, builtin.NewShell(root, timeout)
}

func runBash(t *testing.T, tool v1.Tool, arguments string) v1.ToolResult {
	t.Helper()
	result, err := tool.Invoke(context.Background(), v1.ToolCall{ID: "bash-1", Name: builtin.ToolBash, Arguments: arguments})
	if err != nil {
		t.Fatalf("bash.Invoke returned Go error: %v", err)
	}
	return result
}

func TestBashBasicOutputAndExitCode(t *testing.T) {
	_, bash := newBashForTest(t, time.Second)

	result := runBash(t, bash, `{"command":"echo hello"}`)
	if result.IsError {
		t.Fatalf("bash(echo) = %q, want success", result.Content)
	}
	if strings.TrimSpace(result.Content) != "hello" {
		t.Fatalf("bash(echo) content = %q, want hello", result.Content)
	}

	// stdout and stderr are merged into one stream.
	result = runBash(t, bash, `{"command":"echo out; echo err >&2"}`)
	if result.IsError {
		t.Fatalf("bash(stderr) = %q, want success", result.Content)
	}
	requireContains(t, result.Content, "out", "err")

	// A non-zero exit is a tool error and reports the exit code.
	result = runBash(t, bash, `{"command":"echo before; exit 3"}`)
	if !result.IsError {
		t.Fatalf("bash(exit 3) = %q, want IsError", result.Content)
	}
	requireContains(t, result.Content, "before", "[exit code: 3]")

	// A command that does not exist exits 127.
	result = runBash(t, bash, `{"command":"definitely-not-a-rig-command-xyz"}`)
	if !result.IsError {
		t.Fatalf("bash(unknown command) = %q, want IsError", result.Content)
	}
	requireContains(t, result.Content, "[exit code: 127]")

	// Missing command argument is a model-facing error.
	result = runBash(t, bash, `{}`)
	if !result.IsError || !strings.Contains(result.Content, "command is required") {
		t.Fatalf("bash({}) = %q (IsError=%v)", result.Content, result.IsError)
	}
}

func TestBashWorkdirIsConfinedToRoot(t *testing.T) {
	root, bash := newBashForTest(t, time.Second)
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	physicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks(root): %v", err)
	}

	result := runBash(t, bash, `{"command":"pwd","workdir":"sub"}`)
	if result.IsError {
		t.Fatalf("bash(pwd, workdir=sub) = %q", result.Content)
	}
	if got := strings.TrimSpace(result.Content); got != filepath.Join(physicalRoot, "sub") {
		t.Fatalf("pwd = %q, want %q", got, filepath.Join(physicalRoot, "sub"))
	}

	for _, arguments := range []string{
		`{"command":"pwd","workdir":".."}`,
		`{"command":"pwd","workdir":"/etc"}`,
		`{"command":"pwd","workdir":"missing"}`,
	} {
		result = runBash(t, bash, arguments)
		if !result.IsError {
			t.Fatalf("bash(%s) = %q, want IsError", arguments, result.Content)
		}
	}
}

func TestBashTimeout(t *testing.T) {
	// The constructor timeout is the default.
	_, bash := newBashForTest(t, 50*time.Millisecond)
	started := time.Now()
	result := runBash(t, bash, `{"command":"sleep 5"}`)
	elapsed := time.Since(started)
	if !result.IsError || !strings.Contains(result.Content, "timeout") {
		t.Fatalf("bash(timeout) = %q (IsError=%v), want a timeout error", result.Content, result.IsError)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("timeout took %s, want it to fire near 50ms", elapsed)
	}

	// timeout_ms overrides the constructor default.
	_, longDefault := newBashForTest(t, 5*time.Second)
	started = time.Now()
	result = runBash(t, longDefault, `{"command":"sleep 5","timeout_ms":50}`)
	if !result.IsError || !strings.Contains(result.Content, "timeout") || time.Since(started) > 3*time.Second {
		t.Fatalf("bash(timeout_ms=50) = %q after %s, want a fast timeout", result.Content, time.Since(started))
	}
}

func TestBashContextCancellationIsAnError(t *testing.T) {
	_, bash := newBashForTest(t, 5*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	_, err := bash.Invoke(ctx, v1.ToolCall{Name: builtin.ToolBash, Arguments: `{"command":"sleep 5"}`})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("bash(cancelled ctx) error = %v, want context.Canceled", err)
	}
}

func TestBashOutputIsTruncated(t *testing.T) {
	_, bash := newBashForTest(t, 5*time.Second)
	// 64 KiB of "x" through the shell's stdout.
	result := runBash(t, bash, `{"command":"dd if=/dev/zero bs=4096 count=16 2>/dev/null | tr '\\0' x"}`)
	if result.IsError {
		t.Fatalf("bash(big output) = %q, want success", result.Content)
	}
	if len(result.Content) > builtin.MaxShellOutputBytes+256 {
		t.Fatalf("bash output length = %d, want <= %d + notice", len(result.Content), builtin.MaxShellOutputBytes)
	}
	requireContains(t, result.Content, "truncated", "output")
}

func TestBashIsConcurrent(t *testing.T) {
	_, bash := newBashForTest(t, 5*time.Second)
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for call := 0; call < 3; call++ {
				result, err := bash.Invoke(context.Background(), v1.ToolCall{
					Name: builtin.ToolBash, Arguments: `{"command":"echo concurrent"}`,
				})
				if err != nil || result.IsError || !strings.Contains(result.Content, "concurrent") {
					t.Errorf("concurrent bash: result=%q err=%v", result.Content, err)
					return
				}
			}
		}()
	}
	wg.Wait()
}
