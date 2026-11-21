package surface_test

import (
	"context"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/surface"
)

type namedTool struct{ name string }

func (t *namedTool) Name() string           { return t.name }
func (t *namedTool) Description() string    { return t.name }
func (t *namedTool) Schema() map[string]any { return map[string]any{} }
func (t *namedTool) Invoke(context.Context, rig.ToolCall) (rig.ToolResult, error) {
	return rig.ToolResult{}, nil
}

func tools(names ...string) []rig.Tool {
	out := make([]rig.Tool, 0, len(names))
	for _, name := range names {
		out = append(out, &namedTool{name: name})
	}
	return out
}

func namesOf(tools []rig.Tool) []string { return surface.Names(tools) }

func TestWithoutNamesPreservesOrder(t *testing.T) {
	got := namesOf(surface.WithoutNames(tools("a", "b", "c"), []string{" b ", ""}))
	if len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Fatalf("WithoutNames = %v", got)
	}
	// An empty exclusion list returns the same slice untouched.
	input := tools("a")
	if out := surface.WithoutNames(input, nil); len(out) != 1 || out[0].Name() != "a" {
		t.Fatalf("WithoutNames(nil) = %v", namesOf(out))
	}
}

func TestOnlyNamesKeepsAllowAndKeepOrder(t *testing.T) {
	got := namesOf(surface.OnlyNames(tools("a", "b", "c"), []string{"b"}, "c"))
	if len(got) != 2 || got[0] != "b" || got[1] != "c" {
		t.Fatalf("OnlyNames = %v, want registration order b,c", got)
	}
	input := tools("a", "b")
	if out := surface.OnlyNames(input, nil); len(out) != 2 {
		t.Fatalf("OnlyNames(nil) = %v", namesOf(out))
	}
}

func TestNamesHandlesNilAndWhitespace(t *testing.T) {
	input := []rig.Tool{nil, &namedTool{name: " a "}, &namedTool{name: ""}}
	if got := surface.Names(input); len(got) != 1 || got[0] != "a" {
		t.Fatalf("Names = %v", got)
	}
	if set := surface.NameSet(input); !set["a"] || len(set) != 1 {
		t.Fatalf("NameSet = %v", set)
	}
}
