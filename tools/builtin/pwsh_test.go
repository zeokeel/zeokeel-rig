package builtin_test

// Tests for the pwsh tool. A temporary executable shell script stands in for
// PowerShell: it records its argv and then behaves as the test needs, so the
// tests are deterministic and need no PowerShell installation.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/builtin"
)

// requireUnixish skips the fake-binary tests on Windows: the fake pwsh is a
// /bin/sh script.
func requireUnixish(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake pwsh executable is a Unix shell script")
	}
}

// fakePwsh writes an executable script that records its arguments to
// "<script>.args" and then runs body.
func fakePwsh(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pwsh-fake")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$0.args\"\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake pwsh: %v", err)
	}
	return path
}

// fakePwshArgs reads the arguments the fake binary received.
func fakePwshArgs(t *testing.T, script string) []string {
	t.Helper()
	data, err := os.ReadFile(script + ".args")
	if err != nil {
		t.Fatalf("read fake pwsh args: %v", err)
	}
	trimmed := strings.TrimRight(string(data), "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

func invokePwsh(t *testing.T, tool rig.Tool, arguments string) rig.ToolResult {
	t.Helper()
	result, err := tool.Invoke(context.Background(), rig.ToolCall{ID: "call-1", Name: tool.Name(), Arguments: arguments})
	if err != nil {
		t.Fatalf("pwsh.Invoke returned Go error: %v", err)
	}
	return result
}

func invokePwshError(t *testing.T, tool rig.Tool, arguments string) string {
	t.Helper()
	result := invokePwsh(t, tool, arguments)
	if !result.IsError {
		t.Fatalf("pwsh: result = %q, want IsError", result.Content)
	}
	return result.Content
}

func TestPwshPassesPowerShellFlagsAndScript(t *testing.T) {
	requireUnixish(t)
	script := fakePwsh(t, `echo "pwsh-output"`)
	tool := builtin.NewPwshWithConfig(builtin.PwshConfig{Binary: script, Root: t.TempDir()})
	if tool.Name() != builtin.ToolPwsh {
		t.Fatalf("Name = %q, want %q", tool.Name(), builtin.ToolPwsh)
	}

	result := invokePwsh(t, tool, `{"command":"Write-Output 'hello world'"}`)
	if result.IsError || !strings.Contains(result.Content, "pwsh-output") {
		t.Fatalf("pwsh result = %q (IsError=%v)", result.Content, result.IsError)
	}
	args := fakePwshArgs(t, script)
	want := []string{"-NoProfile", "-NonInteractive", "-Command", "Write-Output 'hello world'"}
	if len(args) != len(want) {
		t.Fatalf("pwsh argv = %q, want %q", args, want)
	}
	for index := range want {
		if args[index] != want[index] {
			t.Fatalf("pwsh argv = %q, want %q", args, want)
		}
	}
}

func TestPwshExitCodeAndOutput(t *testing.T) {
	requireUnixish(t)
	script := fakePwsh(t, `echo before; echo err >&2; exit 3`)
	tool := builtin.NewPwshWithConfig(builtin.PwshConfig{Binary: script, Root: t.TempDir()})
	result := invokePwsh(t, tool, `{"command":"exit 3"}`)
	if result.IsError {
		t.Fatalf("pwsh(exit 3) = %q, want non-error result", result.Content)
	}
	requireContains(t, result.Content, "before", "err", "[exit code: 3]")
}

func TestPwshTimeout(t *testing.T) {
	requireUnixish(t)
	script := fakePwsh(t, `sleep 5`)
	// Constructor timeout is the default.
	tool := builtin.NewPwshWithConfig(builtin.PwshConfig{Binary: script, Root: t.TempDir(), Timeout: 50 * time.Millisecond})
	started := time.Now()
	text := invokePwshError(t, tool, `{"command":"sleep"}`)
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("pwsh timeout took %s, want near 50ms", elapsed)
	}
	requireContains(t, text, "timeout")

	// timeout_ms overrides the constructor timeout.
	longDefault := builtin.NewPwshWithConfig(builtin.PwshConfig{Binary: script, Root: t.TempDir(), Timeout: 5 * time.Second})
	started = time.Now()
	text = invokePwshError(t, longDefault, `{"command":"sleep","timeout_ms":50}`)
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("pwsh timeout_ms took %s, want near 50ms", elapsed)
	}
	requireContains(t, text, "timeout")
}

