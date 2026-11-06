// Package ralph provides the ralph tool: it drives one subagent in repeated
// rounds toward a completion marker. Each round sends the previous round's
// output plus the completion prompt back to the same subagent (its conversation
// is preserved), and the loop stops when the reply contains DONE / 完成 or the
// round budget runs out.
package ralph

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// ToolName is the model-facing name of the ralph tool.
const ToolName = "ralph"

const (
	// DefaultMaxRounds is the round budget when the call omits max_rounds.
	DefaultMaxRounds = 8
	// MaxMaxRounds caps a per-call round budget.
	MaxMaxRounds = 32
	// DefaultCompletionPrompt tells the child how to signal completion.
	DefaultCompletionPrompt = "If the task is complete, reply DONE"
	// MaxRoundSummaryChars bounds one round output in the result summary.
	MaxRoundSummaryChars = 2000
)

// donePattern matches a standalone DONE token (so "undone" is not completion)
// or the Chinese completion marker.
var donePattern = regexp.MustCompile(`(?i)(^|[^a-zA-Z])done([^a-zA-Z]|$)|完成`)

// New returns the ralph tool backed by runner. A nil runner produces a tool
// that reports a configuration error when invoked.
func New(runner rig.SubagentRunner) rig.Tool { return &tool{runner: runner} }

// NewTool is the named constructor; it is New.
func NewTool(runner rig.SubagentRunner) rig.Tool { return New(runner) }

// ralphArgs is the argument object.
type ralphArgs struct {
	Prompt           string `json:"prompt"`
	CompletionPrompt string `json:"completion_prompt"`
	MaxRounds        int    `json:"max_rounds"`
	Name             string `json:"name"`
}

// roundResult is one completed round.
type roundResult struct {
	Number int
	Output string
}

type tool struct {
	runner rig.SubagentRunner
}

var _ rig.Tool = (*tool)(nil)

// Name implements rig.Tool.
func (t *tool) Name() string { return ToolName }

// Description implements rig.Tool.
func (t *tool) Description() string {
	return "Keep prompting one subagent until it reports completion. Round 1 sends the task; every later round " +
		"sends the previous round's output plus a completion prompt, and the loop stops when the reply contains " +
		"DONE (or 完成) or the round budget is reached. Use it for tasks that need several self-correction passes."
}

// Schema implements rig.Tool.
func (t *tool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"prompt":            toolutil.StringProp("Complete task description for the subagent."),
			"completion_prompt": toolutil.StringProp(fmt.Sprintf("Sent after each round with the previous output (may use {{output}} and {{round}}). Default: %q.", DefaultCompletionPrompt)),
			"max_rounds": map[string]any{
				"type":        "integer",
				"description": fmt.Sprintf("Maximum rounds (default %d, maximum %d).", DefaultMaxRounds, MaxMaxRounds),
			},
			"name": toolutil.StringProp("Optional subagent name for logs."),
		},
		"required":             []string{"prompt"},
		"additionalProperties": false,
	}
}

