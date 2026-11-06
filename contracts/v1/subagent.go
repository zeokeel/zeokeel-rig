package v1

import "context"

// SubagentSpec describes one child agent started by an orchestration tool
// (subagent / workflow / team) or by a host that wants in-process delegation.
type SubagentSpec struct {
	// Name is the child's display name (unique within a team manager).
	Name string
	// Prompt is the first user message for the child.
	Prompt string
	// SystemPrompt overrides the parent's system prompt when non-empty.
	SystemPrompt string
	// Model overrides the parent's model when non-empty.
	Model string
	// MaxIterations caps the child's model<->tool rounds (0 = agent default).
	MaxIterations int
	// Fork seeds the child with a copy of the parent session's completed
	// history instead of a fresh context. The runner must have been configured
	// with RunnerOptions.ParentSessionID (and parent History); otherwise Start
	// reports a configuration error.
	Fork bool
	// OutputSchema asks the child for structured output: the runner exposes a
	// structured_output tool whose arguments must match this (subset of) JSON
	// Schema, and returns the captured object in SubagentResult.Structured.
	OutputSchema map[string]any
}

// SubagentResult is the outcome of one subagent run.
type SubagentResult struct {
	// Name echoes the spec name.
	Name string
	// Output is the final assistant text of the run.
	Output string
	// Usage is the child's token accounting for that run.
	Usage Usage
	// Iterations is the number of model rounds executed.
	Iterations int
	// Structured holds the child's structured output when the spec carried an
	// OutputSchema (nil otherwise).
	Structured map[string]any
}

// Subagent is a live in-process child agent. Implementations must be safe for
// concurrent Send/Wait/Stop calls; runs on one Subagent are serialized.
type Subagent interface {
	// Name returns the spec name.
	Name() string
	// Send delivers a follow-up user message and blocks until that run ends.
	// It waits for any in-flight run first (runs are serialized).
	Send(ctx context.Context, message string) (SubagentResult, error)
	// Wait blocks for the current/first run and returns its result. Calling it
	// after the run finished returns the same result.
	Wait(ctx context.Context) (SubagentResult, error)
	// Stop cancels the in-flight run and releases the subagent. Idempotent.
	Stop() error
}

// SubagentRunner starts child agents for orchestration. The agent package
// provides the default implementation (agent.NewRunner), delegating to the same
// provider/model/tool configuration as its parent with a depth and concurrency
// limit. Third parties may implement it to plug a different runtime.
type SubagentRunner interface {
	Start(ctx context.Context, spec SubagentSpec) (Subagent, error)
}