func TestPwshContextCancellation(t *testing.T) {
	requireUnixish(t)
	script := fakePwsh(t, `sleep 5`)
	tool := builtin.NewPwshWithConfig(builtin.PwshConfig{Binary: script, Root: t.TempDir(), Timeout: 5 * time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	_, err := tool.Invoke(ctx, rig.ToolCall{Arguments: `{"command":"sleep"}`})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled pwsh error = %v, want context.Canceled", err)
	}
}

func TestPwshMissingBinary(t *testing.T) {
	requireUnixish(t)
	// An injected binary that does not exist is a readable start failure.
	tool := builtin.NewPwshWithConfig(builtin.PwshConfig{
		Binary: "rig-no-such-pwsh-binary-xyz",
		Root:   t.TempDir(),
	})
	text := invokePwshError(t, tool, `{"command":"Write-Output hi"}`)
	requireContains(t, text, "cannot start", "rig-no-such-pwsh-binary-xyz")

	// Auto-detection with an empty PATH reports the missing installation.
	emptyPath := t.TempDir()
	t.Setenv("PATH", emptyPath)
	auto := builtin.NewPwsh(t.TempDir(), time.Second)
	text = invokePwshError(t, auto, `{"command":"Write-Output hi"}`)
	requireContains(t, text, "PowerShell not found", "pwsh", "powershell")
}

func TestPwshWorkdirLegacy(t *testing.T) {
	requireUnixish(t)
	root := policyRoot(t)
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	script := fakePwsh(t, `pwd`)
	tool := builtin.NewPwshWithConfig(builtin.PwshConfig{Binary: script, Root: root})
	result := invokePwsh(t, tool, `{"command":"Get-Location","workdir":"sub"}`)
	if result.IsError || !strings.HasSuffix(strings.TrimSpace(result.Content), "sub") {
		t.Fatalf("pwd in sub = %q (IsError=%v)", result.Content, result.IsError)
	}
	requireContains(t, invokePwshError(t, tool, `{"command":"Get-Location","workdir":".."}`), "outside")
	requireContains(t, invokePwshError(t, tool, `{"command":"Get-Location","workdir":"missing"}`), "workdir")
}

func TestPwshPolicyCommandAllowlist(t *testing.T) {
	requireUnixish(t)
	root := policyRoot(t)
	script := fakePwsh(t, `echo allowed`)
	allowedPolicy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithCommandAllowlist(filepath.Base(script)),
	)
	tool := builtin.NewPwshWithConfig(builtin.PwshConfig{Binary: script, Policy: allowedPolicy})
	if result := invokePwsh(t, tool, `{"command":"Write-Output allowed"}`); result.IsError || !strings.Contains(result.Content, "allowed") {
		t.Fatalf("allowed pwsh = %q (IsError=%v)", result.Content, result.IsError)
	}

	deniedPolicy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithCommandAllowlist("some-other-binary"),
	)
	denied := builtin.NewPwshWithConfig(builtin.PwshConfig{Binary: script, Policy: deniedPolicy})
	requireContains(t, invokePwshError(t, denied, `{"command":"Write-Output x"}`), "not in the allowlist")

	// No allowlist at all means the shell is disabled.
	noShell := builtin.NewPwshWithConfig(builtin.PwshConfig{
		Binary: script,
		Policy: sandbox.New(sandbox.WithRoots(root)),
	})
	requireContains(t, invokePwshError(t, noShell, `{"command":"Write-Output x"}`), "disabled")

	// The empty default policy has no allowlist, so the shell is disabled; the
	// injected binary keeps this assertion independent of what is installed.
	defaultPolicy := builtin.NewPwshWithConfig(builtin.PwshConfig{Binary: script, Policy: sandbox.New()})
	requireContains(t, invokePwshError(t, defaultPolicy, `{"command":"Write-Output x"}`), "disabled")

	// Read-only policy disables the shell even with an allowlist.
	readOnly := builtin.NewPwshWithConfig(builtin.PwshConfig{
		Binary: script,
		Policy: sandbox.New(
			sandbox.WithRoots(root),
			sandbox.WithCommandAllowlist(filepath.Base(script)),
			sandbox.WithReadOnly(true),
		),
	})
	requireContains(t, invokePwshError(t, readOnly, `{"command":"Write-Output x"}`), "read-only")
}

