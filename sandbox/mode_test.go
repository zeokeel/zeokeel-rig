package sandbox_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

func TestPermissionModes(t *testing.T) {
	workspace := t.TempDir()
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "outside.txt")

	readOnly := sandbox.New(
		sandbox.WithRoots(workspace),
		sandbox.WithMode(sandbox.ModeReadOnly),
		sandbox.WithCommandAllowlist("echo"),
	)
	if !readOnly.ReadOnly() || readOnly.Mode() != sandbox.ModeReadOnly {
		t.Fatalf("read-only mode not reported: readOnly=%v mode=%q", readOnly.ReadOnly(), readOnly.Mode())
	}
	if _, err := readOnly.ResolvePath("inside.txt"); err != nil {
		t.Fatalf("read-only ResolvePath inside workspace: %v", err)
	}
	if err := readOnly.CheckCommand([]string{"echo", "hi"}, workspace); err == nil {
		t.Fatal("read-only mode accepted shell execution")
	}

	write := sandbox.New(
		sandbox.WithRoots(workspace),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithCommandAllowlist("echo"),
	)
	if write.Mode() != sandbox.ModeWorkspaceWrite {
		t.Fatalf("mode = %q", write.Mode())
	}
	if err := write.CheckCommand([]string{"echo", "hi"}, workspace); err != nil {
		t.Fatalf("workspace-write allow-listed command: %v", err)
	}
	if err := write.CheckCommand([]string{"curl"}, workspace); err == nil {
		t.Fatal("workspace-write accepted a command outside the allowlist")
	}
	if _, err := write.ResolvePath(outside); err == nil {
		t.Fatal("workspace-write allowed a path outside the workspace")
	}

	full := sandbox.New(
		sandbox.WithRoots(workspace),
		sandbox.WithMode(sandbox.ModeDangerFullAccess),
	)
	if full.Mode() != sandbox.ModeDangerFullAccess {
		t.Fatalf("mode = %q", full.Mode())
	}
	if err := full.CheckCommand([]string{"arbitrary-binary", "--flag"}, outsideDir); err != nil {
		t.Fatalf("danger-full-access rejected a command: %v", err)
	}
	if _, err := full.ResolvePath(outside); err != nil {
		t.Fatalf("danger-full-access ResolvePath outside workspace: %v", err)
	}
	root, relative, abs, _, err := full.OpenPath(outside)
	if err != nil {
		t.Fatalf("danger-full-access OpenPath: %v", err)
	}
	defer root.Close() //nolint:errcheck
	if err := root.WriteFile(relative, []byte("hi"), 0o644); err != nil {
		t.Fatalf("danger-full-access write: %v", err)
	}
	if data, err := os.ReadFile(abs); err != nil || string(data) != "hi" {
		t.Fatalf("danger-full-access wrote wrong content: %q, %v", data, err)
	}
	if err := full.CheckPath("/etc/passwd"); err != nil {
		t.Fatalf("danger-full-access CheckPath: %v", err)
	}

	invalid := sandbox.New(sandbox.WithMode(sandbox.Mode("bogus")))
	if _, err := invalid.ResolvePath("."); err == nil || !strings.Contains(err.Error(), "permission mode") {
		t.Fatalf("invalid mode ResolvePath error = %v", err)
	}
	if err := invalid.CheckCommand([]string{"echo"}, ""); err == nil || !strings.Contains(err.Error(), "permission mode") {
		t.Fatalf("invalid mode CheckCommand error = %v", err)
	}
}