// Invoke implements rig.Tool.
func (t *tool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	var args ralphArgs
	if err := toolutil.DecodeArgsAs("ralph", call, &args); err != nil {
		return rig.ToolResult{}, err
	}
	prompt := strings.TrimSpace(args.Prompt)
	if prompt == "" {
		return toolutil.ErrorResultf("ralph: prompt is required"), nil
	}
	maxRounds := args.MaxRounds
	if maxRounds <= 0 {
		maxRounds = DefaultMaxRounds
	}
	if maxRounds > MaxMaxRounds {
		maxRounds = MaxMaxRounds
	}
	completionPrompt := strings.TrimSpace(args.CompletionPrompt)
	if completionPrompt == "" {
		completionPrompt = DefaultCompletionPrompt
	}
	if t.runner == nil {
		return rig.ToolResult{}, fmt.Errorf(
			"ralph: no rig.SubagentRunner configured (construct the tool with ralph.New(runner))")
	}

	name := strings.TrimSpace(args.Name)
	if name == "" {
		name = "ralph"
	}
	sub, err := t.runner.Start(ctx, rig.SubagentSpec{Name: name, Prompt: prompt})
	if err != nil {
		return rig.ToolResult{}, fmt.Errorf("ralph: start: %w", err)
	}
	defer func() { _ = sub.Stop() }()

	rounds := make([]roundResult, 0, maxRounds)
	current, err := sub.Wait(ctx)
	if err != nil {
		return runFailure(ctx, rounds, 1, err)
	}
	rounds = append(rounds, roundResult{Number: 1, Output: current.Output})

	for round := 1; ; round++ {
		if isComplete(rounds[len(rounds)-1].Output) {
			return successResult(rounds, current.Output), nil
		}
		if round >= maxRounds {
			return limitResult(rounds, maxRounds), nil
		}
		nextPrompt := renderContinuePrompt(completionPrompt, rounds[len(rounds)-1].Output, round+1)
		next, err := sub.Send(ctx, nextPrompt)
		if err != nil {
			return runFailure(ctx, rounds, round+1, err)
		}
		rounds = append(rounds, roundResult{Number: round + 1, Output: next.Output})
		current = next
	}
}

// isComplete reports whether one round output contains a completion marker.
func isComplete(output string) bool {
	return donePattern.MatchString(output)
}

// renderContinuePrompt builds the follow-up message for one round. The
// completion prompt may reference {{output}} and {{round}}; otherwise the
// previous output is prepended so the child sees it even though it also owns
// the conversation history.
func renderContinuePrompt(completionPrompt, previousOutput string, round int) string {
	roundText := fmt.Sprintf("%d", round)
	template := completionPrompt
	hasOutputPlaceholder := strings.Contains(template, "{{output}}")
	template = strings.ReplaceAll(template, "{{output}}", previousOutput)
	template = strings.ReplaceAll(template, "{{round}}", roundText)
	if hasOutputPlaceholder {
		return template
	}
	return fmt.Sprintf("Previous round output:\n%s\n\n%s", strings.TrimSpace(previousOutput), template)
}

// successResult renders the completed loop.
func successResult(rounds []roundResult, finalOutput string) rig.ToolResult {
	var builder strings.Builder
	fmt.Fprintf(&builder, "ralph: completed after %d round(s)\n", len(rounds))
	builder.WriteString(renderRounds(rounds))
	if strings.TrimSpace(finalOutput) != "" {
		builder.WriteString("\n\nfinal output:\n" + toolutil.TruncateBytes(strings.TrimSpace(finalOutput), MaxRoundSummaryChars, "\n[truncated: round output exceeded 2000 characters]"))
	}
	return rig.ToolResult{Content: builder.String()}
}

// limitResult renders the loop that exhausted its round budget.
func limitResult(rounds []roundResult, maxRounds int) rig.ToolResult {
	var builder strings.Builder
	fmt.Fprintf(&builder, "ralph: reached max_rounds (%d) without a completion marker; the task is not confirmed complete\n", maxRounds)
	builder.WriteString(renderRounds(rounds))
	return rig.ToolResult{Content: builder.String(), IsError: true}
}

// runFailure renders a subagent failure after some completed rounds. A
// cancelled caller context is a Go error; subagent failures are tool results.
func runFailure(ctx context.Context, rounds []roundResult, round int, err error) (rig.ToolResult, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return rig.ToolResult{}, ctxErr
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "ralph: round %d failed: %v\n", round, err)
	builder.WriteString(renderRounds(rounds))
	return rig.ToolResult{Content: builder.String(), IsError: true}, nil
}

// renderRounds renders every completed round summary, bounded per round.
func renderRounds(rounds []roundResult) string {
	var builder strings.Builder
	for _, round := range rounds {
		fmt.Fprintf(&builder, "\n== round %d ==\n%s", round.Number, toolutil.TruncateBytes(strings.TrimSpace(round.Output), MaxRoundSummaryChars, "\n[truncated: round output exceeded 2000 characters]"))
	}
	return strings.TrimSuffix(builder.String(), "\n")
}
