package rig

import "context"

// Ordered lets a stage/extension declare its position in its phase. Stages are
// stable-sorted by Order at construction; equal orders keep registration order.
type Ordered interface {
	Order() int
}

// PromptDraft is the assembled system prompt for one run, before the first
// model request. A PromptStage may rewrite SystemPrompt; byte changes invalidate
// the provider's prompt cache, so stages should be deterministic.
type PromptDraft struct {
	SessionID    string
	Model        string
	SystemPrompt string
}

// PromptStage runs once per Run, after SystemPrompt/SystemPromptFunc resolution.
type PromptStage interface {
	Prompt(ctx context.Context, draft *PromptDraft) error
}

// RequestDraft is one model request. A RequestStage may rewrite any field; the
// provider sees exactly the returned draft.
type RequestDraft struct {
	SessionID   string
	Model       string
	Messages    []Message
	Tools       []ToolSpec
	Temperature *float64
	TopP        *float64
	MaxTokens   int
}

// RequestStage runs before every model request.
type RequestStage interface {
	Request(ctx context.Context, draft *RequestDraft) error
}

// ResponseDraft is the assistant message assembled from one model response,
// before it is written to history or emitted. A ResponseStage may rewrite the
// text/reasoning; rewriting ToolCalls is allowed but the agent re-normalizes
// names afterwards, so stages should not change call identity.
type ResponseDraft struct {
	SessionID    string
	Model        string
	Message      Message
	FinishReason string
	Usage        Usage
}

// ResponseStage runs after each streamed model response completes.
type ResponseStage interface {
	Response(ctx context.Context, draft *ResponseDraft) error
}

// TurnInfo describes one model↔tool iteration.
type TurnInfo struct {
	SessionID string
	Model     string
	Iteration int
	Result    RunResult
}

// TurnStage observes or vetoes iteration boundaries. Returning an error from
// TurnStart aborts the run; TurnEnd errors also abort.
type TurnStage interface {
	TurnStart(ctx context.Context, info *TurnInfo) error
	TurnEnd(ctx context.Context, info *TurnInfo) error
}

// CompactionDraft is the conversation about to be compacted. A CompactionStage
// may rewrite Messages; the compactor sees the rewritten input.
type CompactionDraft struct {
	SessionID    string
	Before       int
	Messages     []Message
	SystemTokens int
	Budget       int
}

// CompactionStage runs before the automatic compactor when the budget is over.
type CompactionStage interface {
	Compact(ctx context.Context, draft *CompactionDraft) error
}
