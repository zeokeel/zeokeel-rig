package discovery_test

import (
	"context"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/discovery"
)

type surfaceTool struct{ name string }

func (t *surfaceTool) Name() string           { return t.name }
func (t *surfaceTool) Description() string    { return t.name }
func (t *surfaceTool) Schema() map[string]any { return map[string]any{} }
func (t *surfaceTool) Invoke(context.Context, rig.ToolCall) (rig.ToolResult, error) {
	return rig.ToolResult{}, nil
}

func TestActiveNamesHidesTailAndKeepsForced(t *testing.T) {
	tools := []rig.Tool{
		&surfaceTool{name: "read_file"},
		&surfaceTool{name: "enter_worktree"},
		&surfaceTool{name: "mcp__srv__echo"},
		&surfaceTool{name: "custom_tool"},
	}
	activation := discovery.Activation{
		Tail:         []string{"enter_worktree"},
		TailPrefixes: []string{"mcp__"},
		Forced:       []string{"enter_worktree"},
	}
	got := discovery.ActiveNames(tools, activation)
	want := []string{"read_file", "enter_worktree", "custom_tool"}
	if len(got) != len(want) {
		t.Fatalf("ActiveNames = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("ActiveNames = %v, want %v", got, want)
		}
	}
	// Forcing a tool that is not registered changes nothing.
	leaky := discovery.Activation{Tail: []string{"read_file"}, Forced: []string{"missing"}}
	if !leaky.Hidden("read_file") {
		t.Fatal("forced unknown name leaked into the active set")
	}
}

func TestActivationHiddenDefaults(t *testing.T) {
	if (discovery.Activation{}).Hidden("anything") {
		t.Fatal("empty activation hides tools")
	}
	trimmed := discovery.Activation{Tail: []string{" undo "}}
	if !trimmed.Hidden("undo") {
		t.Fatal("tail entry not trimmed")
	}
	if (discovery.Activation{TailPrefixes: []string{"mcp__"}}).Hidden("mcp_tool") {
		t.Fatal("prefix matched a non-prefixed name")
	}
}
