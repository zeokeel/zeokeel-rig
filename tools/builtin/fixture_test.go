package builtin_test

// Shared fixtures for the builtin tool tests.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
	"github.com/zeokeel/zeokeel-rig/tools/builtin"
)

// fsFixture is one rooted tool set over a temp directory.
type fsFixture struct {
	root  string
	tools map[string]v1.Tool
}

func newFSFixture(t *testing.T) *fsFixture {
	t.Helper()
	root := t.TempDir()
	tools := builtin.NewFS(root)
	byName := make(map[string]v1.Tool, len(tools))
	for _, tool := range tools {
		byName[tool.Name()] = tool
	}
	return &fsFixture{root: root, tools: byName}
}

// tool returns one tool by name.
func (f *fsFixture) tool(t *testing.T, name string) v1.Tool {
	t.Helper()
	tool, ok := f.tools[name]
	if !ok {
		t.Fatalf("tool %q not found in %v", name, f.toolNames())
	}
	return tool
}

func (f *fsFixture) toolNames() []string {
	names := make([]string, 0, len(f.tools))
	for name := range f.tools {
		names = append(names, name)
	}
	return names
}

// write creates one file under the root (parents included).
func (f *fsFixture) write(t *testing.T, relative, content string) string {
	t.Helper()
	absolute := filepath.Join(f.root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(absolute), 0o755); err != nil {
		t.Fatalf("MkdirAll for %q: %v", relative, err)
	}
	if err := os.WriteFile(absolute, []byte(content), 0o644); err != nil {
		t.Fatalf("write %q: %v", relative, err)
	}
	return absolute
}

// mkdir creates one directory under the root.
func (f *fsFixture) mkdir(t *testing.T, relative string) string {
	t.Helper()
	absolute := filepath.Join(f.root, filepath.FromSlash(relative))
	if err := os.MkdirAll(absolute, 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", relative, err)
	}
	return absolute
}

// invoke runs one tool call and requires no Go error (tool failures must be
// reported as IsError results so the model can see them).
func (f *fsFixture) invoke(t *testing.T, name, arguments string) v1.ToolResult {
	t.Helper()
	result, err := f.tool(t, name).Invoke(context.Background(), v1.ToolCall{
		ID: "call-1", Name: name, Arguments: arguments,
	})
	if err != nil {
		t.Fatalf("%s.Invoke returned Go error: %v", name, err)
	}
	return result
}

// invokeError requires IsError and returns the model-facing text.
func (f *fsFixture) invokeError(t *testing.T, name, arguments string) string {
	t.Helper()
	result := f.invoke(t, name, arguments)
	if !result.IsError {
		t.Fatalf("%s: result = %q, want IsError", name, result.Content)
	}
	return result.Content
}

// requireContains asserts every fragment is in text.
func requireContains(t *testing.T, text string, fragments ...string) {
	t.Helper()
	for _, fragment := range fragments {
		if !strings.Contains(text, fragment) {
			t.Fatalf("text does not contain %q:\n%s", fragment, text)
		}
	}
}

// requireNotContains asserts none of the fragments is in text.
func requireNotContains(t *testing.T, text string, fragments ...string) {
	t.Helper()
	for _, fragment := range fragments {
		if strings.Contains(text, fragment) {
			t.Fatalf("text unexpectedly contains %q:\n%s", fragment, text)
		}
	}
}

// schemaProperties returns the property names of one schema object.
func schemaProperties(schema map[string]any) map[string]any {
	properties, _ := schema["properties"].(map[string]any)
	return properties
}

// schemaRequired returns the required array of one schema object.
func schemaRequired(t *testing.T, schema map[string]any) []string {
	t.Helper()
	raw, ok := schema["required"]
	if !ok {
		return nil
	}
	required, ok := raw.([]string)
	if !ok {
		t.Fatalf("schema required = %T (%v), want []string", raw, raw)
	}
	return required
}
