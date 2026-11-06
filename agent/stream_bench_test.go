package agent_test

// Benchmarks for the agent loop's streaming/round bookkeeping. The Provider is
// an in-memory scripted fake, so the benchmarks are deterministic, run offline,
// and measure exactly what the loop adds on top of a provider: chunk handling,
// event emission, tool-call assembly and the tool round.

import (
	"context"
	"io"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/agent"
)

// benchSink keeps measured values observable so the compiler cannot eliminate
// the benchmarked work.
var benchSink any

// benchStream replays a fixed chunk script, then reports io.EOF.
type benchStream struct {
	chunks []rig.Chunk
	next   int
}

func (s *benchStream) Recv() (rig.Chunk, error) {
	if s.next >= len(s.chunks) {
		return rig.Chunk{}, io.EOF
	}
	chunk := s.chunks[s.next]
	s.next++
	return chunk, nil
}

// benchTextProvider answers every request with one short streamed completion
// carrying text, reasoning, usage and a finish reason.
type benchTextProvider struct{}

func (benchTextProvider) Name() string { return "bench-text" }

func (benchTextProvider) Stream(_ context.Context, _ rig.Request) (rig.Stream, error) {
	return &benchStream{chunks: []rig.Chunk{
		{Reasoning: "thinking"},
		{Delta: "The answer "},
		{Delta: "is 42."},
		{Usage: &rig.Usage{PromptTokens: 11, CompletionTokens: 7, TotalTokens: 18}},
		{FinishReason: "stop"},
	}}, nil
}

// benchToolProvider requests one tool call first and, once the tool result is
// part of the conversation, returns the final answer.
type benchToolProvider struct{}

func (benchToolProvider) Name() string { return "bench-tools" }

func (benchToolProvider) Stream(_ context.Context, req rig.Request) (rig.Stream, error) {
	for _, message := range req.Messages {
		if message.Role == rig.RoleTool {
			return &benchStream{chunks: []rig.Chunk{
				{Delta: "Done."},
				{FinishReason: "stop"},
			}}, nil
		}
	}
	return &benchStream{chunks: []rig.Chunk{
		{Delta: "Checking."},
		{ToolCalls: []rig.ToolCallDelta{
			{Index: 0, ID: "call_1", Name: "bench_tool", Arguments: `{"path":"main.go"}`},
		}},
		{FinishReason: "tool_calls"},
	}}, nil
}

// benchTool is a minimal in-memory tool for the tool-round benchmark.
type benchTool struct{}

func (benchTool) Name() string           { return "bench_tool" }
func (benchTool) Description() string    { return "benchmark tool" }
func (benchTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (benchTool) Invoke(context.Context, rig.ToolCall) (rig.ToolResult, error) {
	return rig.ToolResult{Content: "ok"}, nil
}

// newBenchAgent builds a deterministic benchmark agent.
func newBenchAgent(b *testing.B, provider rig.Provider, tools ...rig.Tool) rig.Agent {
	b.Helper()
	a, err := agent.New(rig.AgentSpec{
		Provider:     provider,
		Model:        "bench-model",
		SystemPrompt: "be brief",
		Tools:        tools,
	})
	if err != nil {
		b.Fatalf("agent.New: %v", err)
	}
	return a
}

// BenchmarkRunTextStream measures one complete text-only run with an event
// consumer attached (the common streaming UI path).
func BenchmarkRunTextStream(b *testing.B) {
	a := newBenchAgent(b, benchTextProvider{})
	ctx := context.Background()
	events := 0
	req := rig.RunRequest{
		Input: []rig.Message{rig.NewMessage(rig.RoleUser, "What is the answer?")},
		Emit:  func(rig.Event) { events++ },
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result, err := a.Run(ctx, req)
		if err != nil {
			b.Fatalf("Run: %v", err)
		}
		if result.Iterations != 1 {
			b.Fatalf("Iterations = %d, want 1", result.Iterations)
		}
		if result.Usage.TotalTokens != 18 {
			b.Fatalf("Usage.TotalTokens = %d, want 18", result.Usage.TotalTokens)
		}
		if result.FinishReason != "stop" {
			b.Fatalf("FinishReason = %q, want stop", result.FinishReason)
		}
		benchSink = result
	}
	benchSink = events
}

// BenchmarkRunTextStreamNoEmit measures the same run with no event consumer, so
// the cost of Emit dispatch is visible by comparison.
func BenchmarkRunTextStreamNoEmit(b *testing.B) {
	a := newBenchAgent(b, benchTextProvider{})
	ctx := context.Background()
	req := rig.RunRequest{Input: []rig.Message{rig.NewMessage(rig.RoleUser, "What is the answer?")}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result, err := a.Run(ctx, req)
		if err != nil {
			b.Fatalf("Run: %v", err)
		}
		benchSink = result
	}
}

// BenchmarkRunToolRound measures a two-round run: the model requests one tool,
// the loop assembles the streamed call, runs it and feeds the result back.
func BenchmarkRunToolRound(b *testing.B) {
	a := newBenchAgent(b, benchToolProvider{}, benchTool{})
	ctx := context.Background()
	toolResults := 0
	req := rig.RunRequest{
		Input: []rig.Message{rig.NewMessage(rig.RoleUser, "Check main.go.")},
		Emit: func(event rig.Event) {
			if event.Type == rig.EventToolResult {
				toolResults++
			}
		},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result, err := a.Run(ctx, req)
		if err != nil {
			b.Fatalf("Run: %v", err)
		}
		if result.Iterations != 2 {
			b.Fatalf("Iterations = %d, want 2", result.Iterations)
		}
		benchSink = result
	}
	benchSink = toolResults
}
