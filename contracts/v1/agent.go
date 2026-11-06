package v1

import "context"

// EventType identifies one agent event. Events are emitted synchronously from
// the run goroutine; a consumer that must not block should buffer them.
type EventType string

const (
	EventTurnStart      EventType = "turn_start"
	EventTextDelta      EventType = "text_delta"
	EventReasoningDelta EventType = "reasoning_delta"
	EventToolCall       EventType = "tool_call"   // model requested a call
	EventToolResult     EventType = "tool_result" // a call finished
	EventUsage          EventType = "usage"       // token accounting for a model call
	EventMessage        EventType = "message"     // a completed message appended
	EventTurnEnd        EventType = "turn_end"    // one model round finished
	EventError          EventType = "error"
)

// Event is one observable step of an agent run. Only the fields relevant to
// Type are set; consumers must ignore the rest.
type Event struct {
	Type      EventType
	Text      string
	Reasoning string
	Call      *ToolCall
	Result    *ToolResult
	Message   *Message
	Usage     *Usage
	Iteration int
	Err       error
}

// RunRequest is one agent turn (or continuation) for a session.
type RunRequest struct {
	// SessionID selects the conversation history; empty means a fresh history.
	SessionID string
	// Input is appended as the next user message(s) before the run starts.
	// Callers that manage history themselves leave it empty.
	Input []Message
	// Model overrides the agent's default model.
	Model string
	// MaxIterations caps model<->tool rounds; 0 uses the agent default.
	MaxIterations int
	// Gate, when set, overrides the agent's approval gate for this run.
	Gate Gate
	// Emit observes events; nil drops them.
	Emit func(Event)
}

// RunResult is the outcome of one agent run.
type RunResult struct {
	// Messages appended to history during this run, in order.
	Messages []Message
	// Usage is the summed usage of every model call in the run.
	Usage Usage
	// Iterations is the number of model rounds executed.
	Iterations int
	// FinishReason is the final model finish reason ("stop", "length", ...).
	FinishReason string
}

// Agent runs a model<->tool loop over a session history.
type Agent interface {
	// Run executes one turn. It returns when the model stops requesting tools,
	// the iteration limit is hit, the context is cancelled, or a fatal error
	// occurs. A cancelled context must return ctx.Err().
	Run(ctx context.Context, req RunRequest) (RunResult, error)
}

// AgentSpec describes an agent's static configuration. Agent implementations
// construct themselves from a spec; the assembly passes the decoded config.
type AgentSpec struct {
	// Model is the default model name.
	Model string
	// SystemPrompt is prepended to every run when non-empty.
	SystemPrompt string
	// MaxIterations is the default round limit (0 = 20).
	MaxIterations int
	// Tools are the tools exposed to the model.
	Tools []Tool
	// Provider is the model backend.
	Provider Provider
	// History stores the conversation; nil disables persistence.
	History History
	// Gate is the default approval gate; nil means allow all.
	Gate Gate
	// Temperature/TopP/MaxTokens are default sampling options (0/nil = unset).
	Temperature *float64
	TopP        *float64
	MaxTokens   int
	// Logger receives diagnostic output; nil drops it.
	Logger Logger
}

// Logger is the minimal logging surface a Rig component may use.
type Logger interface {
	Printf(format string, args ...any)
}

// Settings is a tiny key/value settings surface (file-backed in rig/host).
type Settings interface {
	Get(key, def string) string
	Set(key, value string) error
}
