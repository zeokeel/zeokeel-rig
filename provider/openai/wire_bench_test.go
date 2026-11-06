package openai

// Benchmarks for the request-side wire conversion: mapping a rig.Request to the
// OpenAI-compatible chat-completions JSON body. This path runs on every model
// call, so its allocations matter. No network is used; New only validates the
// base URL and reads no credentials when Config.APIKey is set.

import (
	"testing"

	"github.com/zeokeel/zeokeel-rig"
)

// benchSink keeps the benchmark results observable so the compiler cannot
// eliminate the measured work.
var benchSink any

// benchProvider builds a deterministic provider for benchmarks.
func benchProvider(b *testing.B) *Provider {
	b.Helper()
	provider, err := New(Config{
		BaseURL:      "http://127.0.0.1:1/v1",
		APIKey:       "bench-key",
		DefaultModel: "bench-model",
	})
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	return provider
}

// benchRequest is a representative conversation: system prompt, user text with
// an image, an assistant tool call, its tool result, and two tool specs.
func benchRequest() rig.Request {
	temperature := 0.7
	topP := 0.9
	return rig.Request{
		Messages: []rig.Message{
			rig.NewMessage(rig.RoleSystem, "You are a careful coding agent."),
			{Role: rig.RoleUser, Content: []rig.ContentPart{
				rig.TextPart("What does this screenshot show?"),
				rig.ImagePart("https://example.com/screenshot.png", ""),
			}},
			{
				Role: rig.RoleAssistant,
				ToolCalls: []rig.ToolCall{
					{ID: "call_1", Name: "read_file", Arguments: `{"path":"main.go"}`},
					{ID: "call_2", Name: "grep", Arguments: "{}"},
				},
			},
			{Role: rig.RoleTool, ToolCallID: "call_1", Name: "read_file", Content: []rig.ContentPart{rig.TextPart("package main")}},
			{Role: rig.RoleTool, ToolCallID: "call_2", Name: "grep", Content: []rig.ContentPart{rig.TextPart("no matches")}},
		},
		Tools: []rig.ToolSpec{
			{Name: "read_file", Description: "Read a file", Parameters: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}},
			{Name: "grep", Description: "Search files", Parameters: map[string]any{"type": "object", "properties": map[string]any{"pattern": map[string]any{"type": "string"}}}},
		},
		Temperature: &temperature,
		TopP:        &topP,
		MaxTokens:   4096,
	}
}

// BenchmarkBuildRequest measures the plain request conversion (no Extra).
func BenchmarkBuildRequest(b *testing.B) {
	provider := benchProvider(b)
	req := benchRequest()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		raw, err := provider.buildRequest(req)
		if err != nil {
			b.Fatalf("buildRequest: %v", err)
		}
		if len(raw) == 0 {
			b.Fatal("buildRequest returned an empty body")
		}
		benchSink = raw
	}
}

// BenchmarkBuildRequestWithExtra measures the marshal/merge path used when a
// caller passes provider-specific fields (Extra is re-encoded through a map).
func BenchmarkBuildRequestWithExtra(b *testing.B) {
	provider := benchProvider(b)
	req := benchRequest()
	req.Extra = map[string]any{"reasoning_effort": "high", "model": "must-not-override"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		raw, err := provider.buildRequest(req)
		if err != nil {
			b.Fatalf("buildRequest: %v", err)
		}
		benchSink = raw
	}
}

// BenchmarkWireContentText measures the common text-only content conversion.
func BenchmarkWireContentText(b *testing.B) {
	message := rig.NewMessage(rig.RoleUser, "Explain this function in one paragraph.")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSink = wireContent(message)
	}
}

// BenchmarkWireContentImage measures the multimodal array conversion, which
// allocates the part slice and per-part pointers.
func BenchmarkWireContentImage(b *testing.B) {
	message := rig.Message{Role: rig.RoleUser, Content: []rig.ContentPart{
		rig.TextPart("Compare these two screenshots."),
		rig.ImagePart("https://example.com/a.png", "high"),
		rig.TextPart("The second one is after the fix."),
		rig.ImagePart("data:image/png;base64,iVBORw0KGgo=", ""),
	}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSink = wireContent(message)
	}
}
