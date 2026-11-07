//go:build linux

package sandbox_test

import (
	"context"
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
	"github.com/zeokeel/zeokeel-rig/tools/full"
)

func requireBwrap(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bubblewrap (bwrap) not installed")
	}
}

func TestOSExecWrapArgv(t *testing.T) {
	root := t.TempDir()
	osExec, err := sandbox.NewOSExec(sandbox.OSExecOptions{Root: root})
	if err != nil {
		t.Fatalf("NewOSExec: %v", err)
	}
	if osExec.Backend() != "bwrap" {
		t.Fatalf("backend = %q, want bwrap", osExec.Backend())
	}
	cmd, err := osExec.CommandContext(context.Background(), root, "/bin/sh", "-c", "echo hi")
	if err != nil {
		t.Fatalf("CommandContext: %v", err)
	}
	if !strings.HasSuffix(cmd.Path, "bwrap") {
		t.Fatalf("cmd.Path = %q, want bwrap", cmd.Path)
	}
	joined := strings.Join(cmd.Args, " ")
	for _, want := range []string{"--unshare-all", "--ro-bind / /", "--tmpfs /tmp", "--chdir " + root, "-- /bin/sh -c echo hi"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv missing %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "--share-net") {
		t.Errorf("network should be unshared by default: %s", joined)
	}

	networked, err := sandbox.NewOSExec(sandbox.OSExecOptions{Root: root, AllowNetwork: true})
	if err != nil {
		t.Fatalf("NewOSExec(network): %v", err)
	}
	cmd, err = networked.CommandContext(context.Background(), root, "/bin/sh", "-c", "echo hi")
	if err != nil {
		t.Fatalf("CommandContext(network): %v", err)
	}
	if !strings.Contains(strings.Join(cmd.Args, " "), "--share-net") {
		t.Errorf("AllowNetwork did not add --share-net: %v", cmd.Args)
	}

	bad, err := sandbox.NewOSExec(sandbox.OSExecOptions{Root: root, Backend: "nope"})
	if err == nil || bad != nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("NewOSExec(bad backend) = %v, %v; want unsupported error", bad, err)
	}
}

// TestOSExecEnforcesFilesystemAndNetwork runs real commands through the
// bubblewrap backend (integration test; skipped when bwrap is unavailable).
func TestOSExecEnforcesFilesystemAndNetwork(t *testing.T) {
	requireBwrap(t)
	root := t.TempDir()
	osExec, err := sandbox.NewOSExec(sandbox.OSExecOptions{Root: root})
	if err != nil {
		t.Fatalf("NewOSExec: %v", err)
	}
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithCommandAllowlist("sh", "id", "touch", "cat", "echo"),
		sandbox.WithOSExec(osExec),
	)
	tools := builtin.NewWithPolicy(policy)
	var bash rig.Tool
	for _, tool := range tools {
		if tool.Name() == "bash" {
			bash = tool
		}
	}
	run := func(command string) rig.ToolResult {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		result, err := bash.Invoke(ctx, rig.ToolCall{Name: "bash", Arguments: fmt.Sprintf(`{"command":%q}`, command)})
		if err != nil {
			t.Fatalf("bash.Invoke(%q): %v", command, err)
		}
		return result
	}

	if result := run("id"); result.IsError {
		t.Fatalf("id inside sandbox failed: %s", result.Content)
	}
	inside := filepath.Join(root, "inside.txt")
	if result := run("touch " + inside); result.IsError {
		t.Fatalf("write inside workspace failed: %s", result.Content)
	}
	if _, err := os.Stat(inside); err != nil {
		t.Fatalf("file inside workspace missing: %v", err)
	}
	outside := "/etc/rig-bwrap-outside-" + time.Now().Format("150405.000000")
	if result := run("touch " + outside); !result.IsError {
		t.Fatalf("write outside workspace unexpectedly succeeded: %s", result.Content)
	}
	if _, err := os.Stat(outside); err == nil {
		_ = os.Remove(outside)
		t.Fatalf("sandboxed command wrote %s outside the workspace", outside)
	}
	// Reads outside the writable root stay available (ro-bind /): the policy,
	// not the OS sandbox, decides which paths the model may target.
	if result := run("cat /etc/hostname"); result.IsError {
		t.Fatalf("read outside workspace failed: %s", result.Content)
	}
	// With --unshare-all the sandbox has only a loopback network namespace.
	hostNet, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		t.Fatalf("read host /proc/net/dev: %v", err)
	}
	if !strings.Contains(string(hostNet), "eth0") && !strings.Contains(string(hostNet), "ens") {
		t.Skip("host has no conventional ethernet device; cannot assert network isolation this way")
	}
	if result := run("cat /proc/net/dev"); result.IsError || strings.Contains(result.Content, "eth0") || strings.Contains(result.Content, "ens") {
		t.Fatalf("network namespace not isolated (isError=%v): %s", result.IsError, result.Content)
	}
}

// TestOSExecFullToolSurface constructs the complete 74-tool surface with an
// OS-sandboxed policy, mirroring kit/full wiring and catching tools that
// assume they can spawn a process outside the sandbox.
func TestOSExecFullToolSurface(t *testing.T) {
	requireBwrap(t)
	root := t.TempDir()
	osExec, err := sandbox.NewOSExec(sandbox.OSExecOptions{Root: root})
	if err != nil {
		t.Fatalf("NewOSExec: %v", err)
	}
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithCommandAllowlist("sh", "echo"),
		sandbox.WithOSExec(osExec),
	)
	all := full.All(full.AllOptions{Policy: policy, Root: root})
	if len(all) != 74 {
		t.Fatalf("full.All returned %d tools, want 74", len(all))
	}
	for _, tool := range all {
		if tool.Name() == "" || tool.Description() == "" {
			t.Errorf("tool with empty name/description: %+v", tool)
		}
	}
}
