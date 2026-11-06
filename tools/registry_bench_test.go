package tools_test

// Benchmarks for the tool registry hot path: construction, the model-facing
// spec snapshot, and name-based dispatch. All tools are in-process fakes, so
// the benchmarks are deterministic and never touch the network or filesystem.

import (
	"context"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools"
)

// benchSink keeps measured values observable so the compiler cannot eliminate
// the benchmarked work.
var benchSink any

// benchRegistryTool is a minimal in-memory rig.Tool.
type benchRegistryTool struct {
	name   string
	schema map[string]any
}

func (t benchRegistryTool) Name() string        { return t.name }
func (t benchRegistryTool) Description() string { return "benchmark tool " + t.name }
func (t benchRegistryTool) Schema() map[string]any {
	return t.schema
}
func (t benchRegistryTool) Invoke(_ context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	return rig.ToolResult{Content: call.Name + ":" + call.Arguments}, nil
}

// benchRegistryTools returns a fixed set of eight tools, a realistic
// model-facing tool count for an agent run.
func benchRegistryTools() []rig.Tool {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "path to act on"},
		},
		"required": []string{"path"},
	}
	list := make([]rig.Tool, 0, 8)
	for _, name := range []string{"read_file", "write_file", "list_dir", "glob", "grep", "bash", "web_search", "web_fetch"} {
		list = append(list, benchRegistryTool{name: name, schema: schema})
	}
	return list
}

// BenchmarkNewRegistry measures construction of an eight-tool registry.
func BenchmarkNewRegistry(b *testing.B) {
	list := benchRegistryTools()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		registry := tools.NewRegistry(list...)
		if registry.Len() != len(list) {
			b.Fatalf("Len() = %d, want %d", registry.Len(), len(list))
		}
		benchSink = registry
	}
}

// BenchmarkRegistrySpecs measures one model-facing spec snapshot of eight tools.
func BenchmarkRegistrySpecs(b *testing.B) {
	registry := tools.NewRegistry(benchRegistryTools()...)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		specs := registry.Specs()
		if len(specs) != 8 {
			b.Fatalf("Specs() returned %d entries, want 8", len(specs))
		}
		benchSink = specs
	}
}

// BenchmarkRegistryInvoke measures one dispatch through the read lock to the
// tool body (the hot path of a single tool call).
func BenchmarkRegistryInvoke(b *testing.B) {
	registry := tools.NewRegistry(benchRegistryTools()...)
	ctx := context.Background()
	call := rig.ToolCall{Name: "grep", Arguments: `{"path":"src"}`}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result, err := registry.Invoke(ctx, call)
		if err != nil {
			b.Fatalf("Invoke: %v", err)
		}
		if result.Content == "" {
			b.Fatal("Invoke returned an empty result")
		}
		benchSink = result
	}
}

// BenchmarkRegistryTools measures the sorted snapshot used by the tool-lister
// API (and by Specs internally).
func BenchmarkRegistryTools(b *testing.B) {
	registry := tools.NewRegistry(benchRegistryTools()...)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		list := registry.Tools()
		if len(list) != 8 {
			b.Fatalf("Tools() returned %d entries, want 8", len(list))
		}
		benchSink = list
	}
}