func TestPwshPolicyEnvWhitelist(t *testing.T) {
	requireUnixish(t)
	root := policyRoot(t)
	t.Setenv("RIG_PWSH_VISIBLE", "visible-value")
	t.Setenv("RIG_PWSH_HIDDEN", "hidden-value")
	script := fakePwsh(t, `echo v=$RIG_PWSH_VISIBLE h=$RIG_PWSH_HIDDEN`)
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithCommandAllowlist(filepath.Base(script)),
		sandbox.WithEnvAllowlist("PATH", "RIG_PWSH_VISIBLE"),
	)
	tool := builtin.NewPwshWithConfig(builtin.PwshConfig{Binary: script, Policy: policy})
	result := invokePwsh(t, tool, `{"command":"Write-Output env"}`)
	if result.IsError {
		t.Fatalf("pwsh env: %q", result.Content)
	}
	requireContains(t, result.Content, "v=visible-value")
	if strings.Contains(result.Content, "hidden-value") || !strings.Contains(result.Content, "h=") {
		t.Fatalf("hidden environment variable leaked: %q", result.Content)
	}
}

func TestPwshPolicyTimeoutCeilingAndOutputCap(t *testing.T) {
	requireUnixish(t)
	root := policyRoot(t)

	timeoutScript := fakePwsh(t, `sleep 5`)
	timeoutPolicy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithCommandAllowlist(filepath.Base(timeoutScript)),
		sandbox.WithEnvAllowlist("PATH"),
		sandbox.WithTimeout(50*time.Millisecond),
	)
	timeoutTool := builtin.NewPwshWithConfig(builtin.PwshConfig{Binary: timeoutScript, Policy: timeoutPolicy})
	started := time.Now()
	text := invokePwshError(t, timeoutTool, `{"command":"sleep"}`)
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("policy timeout took %s, want near 50ms", elapsed)
	}
	requireContains(t, text, "timeout")

	outputScript := fakePwsh(t, `dd if=/dev/zero bs=4096 count=8 2>/dev/null | tr '\0' x`)
	outputPolicy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithCommandAllowlist(filepath.Base(outputScript)),
		sandbox.WithEnvAllowlist("PATH"),
		sandbox.WithMaxOutputBytes(64),
	)
	outputTool := builtin.NewPwshWithConfig(builtin.PwshConfig{Binary: outputScript, Policy: outputPolicy})
	result := invokePwsh(t, outputTool, `{"command":"big output"}`)
	if result.IsError {
		t.Fatalf("pwsh output cap: %q", result.Content)
	}
	if len(result.Content) > 64+256 {
		t.Fatalf("pwsh output length = %d, want <= 64 + notice", len(result.Content))
	}
	requireContains(t, result.Content, "truncated")
}

func TestPwshPolicyWorkdir(t *testing.T) {
	requireUnixish(t)
	root := policyRoot(t)
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	script := fakePwsh(t, `pwd`)
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithCommandAllowlist(filepath.Base(script)),
	)
	tool := builtin.NewPwshWithConfig(builtin.PwshConfig{Binary: script, Policy: policy})
	if result := invokePwsh(t, tool, `{"command":"Get-Location","workdir":"sub"}`); result.IsError {
		t.Fatalf("policy workdir sub = %q", result.Content)
	}
	requireContains(t, invokePwshError(t, tool, `{"command":"Get-Location","workdir":".."}`), "sandbox")
}

func TestPwshSchemaAndMissingCommand(t *testing.T) {
	tool := builtin.NewPwsh(t.TempDir(), time.Second)
	if tool.Description() == "" {
		t.Fatal("pwsh has an empty description")
	}
	schema := tool.Schema()
	required, _ := schema["required"].([]string)
	if len(required) != 1 || required[0] != "command" {
		t.Fatalf("schema required = %v, want [command]", required)
	}
	properties, _ := schema["properties"].(map[string]any)
	for _, name := range []string{"command", "timeout_ms", "workdir"} {
		if _, ok := properties[name]; !ok {
			t.Fatalf("schema is missing %q: %v", name, properties)
		}
	}
	result, err := tool.Invoke(context.Background(), rig.ToolCall{Arguments: `{}`})
	if err != nil {
		t.Fatalf("pwsh({}) error: %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content, "command is required") {
		t.Fatalf("pwsh({}) = %q (IsError=%v)", result.Content, result.IsError)
	}
}

// TestPwshAutoDetectionWhenInstalled runs the real detection path when a
// PowerShell binary happens to be installed; otherwise it skips.
func TestPwshAutoDetectionWhenInstalled(t *testing.T) {
	requireUnixish(t)
	if _, err := exec.LookPath("pwsh"); err != nil {
		if _, err := exec.LookPath("powershell"); err != nil {
			t.Skip("no PowerShell binary on PATH")
		}
	}
	tool := builtin.NewPwsh(t.TempDir(), 20*time.Second)
	result := invokePwsh(t, tool, `{"command":"Write-Output detected"}`)
	if result.IsError || !strings.Contains(result.Content, "detected") {
		t.Fatalf("auto-detected pwsh = %q (IsError=%v)", result.Content, result.IsError)
	}
}
