package rig

import "context"

// Request is one model completion request. Stream is always true in this contract generation; the
// streaming interface is the only model transport so that partial output is
// observable everywhere.
type Request struct {
	Model       string
	Messages    []Message
	Tools       []ToolSpec
	Temperature *float64
	TopP        *float64
	MaxTokens   int
	// Extra carries provider-specific options (e.g. reasoning effort) without
	// widening this interface; providers ignore keys they do not know.
	Extra map[string]any
}

// ToolCallDelta is one partial tool call in a stream. Index identifies the
// call within the response; ID and Name arrive first, Arguments accumulate.
type ToolCallDelta struct {
	Index     int
	ID        string
	Name      string
	Arguments string
}

// Chunk is one streamed piece of a completion.
type Chunk struct {
	Delta        string
	Reasoning    string
	ToolCalls    []ToolCallDelta
	Usage        *Usage
	FinishReason string // "stop", "tool_calls", "length"; empty while streaming
}

// Stream is a model response stream. Recv returns io.EOF after the final chunk.
// Implementations must be safe to call from one goroutine at a time and must
// release resources when Recv returns an error or io.EOF.
type Stream interface {
	Recv() (Chunk, error)
}

// Provider is a model backend. Third parties implement this to add a vendor.
type Provider interface {
	// Name is a stable identifier ("openai", "my-llm", ...).
	Name() string
	// Stream starts one completion. The returned Stream is owned by the caller.
	Stream(ctx context.Context, req Request) (Stream, error)
}
