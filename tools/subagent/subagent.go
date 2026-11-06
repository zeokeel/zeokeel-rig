// Package subagent exposes the in-process subagent runner (agent.NewRunner) as
// model-callable tools: the model delegates a task to a child agent and
// receives that child's final answer.
//
// Two tools are provided:
//
//   - subagent — a fresh child with its own context window. Optional fork=true
//     seeds the child with a copy of the parent session's completed history
//     (the runner must be configured with a parent session, otherwise Start
//     reports a configuration error). Optional output_schema asks the runner
//     for structured output; when the runner returns SubagentResult.Structured
//     the tool renders it as JSON alongside the child's text summary.
//   - subagent_fork — the same delegation with fork forced on (NewForkTool).
//
// The child still cannot ask follow-up questions, so a complete, self-contained
// prompt is required. The runner enforces depth/concurrency limits and filters
// orchestration tools from the child, so a subagent cannot recursively spawn
// more subagents by default.
//
// Tools(runner) returns both tools; New/NewTool keep the original single-tool
// constructors.
package subagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// Tool names. They are stable model-facing identifiers.
const (
	// ToolName is the model-facing name of the fresh-context delegation tool.
	ToolName = "subagent"
	// ToolNameFork is the model-facing name of the forked delegation tool; it
	// inherits a copy of the parent conversation.
	ToolNameFork = "subagent_fork"
)

// New returns the subagent tool backed by runner. A nil runner produces a tool
// that reports a configuration error when invoked (hosts may build tools before
// wiring the runner).
func New(runner rig.SubagentRunner) rig.Tool {
	return &tool{runner: runner}
}

// NewTool is the named constructor; it is New.
func NewTool(runner rig.SubagentRunner) rig.Tool { return New(runner) }

// NewForkTool returns the subagent_fork tool backed by runner: it delegates to a
// child seeded with a copy of the parent session's completed history
// (rig.SubagentSpec.Fork = true). The runner must have been configured with a
// parent session; otherwise its Start error is passed through to the caller.
func NewForkTool(runner rig.SubagentRunner) rig.Tool {
	return &tool{runner: runner, fork: true}
}

// ForkTool is the named constructor of subagent_fork; it is NewForkTool.
func ForkTool(runner rig.SubagentRunner) rig.Tool { return NewForkTool(runner) }

// Tools returns the delegation tools for runner: subagent followed by
// subagent_fork. It is the registration entry point for a full tool set.
func Tools(runner rig.SubagentRunner) []rig.Tool {
	return []rig.Tool{New(runner), NewForkTool(runner)}
}

// tool is the rig.Tool implementation shared by subagent and subagent_fork.
type tool struct {
	runner rig.SubagentRunner
	// fork forces rig.SubagentSpec.Fork. It is false for subagent (where the
	// caller may still pass fork=true) and true for subagent_fork.
	fork bool
}

var _ rig.Tool = (*tool)(nil)

// Name implements rig.Tool.
func (t *tool) Name() string {
	if t.fork {
		return ToolNameFork
	}
	return ToolName
}

// Description implements rig.Tool.
func (t *tool) Description() string {
	if t.fork {
		return "Delegate a task to a forked subagent that inherits a copy of the current conversation's completed " +
			"history before working on it. Use it when the child needs the current context (decisions already made, " +
			"files discussed) instead of a fresh prompt. It still cannot ask follow-up questions, so state the task " +
			"clearly and completely."
	}
	return "Delegate a self-contained task to a subagent. The subagent has its own context window and " +
		"returns only its final answer; use it for research, review or any task that would otherwise flood " +
		"this conversation. It cannot ask follow-up questions: give it a complete, self-contained prompt. " +
		"Set fork=true to let the child inherit a copy of this conversation, and output_schema to ask for a " +
		"structured JSON result."
}

// Schema implements rig.Tool.
func (t *tool) Schema() map[string]any {
	properties := map[string]any{
		"prompt": toolutil.StringProp("Complete task description for the subagent; it cannot see the current conversation unless fork=true."),
		"name":   toolutil.StringProp("Optional short name for the subagent (for logs/results)."),
		"model":  toolutil.StringProp("Optional model override for the subagent (defaults to the parent's model)."),
		"max_iterations": map[string]any{
			"type":        "integer",
			"description": "Optional cap on the subagent's model<->tool rounds (0 = runner default).",
		},
	}
	if !t.fork {
		properties["fork"] = map[string]any{
			"type": "boolean",
			"description": "Seed the child with a copy of this session's completed history instead of a fresh " +
				"context (requires a runner configured with a parent session).",
		}
		properties["output_schema"] = map[string]any{
			"type": "object",
			"description": "Optional JSON Schema for the child's structured output. When the runner returns a " +
				"structured result, the tool returns it as JSON together with the child's text summary.",
		}
	}
	return map[string]any{
		"type":                 "object",
		"properties":           properties,
		"required":             []string{"prompt"},
		"additionalProperties": false,
	}
}

