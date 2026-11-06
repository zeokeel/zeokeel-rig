package tools_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools"
)

// fakeTool is a recording rig.Tool for registry tests.
type fakeTool struct {
	name        string
	description string
	schema      map[string]any
	invoke      func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error)
	calls       atomic.Int64
}

func (f *fakeTool) Name() string           { return f.name }
func (f *fakeTool) Description() string    { return f.description }
func (f *fakeTool) Schema() map[string]any { return f.schema }
func (f *fakeTool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	f.calls.Add(1)
	if f.invoke != nil {
		return f.invoke(ctx, call)
	}
	return rig.ToolResult{Content: "ok"}, nil
}

func TestRegistryRegisterAndSnapshots(t *testing.T) {
	zeta := &fakeTool{name: "zeta", description: "the zeta tool", schema: map[string]any{"type": "object"}}
	alpha := &fakeTool{name: "alpha", description: "the alpha tool", schema: map[string]any{"type": "object"}}
	registry := tools.NewRegistry(zeta, alpha)

	if got := registry.Len(); got != 2 {
		t.Fatalf("Len = %d, want 2", got)
	}
	tools := registry.Tools()
	if len(tools) != 2 || tools[0].Name() != "alpha" || tools[1].Name() != "zeta" {
		t.Fatalf("Tools() = %v, want sorted [alpha zeta]", names(tools))
	}
	specs := registry.Specs()
	if len(specs) != 2 || specs[0].Name != "alpha" || specs[1].Name != "zeta" {
		t.Fatalf("Specs() = %+v, want sorted [alpha zeta]", specs)
	}
	if specs[0].Description != "the alpha tool" || specs[0].Parameters == nil {
		t.Fatalf("spec of alpha = %+v, want description and schema", specs[0])
	}

	// Snapshots: mutating the returned slices must not affect the registry.
	tools[0] = nil
	_ = registry.Tools()[0]

	// A rig.ToolLister alias sees the same tools.
	var lister rig.ToolLister = registry
	if got := len(lister.Tools()); got != 2 {
		t.Fatalf("ToolLister.Tools() length = %d, want 2", got)
	}
}

func names(tools []rig.Tool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name())
	}
	return out
}

func TestRegistryRegisterErrors(t *testing.T) {
	registry := tools.NewRegistry()
	if err := registry.Register(nil); err == nil {
		t.Fatal("Register(nil) returned nil error")
	}
	var nilPointer *fakeTool
	if err := registry.Register(nilPointer); err == nil {
		t.Fatal("Register(typed nil) returned nil error")
	}
	if err := registry.Register(&fakeTool{name: "  "}); err == nil {
		t.Fatal("Register(blank name) returned nil error")
	}
	if err := registry.Register(&fakeTool{name: "a"}); err != nil {
		t.Fatalf("Register(a): %v", err)
	}
	if err := registry.Register(&fakeTool{name: "a"}); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate Register error = %v, want duplicate error", err)
	}
	if got := registry.Len(); got != 1 {
		t.Fatalf("Len after failed registers = %d, want 1", got)
	}
}

func TestRegistryNewRegistryPanicsOnInvalidTool(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		tools []rig.Tool
	}{
		{"nil tool", []rig.Tool{nil}},
		{"blank name", []rig.Tool{&fakeTool{name: ""}}},
		{"duplicate", []rig.Tool{&fakeTool{name: "x"}, &fakeTool{name: "x"}}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("NewRegistry did not panic for a programming error")
				}
			}()
			tools.NewRegistry(testCase.tools...)
		})
	}
}

func TestRegistryInvokePassesCallThroughUntouched(t *testing.T) {
	type ctxKey struct{}
	var gotCall rig.ToolCall
	var gotValue any
	echo := &fakeTool{
		name: "echo",
		invoke: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			gotCall = call
			gotValue = ctx.Value(ctxKey{})
			return rig.ToolResult{Content: "args=" + call.Arguments}, nil
		},
	}
	registry := tools.NewRegistry(echo)

	// The arguments string must reach the tool byte-for-byte, including
	// whitespace and duplicate keys (the registry never re-encodes it).
	raw := `{ "b": 2, "a":1, "a":  3 }`
	call := rig.ToolCall{ID: "call-1", Name: "echo", Arguments: raw}
	ctx := context.WithValue(context.Background(), ctxKey{}, "ctx-value")
	result, err := registry.Invoke(ctx, call)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if gotCall != call {
		t.Fatalf("tool got call %+v, want %+v", gotCall, call)
	}
	if gotValue != "ctx-value" {
		t.Fatalf("tool got ctx value %v, want ctx-value", gotValue)
	}
	if result.Content != "args="+raw || result.IsError {
		t.Fatalf("result = %+v, want the tool's own result", result)
	}
}

func TestRegistryInvokeUnknownTool(t *testing.T) {
	registry := tools.NewRegistry(&fakeTool{name: "known"})
	result, err := registry.Invoke(context.Background(), rig.ToolCall{Name: "missing"})
	if err == nil {
		t.Fatal("Invoke(unknown) returned nil error")
	}
	if !errors.Is(err, rig.ErrToolNotFound) {
		t.Fatalf("error = %v, want errors.Is ErrToolNotFound", err)
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Fatalf("error = %v, want the tool name in the message", err)
	}
	if result.Content != "" || result.IsError || len(result.Parts) != 0 {
		t.Fatalf("result = %+v, want zero result", result)
	}
}

func TestRegistryToolErrorsAndResultsPassThrough(t *testing.T) {
	registry := tools.NewRegistry(
		&fakeTool{name: "fails", invoke: func(context.Context, rig.ToolCall) (rig.ToolResult, error) {
			return rig.ToolResult{}, fmt.Errorf("boom")
		}},
		&fakeTool{name: "text", invoke: func(context.Context, rig.ToolCall) (rig.ToolResult, error) {
			return rig.ToolResult{Content: "bad input", IsError: true}, nil
		}},
	)
	if _, err := registry.Invoke(context.Background(), rig.ToolCall{Name: "fails"}); err == nil || err.Error() != "boom" {
		t.Fatalf("tool error = %v, want boom", err)
	}
	result, err := registry.Invoke(context.Background(), rig.ToolCall{Name: "text"})
	if err != nil {
		t.Fatalf("Invoke(text): %v", err)
	}
	if !result.IsError || result.Content != "bad input" {
		t.Fatalf("result = %+v, want the tool's IsError result", result)
	}
}

func TestRegistryConcurrentUse(t *testing.T) {
	registry := tools.NewRegistry()
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := fmt.Sprintf("tool-%d", worker)
			if err := registry.Register(&fakeTool{name: name}); err != nil {
				t.Errorf("Register(%s): %v", name, err)
				return
			}
			for call := 0; call < 20; call++ {
				_ = registry.Len()
				_ = registry.Tools()
				_ = registry.Specs()
				if _, err := registry.Invoke(context.Background(), rig.ToolCall{Name: name}); err != nil {
					t.Errorf("Invoke(%s): %v", name, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if got := registry.Len(); got != 8 {
		t.Fatalf("Len after concurrent registers = %d, want 8", got)
	}
}
