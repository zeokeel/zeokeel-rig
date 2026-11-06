// Package subagent exposes the in-process subagent runner (agent.NewRunner) as
// a model-callable tool: the model delegates a self-contained task to a child
// agent and receives that child's final answer.
//
// The child has its own context window: it does not see the parent's
// conversation, it gets an isolated history, and it returns only its final text.
// The runner enforces depth/concurrency limits and filters orchestration tools
// from the child, so a subagent cannot recursively spawn more subagents by
// default.
package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
)

// ToolName is the model-facing name of the delegation tool.
const ToolName = "subagent"

// New returns the subagent tool backed by runner. A nil runner produces a tool
// that reports a configuration error when invoked (hosts may build tools before
// wiring the runner).
func New(runner v1.SubagentRunner) v1.Tool {
	return &tool{runner: runner}
}

// NewTool is the named constructor; it is New.
func NewTool(runner v1.SubagentRunner) v1.Tool { return New(runner) }

// tool is the v1.Tool implementation.
type tool struct {
	runner v1.SubagentRunner
}

var _ v1.Tool = (*tool)(nil)

// Name implements v1.Tool.
func (t *tool) Name() string { return ToolName }

// Description implements v1.Tool.
func (t *tool) Description() string {
	return "Delegate a self-contained task to a subagent. The subagent has its own context window and " +
		"returns only its final answer; use it for research, review or any task that would otherwise flood " +
		"this conversation. It cannot ask follow-up questions: give it a complete, self-contained prompt."
}

// Schema implements v1.Tool.
func (t *tool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"prompt": stringProp("Complete task description for the subagent; it cannot see the current conversation."),
			"name":   stringProp("Optional short name for the subagent (for logs/results)."),
			"model":  stringProp("Optional model override for the subagent (defaults to the parent's model)."),
			"max_iterations": map[string]any{
				"type":        "integer",
				"description": "Optional cap on the subagent's model<->tool rounds (0 = runner default).",
			},
		},
		"required":             []string{"prompt"},
		"additionalProperties": false,
	}
}

// subagentArgs is the argument object.
type subagentArgs struct {
	Prompt        string `json:"prompt"`
	Name          string `json:"name"`
	Model         string `json:"model"`
	MaxIterations int    `json:"max_iterations"`
}

// Invoke implements v1.Tool.
func (t *tool) Invoke(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error) {
	var args subagentArgs
	if err := decodeArgs(call, &args); err != nil {
		return v1.ToolResult{}, err
	}
	prompt := strings.TrimSpace(args.Prompt)
	if prompt == "" {
		return v1.ToolResult{Content: "subagent: prompt is required", IsError: true}, nil
	}
	if args.MaxIterations < 0 {
		return v1.ToolResult{Content: "subagent: max_iterations must not be negative", IsError: true}, nil
	}
	if t.runner == nil {
		return v1.ToolResult{}, fmt.Errorf(
			"subagent: no v1.SubagentRunner configured (construct the tool with subagent.New(runner))")
	}
	sub, err := t.runner.Start(ctx, v1.SubagentSpec{
		Name:          strings.TrimSpace(args.Name),
		Prompt:        prompt,
		Model:         strings.TrimSpace(args.Model),
		MaxIterations: args.MaxIterations,
	})
	if err != nil {
		return v1.ToolResult{}, fmt.Errorf("subagent: start: %w", err)
	}
	// Always release the subagent's concurrency slot, even when the run fails.
	defer func() { _ = sub.Stop() }()

	result, err := sub.Wait(ctx)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return v1.ToolResult{}, ctxErr
		}
		return v1.ToolResult{}, fmt.Errorf("subagent: run: %w", err)
	}
	output := strings.TrimSpace(result.Output)
	if output == "" {
		output = "(subagent finished without text output)"
	}
	return v1.ToolResult{Content: output}, nil
}

// decodeArgs decodes one call's arguments; an empty argument string is an empty
// object.
func decodeArgs(call v1.ToolCall, target any) error {
	raw := strings.TrimSpace(call.Arguments)
	if raw == "" {
		raw = "{}"
	}
	if err := json.Unmarshal([]byte(raw), target); err != nil {
		return fmt.Errorf("subagent: invalid arguments JSON: %w", err)
	}
	return nil
}

func stringProp(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}
