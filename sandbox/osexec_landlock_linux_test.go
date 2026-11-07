//go:build linux

package sandbox_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/builtin"
	"github.com/zeokeel/zeokeel-rig/tools/terminal"
)

func newNativeOSExec(t *testing.T, allowNetwork bool) (*sandbox.OSExec, string) {
	t.Helper()
	root := t.TempDir()
	osExec, err := sandbox.NewOSExec(sandbox.OSExecOptions{Root: root, AllowNetwork: allowNetwork})
	if err != nil {
		if errors.Is(err, sandbox.ErrOSExecUnavailable) {
			t.Skipf("native OS sandbox unavailable: %v", err)
		}
		t.Fatalf("NewOSExec: %v", err)
	}
	if osExec.Backend() != "landlock" {
		t.Fatalf("backend = %q, want landlock", osExec.Backend())
	}
	return osExec, root
}

func nativePolicy(osExec *sandbox.OSExec, root string, allow ...string) *sandbox.Policy {
	return sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithCommandAllowlist(allow...),
		sandbox.WithOSExec(osExec),
	)
}

func nativeBash(t *testing.T, policy *sandbox.Policy) rig.Tool {
	t.Helper()
	for _, tool := range builtin.NewWithPolicy(policy) {
		if tool.Name() == "bash" {
			return tool
		}
	}
	t.Fatal("bash tool not found")
	return nil
}

func invokeBash(t *testing.T, bash rig.Tool, command string) rig.ToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := bash.Invoke(ctx, rig.ToolCall{Name: "bash", Arguments: fmt.Sprintf(`{"command":%q}`, command)})
	if err != nil {
		t.Fatalf("bash.Invoke(%q): %v", command, err)
	}
	return result
}

func TestNativeOSExecWrapArgv(t *testing.T) {
	osExec, root := newNativeOSExec(t, false)
	cmd, err := osExec.CommandContext(context.Background(), root, "/bin/sh", "-c", "echo hi")
	if err != nil {
		t.Fatalf("CommandContext: %v", err)
	}
	if cmd.Path == "" || !strings.Contains(strings.Join(cmd.Args, " "), "__rig_os_sandbox_helper__") {
		t.Fatalf("native command is not a helper re-exec: path=%q args=%v", cmd.Path, cmd.Args)
	}
	if _, err := osExec.CommandContext(context.Background(), "/", "/bin/sh", "-c", "echo hi"); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("workdir outside writable roots: err=%v", err)
	}
}

func TestNativeOSExecFilesystem(t *testing.T) {
	osExec, root := newNativeOSExec(t, false)
	bash := nativeBash(t, nativePolicy(osExec, root, "id", "touch", "cat"))
	if result := invokeBash(t, bash, "id"); result.IsError {
		t.Fatalf("id inside native sandbox failed: %s", result.Content)
	}
	inside := filepath.Join(root, "inside.txt")
	if result := invokeBash(t, bash, "touch "+inside); result.IsError {
		t.Fatalf("write inside workspace failed: %s", result.Content)
	}
	if _, err := os.Stat(inside); err != nil {
		t.Fatalf("file inside workspace missing: %v", err)
	}
	outside := "/etc/rig-native-outside-" + time.Now().Format("150405.000000")
	if result := invokeBash(t, bash, "touch "+outside); !result.IsError {
		t.Fatalf("write outside workspace unexpectedly succeeded: %s", result.Content)
	}
	if _, err := os.Stat(outside); err == nil {
		_ = os.Remove(outside)
		t.Fatalf("sandboxed command wrote %s outside the workspace", outside)
	}
	// Reads outside the writable root remain available (parity with ro-bind /).
	if result := invokeBash(t, bash, "cat /etc/hostname"); result.IsError {
		t.Fatalf("read outside workspace failed: %s", result.Content)
	}
}

func TestNativeOSExecSeccompNetwork(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available for the seccomp probe")
	}
	probe := `python3 -B -c 'import socket; socket.socket()'`

	off, root := newNativeOSExec(t, false)
	offBash := nativeBash(t, nativePolicy(off, root, "python3", "sh"))
	if result := invokeBash(t, offBash, probe); !result.IsError {
		t.Fatalf("socket() unexpectedly succeeded with network disabled: %s", result.Content)
	}

	on, rootOn := newNativeOSExec(t, true)
	onBash := nativeBash(t, nativePolicy(on, rootOn, "python3", "sh"))
	if result := invokeBash(t, onBash, probe); result.IsError {
		t.Fatalf("socket() failed with network enabled: %s", result.Content)
	}
}

func TestNativeOSExecPTY(t *testing.T) {
	osExec, root := newNativeOSExec(t, true)
	manager := terminal.NewManager(
		terminal.WithPolicy(nativePolicy(osExec, root, "bash", "echo")),
		terminal.WithTransport(terminal.TransportPTY),
	)
	t.Cleanup(func() { _ = manager.CloseAll() })
	tools := make(map[string]rig.Tool)
	for _, tool := range manager.Tools() {
		tools[tool.Name()] = tool
	}
	call := func(name, arguments string) rig.ToolResult {
		t.Helper()
		result, err := tools[name].Invoke(context.Background(), rig.ToolCall{Name: name, Arguments: arguments})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return result
	}
	open := call("terminal_open", fmt.Sprintf(`{"cwd":%q,"shell":"/bin/bash"}`, root))
	if open.IsError {
		t.Fatalf("terminal_open: %s", open.Content)
	}
	id := ""
	for _, line := range strings.Split(open.Content, "\n") {
		if value, found := strings.CutPrefix(line, "session_id: "); found {
			id = strings.TrimSpace(value)
		}
	}
	if id == "" {
		t.Fatalf("no session id in %q", open.Content)
	}
	if result := call("terminal_send", fmt.Sprintf(`{"session_id":%q,"input":"echo native-pty-ok"}`, id)); result.IsError || !strings.Contains(result.Content, "native-pty-ok") {
		t.Fatalf("terminal_send under native sandbox: %q", result.Content)
	}
	_ = call("terminal_close", fmt.Sprintf(`{"session_id":%q}`, id))
}
