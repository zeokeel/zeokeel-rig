package builtin_test

// Tests for the built-in tool constructors and their advertised schemas.

import (
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/builtin"
)

func TestNewFSToolSet(t *testing.T) {
	tools := builtin.NewFS(t.TempDir())
	wantNames := []string{
		builtin.ToolReadFile, builtin.ToolWriteFile, builtin.ToolEditFile,
		builtin.ToolListDir, builtin.ToolGlob, builtin.ToolGrep,
	}
	if len(tools) != len(wantNames) {
		t.Fatalf("NewFS returned %d tools, want %d", len(tools), len(wantNames))
	}
	seen := make(map[string]bool, len(tools))
	for _, tool := range tools {
		seen[tool.Name()] = true
		if tool.Description() == "" {
			t.Errorf("tool %q has an empty description", tool.Name())
		}
		schema := tool.Schema()
		if schema == nil {
			t.Fatalf("tool %q has a nil schema", tool.Name())
		}
		if schema["type"] != "object" {
			t.Errorf("tool %q schema type = %v, want object", tool.Name(), schema["type"])
		}
		if len(schemaProperties(schema)) == 0 && tool.Name() != builtin.ToolBash {
			t.Errorf("tool %q schema has no properties", tool.Name())
		}
	}
	for _, name := range wantNames {
		if !seen[name] {
			t.Errorf("NewFS did not return %q", name)
		}
	}
}

func TestSchemasDeclareRequiredArguments(t *testing.T) {
	tools := builtin.NewBuiltins(t.TempDir(), time.Second)
	byName := make(map[string]rig.Tool, len(tools))
	for _, tool := range tools {
		byName[tool.Name()] = tool
	}
	cases := []struct {
		tool     string
		required []string
	}{
		{builtin.ToolReadFile, []string{"path"}},
		{builtin.ToolWriteFile, []string{"path", "content"}},
		{builtin.ToolEditFile, []string{"path", "old_string", "new_string"}},
		{builtin.ToolListDir, nil},
		{builtin.ToolGlob, []string{"pattern"}},
		{builtin.ToolGrep, []string{"pattern"}},
		{builtin.ToolBash, []string{"command"}},
	}
	for _, testCase := range cases {
		tool, ok := byName[testCase.tool]
		if !ok {
			t.Fatalf("tool %q missing from NewBuiltins", testCase.tool)
		}
		required := schemaRequired(t, tool.Schema())
		if strings.Join(required, ",") != strings.Join(testCase.required, ",") {
			t.Errorf("%s required = %v, want %v", testCase.tool, required, testCase.required)
		}
		for _, name := range testCase.required {
			property, ok := schemaProperties(tool.Schema())[name].(map[string]any)
			if !ok {
				t.Errorf("%s schema has no property object %q", testCase.tool, name)
				continue
			}
			if property["description"] == "" {
				t.Errorf("%s property %q has no description", testCase.tool, name)
			}
		}
	}
}

func TestNewShellAndNewBuiltins(t *testing.T) {
	shell := builtin.NewShell(t.TempDir(), 0)
	if shell.Name() != builtin.ToolBash {
		t.Fatalf("NewShell name = %q, want %q", shell.Name(), builtin.ToolBash)
	}
	if shell.Schema()["type"] != "object" {
		t.Fatalf("bash schema type = %v, want object", shell.Schema()["type"])
	}
	all := builtin.NewBuiltins(t.TempDir(), time.Second)
	if len(all) != 7 {
		t.Fatalf("NewBuiltins returned %d tools, want 7 (6 fs + bash)", len(all))
	}
}
