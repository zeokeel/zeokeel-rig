package rig

import "context"

// Tool is a capability the model can call. Implementations must be safe for
// concurrent calls when the agent executes a batch of calls in parallel.
type Tool interface {
	// Name is the model-facing function name (stable, unique in a registry).
	Name() string
	// Description tells the model when to use the tool.
	Description() string
	// Schema is the JSON Schema object for the arguments.
	Schema() map[string]any
	// Invoke executes one call. Returning an error is reported to the model as
	// a tool error (when the agent is configured to continue); a ToolResult with
	// IsError=true is passed through as the tool's own error text.
	Invoke(ctx context.Context, call ToolCall) (ToolResult, error)
}

// ToolResult is what a tool returns. Content is the text fed back to the model
// as the tool message (possibly truncated by the caller's policy); IsError marks
// failed execution.
//
// Parts carries optional non-text attachments (for example an image read by a
// read_image tool). Providers cannot place them in the tool message itself, so
// the agent appends them as a follow-up user message after the tool result; the
// field is additive and nil for text-only tools.
type ToolResult struct {
	Content string
	IsError bool
	Parts   []ContentPart
}

// Decision is one approval outcome for a pending tool call.
type Decision string

const (
	DecisionAllow       Decision = "allow"        // allow this call
	DecisionAllowAlways Decision = "allow-always" // allow this call and future identical calls
	DecisionDeny        Decision = "deny"         // deny and report to the model
)

// Gate decides whether a tool call may run. It is called on the agent run's
// goroutine before execution and may block on user input. A nil Gate means the
// agent's configured default (usually "allow everything" for trusted tools).
type Gate interface {
	Approve(ctx context.Context, tool Tool, call ToolCall) (Decision, error)
}

// ToolLister is implemented by registries that can enumerate their tools.
type ToolLister interface {
	Tools() []Tool
}

// History stores conversation messages per session. Implementations must be
// safe for concurrent use; the in-memory implementation lives in session/.
type History interface {
	// Load returns the stored messages for one session (nil when unknown).
	Load(ctx context.Context, sessionID string) ([]Message, error)
	// Append stores messages atomically per call.
	Append(ctx context.Context, sessionID string, messages ...Message) error
}
