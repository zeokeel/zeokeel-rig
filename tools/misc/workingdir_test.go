package misc_test

// Tests for the stateful working_directory tool: query/change, sandbox rules,
// explicit error semantics and concurrency.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
	"github.com/zeokeel/zeokeel-rig/tools/misc"
)

func invoke(t *testing.T, tool v1.Tool, arguments string) v1.ToolResult {
	t.Helper()
	result, err := tool.Invoke(context.Background(), v1.ToolCall{ID: "call-1", Name: tool.Name(), Arguments: arguments})
	if err != nil {
		t.Fatalf("%s.Invoke returned Go error: %v", tool.Name(), err)
	}
	return result
}

func invokeError(t *testing.T, tool v1.Tool, arguments string) string {
	t.Helper()
	result := invoke(t, tool, arguments)
	if !result.IsError {
		t.Fatalf("%s: result = %q, want IsError", tool.Name(), result.Content)
	}
	return result.Content
}

func requireContains(t *testing.T, text string, fragments ...string) {
	t.Helper()
	for _, fragment := range fragments {
		if !strings.Contains(text, fragment) {
			t.Fatalf("text does not contain %q:\n%s", fragment, text)
		}
	}
}

// newWorkingDirFixture creates a rooted tree and the tool over it.
func newWorkingDirFixture(t *testing.T) (string, v1.Tool) {
	t.Helper()
	root := t.TempDir()
	for _, directory := range []string{"sub/nested", "other"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(directory)), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", directory, err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	return root, misc.NewWorkingDirectory(root)
}

func TestWorkingDirectoryQueryAndChange(t *testing.T) {
	root, tool := newWorkingDirFixture(t)
	if tool.Name() != "working_directory" {
		t.Fatalf("Name = %q, want working_directory", tool.Name())
	}
	steps := []struct {
		arguments string
		want      string
	}{
		{`{}`, "."},
		{`{"path":"sub"}`, "sub"},
		{`{}`, "sub"},
		{`{"path":"nested"}`, "sub/nested"},
		{`{"path":".."}`, "sub"},
		{`{"path":".."}`, "."},
		{`{}`, "."},
		// An absolute path inside the root is accepted and normalized.
		{fmt.Sprintf(`{"path":%q}`, filepath.Join(root, "other")), "other"},
	}
	for _, step := range steps {
		result := invoke(t, tool, step.arguments)
		if result.IsError {
			t.Fatalf("%s: %q", step.arguments, result.Content)
		}
		if result.Content != step.want {
			t.Fatalf("%s = %q, want %q", step.arguments, result.Content, step.want)
		}
	}
}

func TestWorkingDirectoryErrors(t *testing.T) {
	root, tool := newWorkingDirFixture(t)
	// A directory that escapes the root through "..".
	requireContains(t, invokeError(t, tool, `{"path":".."}`), "outside")
	requireContains(t, invokeError(t, tool, `{"path":"sub/../.."}`), "outside")
	requireContains(t, invokeError(t, tool, `{"path":"/etc"}`), "outside")
	// Missing target directories are refused, not created.
	requireContains(t, invokeError(t, tool, `{"path":"missing"}`), "stat")
	// A file is not a directory.
	requireContains(t, invokeError(t, tool, `{"path":"file.txt"}`), "not a directory")
	// A symlink that points outside the root is refused by os.Root.
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	requireContains(t, invokeError(t, tool, `{"path":"escape"}`), "working_directory:")
	// The failed calls did not change the current directory.
	if result := invoke(t, tool, `{}`); result.Content != "." {
		t.Fatalf("cwd after failed changes = %q, want .", result.Content)
	}
}

func TestWorkingDirectorySchema(t *testing.T) {
	_, tool := newWorkingDirFixture(t)
	if tool.Description() == "" {
		t.Fatal("working_directory has an empty description")
	}
	schema := tool.Schema()
	properties, _ := schema["properties"].(map[string]any)
	if _, ok := properties["path"].(map[string]any); !ok {
		t.Fatalf("schema has no path property: %v", schema)
	}
	if _, required := schema["required"]; required {
		t.Fatalf("path must be optional, schema = %v", schema)
	}
}

func TestWorkingDirectoryConcurrent(t *testing.T) {
	root, tool := newWorkingDirFixture(t)
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			for call := 0; call < 20; call++ {
				target := filepath.Join(root, "sub")
				if (worker+call)%3 == 0 {
					target = filepath.Join(root, "sub", "nested")
				}
				result, err := tool.Invoke(context.Background(), v1.ToolCall{
					Arguments: fmt.Sprintf(`{"path":%q}`, target),
				})
				if err != nil || result.IsError {
					t.Errorf("concurrent change to %q: result=%q err=%v", target, result.Content, err)
					return
				}
				if _, err := tool.Invoke(context.Background(), v1.ToolCall{Arguments: `{}`}); err != nil {
					t.Errorf("concurrent query: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	result := invoke(t, tool, `{}`)
	if result.IsError || (result.Content != "sub" && result.Content != "sub/nested") {
		t.Fatalf("final cwd = %q (IsError=%v), want sub or sub/nested", result.Content, result.IsError)
	}
	// An absolute path to the root resets the directory.
	if result := invoke(t, tool, fmt.Sprintf(`{"path":%q}`, root)); result.Content != "." || result.IsError {
		t.Fatalf("reset to root = %q (IsError=%v), want .", result.Content, result.IsError)
	}
}