// subagentArgs is the argument object for both tools.
type subagentArgs struct {
	Prompt        string          `json:"prompt"`
	Name          string          `json:"name"`
	Model         string          `json:"model"`
	MaxIterations int             `json:"max_iterations"`
	Fork          bool            `json:"fork"`
	OutputSchema  json.RawMessage `json:"output_schema"`
}

// Invoke implements rig.Tool.
func (t *tool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	var args subagentArgs
	if err := toolutil.DecodeArgsAs("subagent", call, &args); err != nil {
		return rig.ToolResult{}, err
	}
	name := t.Name()
	prompt := strings.TrimSpace(args.Prompt)
	if prompt == "" {
		return rig.ToolResult{Content: name + ": prompt is required", IsError: true}, nil
	}
	if args.MaxIterations < 0 {
		return rig.ToolResult{Content: name + ": max_iterations must not be negative", IsError: true}, nil
	}
	var outputSchema map[string]any
	if !t.fork {
		schema, err := parseOutputSchema(args.OutputSchema)
		if err != nil {
			return rig.ToolResult{Content: name + ": invalid output_schema: " + err.Error(), IsError: true}, nil
		}
		outputSchema = schema
	}
	if t.runner == nil {
		return rig.ToolResult{}, fmt.Errorf(
			"%s: no rig.SubagentRunner configured (construct the tool with subagent.New(runner) or subagent.NewForkTool(runner))",
			name)
	}
	sub, err := t.runner.Start(ctx, rig.SubagentSpec{
		Name:          strings.TrimSpace(args.Name),
		Prompt:        prompt,
		Model:         strings.TrimSpace(args.Model),
		MaxIterations: args.MaxIterations,
		Fork:          t.fork || args.Fork,
		OutputSchema:  outputSchema,
	})
	if err != nil {
		return rig.ToolResult{}, fmt.Errorf("%s: start: %w", name, err)
	}
	// Always release the subagent's concurrency slot, even when the run fails.
	defer func() { _ = sub.Stop() }()

	result, err := sub.Wait(ctx)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return rig.ToolResult{}, ctxErr
		}
		return rig.ToolResult{}, fmt.Errorf("%s: run: %w", name, err)
	}
	return renderResult(name, result), nil
}

// parseOutputSchema validates and decodes the optional output_schema argument.
// A missing or null value means "no structured output"; any other non-object
// JSON value is a readable tool error.
func parseOutputSchema(raw json.RawMessage) (map[string]any, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, nil
	}
	if trimmed[0] != '{' {
		return nil, errors.New("output_schema must be a JSON object (a JSON Schema)")
	}
	var schema map[string]any
	if err := json.Unmarshal(trimmed, &schema); err != nil {
		return nil, fmt.Errorf("output_schema must be a JSON object (a JSON Schema): %w", err)
	}
	if len(schema) == 0 {
		return nil, nil
	}
	return schema, nil
}

// renderResult renders one subagent result: structured output (when present) as
// compact JSON after the child's text summary, otherwise just the text.
func renderResult(toolName string, result rig.SubagentResult) rig.ToolResult {
	text := strings.TrimSpace(result.Output)
	if len(result.Structured) > 0 {
		encoded, err := json.Marshal(result.Structured)
		if err != nil {
			return rig.ToolResult{
				Content: fmt.Sprintf("%s: structured output is not JSON-encodable: %v", toolName, err),
				IsError: true,
			}
		}
		if text == "" {
			// Structured-only result: return the JSON on its own so the caller
			// can decode the tool content directly.
			return rig.ToolResult{Content: string(encoded)}
		}
		var builder strings.Builder
		builder.WriteString(text)
		builder.WriteString("\n\nstructured output:\n")
		builder.Write(encoded)
		builder.WriteString("\n")
		return rig.ToolResult{Content: builder.String()}
	}
	if text == "" {
		text = "(subagent finished without text output)"
	}
	return rig.ToolResult{Content: text}
}
