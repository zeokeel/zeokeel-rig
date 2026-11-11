package builtin_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	rig "github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/builtin"
)

// TestFileToolsFollowWorkingDirectory pins the shared-cwd contract: the file
// tools resolve relative paths from the session cwd (misc.WorkingDirectory),
// not always from the sandbox root.
func TestFileToolsFollowWorkingDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	cwd := "."
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithReadOnly(false))
	tools := builtin.NewWithPolicyOptions(policy, builtin.WithWorkingDirectory(func() string { return cwd }))
	byName := make(map[string]rig.Tool, len(tools))
	for _, tool := range tools {
		byName[tool.Name()] = tool
	}
	invoke := func(name, args string) rig.ToolResult {
		t.Helper()
		result, err := byName[name].Invoke(context.Background(), rig.ToolCall{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return result
	}

	cwd = "sub"
	result := invoke("write_file", `{"path":"nested/note.txt","content":"hello cwd\n"}`)
	if result.IsError {
		t.Fatalf("write_file = %q", result.Content)
	}
	data, err := os.ReadFile(filepath.Join(root, "sub", "nested", "note.txt"))
	if err != nil || string(data) != "hello cwd\n" {
		t.Fatalf("file under cwd = %q, %v; want hello cwd", data, err)
	}

	result = invoke("list_dir", `{"path":"."}`)
	if result.IsError || !strings.Contains(result.Content, "nested") {
		t.Fatalf("list_dir under cwd = %q (error=%v), want nested", result.Content, result.IsError)
	}
	result = invoke("grep", `{"pattern":"hello cwd"}`)
	if result.IsError || !strings.Contains(result.Content, "nested/note.txt") {
		t.Fatalf("grep under cwd = %q (error=%v), want nested/note.txt", result.Content, result.IsError)
	}

	// Absolute paths stay absolute (policy-contained), not cwd-relative.
	result = invoke("write_file", `{"path":"`+filepath.ToSlash(filepath.Join(root, "top.txt"))+`","content":"top"}`)
	if result.IsError {
		t.Fatalf("absolute write_file = %q", result.Content)
	}
	if _, err := os.Stat(filepath.Join(root, "top.txt")); err != nil {
		t.Fatalf("absolute path did not land at root: %v", err)
	}
}
