package ralph

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// ToolName 是 ralph 工具的模型可见名称。
const ToolName = "ralph"

const (
	// DefaultMaxRounds 是调用未提供 max_rounds 时的轮次预算。
	DefaultMaxRounds = 8
	// MaxMaxRounds 限制单次调用的轮次预算上限。
	MaxMaxRounds = 32
	// DefaultCompletionPrompt 告诉子代理如何声明任务完成。
	DefaultCompletionPrompt = "If the task is complete, reply DONE"
	// MaxRoundSummaryChars 限制结果摘要中每一轮输出的长度。
	MaxRoundSummaryChars = 2000
)

// donePattern 匹配独立的 DONE 标记（因此 "undone" 不算完成）或中文完成标记。
var donePattern = regexp.MustCompile(`(?i)(^|[^a-zA-Z])done([^a-zA-Z]|$)|完成`)

// New 返回由 runner 驱动的 ralph 工具。runner 为 nil 时，构造出的工具会在调用时
// 返回配置错误。
func New(runner rig.SubagentRunner) rig.Tool { return &tool{runner: runner} }

// NewTool 是具名构造器，等同于 New。
func NewTool(runner rig.SubagentRunner) rig.Tool { return New(runner) }

// ralphArgs 是工具参数对象。
type ralphArgs struct {
	Prompt           string `json:"prompt"`
	CompletionPrompt string `json:"completion_prompt"`
	MaxRounds        int    `json:"max_rounds"`
	Name             string `json:"name"`
}

// roundResult 表示一轮已完成的迭代。
type roundResult struct {
	Number int
	Output string
}

type tool struct {
	runner rig.SubagentRunner
}

var _ rig.Tool = (*tool)(nil)

// Name 实现 rig.Tool。
func (t *tool) Name() string { return ToolName }

// Description 实现 rig.Tool。
func (t *tool) Description() string {
	return "Keep prompting one subagent until it reports completion. Round 1 sends the task; every later round " +
		"sends the previous round's output plus a completion prompt, and the loop stops when the reply contains " +
		"DONE (or 完成) or the round budget is reached. Use it for tasks that need several self-correction passes."
}

// Schema 实现 rig.Tool。
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

// Invoke 实现 rig.Tool。
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
	maxRounds = min(maxRounds, MaxMaxRounds)
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

// isComplete 判断单轮输出是否包含完成标记。
func isComplete(output string) bool {
	return donePattern.MatchString(output)
}

// renderContinuePrompt 构造下一轮的续跑消息。完成提示词可以引用 {{output}} 与
// {{round}}；否则会把上一轮输出前置，确保子代理即使拥有会话历史也能看到它。
func renderContinuePrompt(completionPrompt, previousOutput string, round int) string {
	roundText := strconv.Itoa(round)
	template := completionPrompt
	hasOutputPlaceholder := strings.Contains(template, "{{output}}")
	template = strings.ReplaceAll(template, "{{output}}", previousOutput)
	template = strings.ReplaceAll(template, "{{round}}", roundText)
	if hasOutputPlaceholder {
		return template
	}
	return fmt.Sprintf("Previous round output:\n%s\n\n%s", strings.TrimSpace(previousOutput), template)
}

// successResult 渲染正常完成的循环结果。
func successResult(rounds []roundResult, finalOutput string) rig.ToolResult {
	var builder strings.Builder
	fmt.Fprintf(&builder, "ralph: completed after %d round(s)\n", len(rounds))
	builder.WriteString(renderRounds(rounds))
	if strings.TrimSpace(finalOutput) != "" {
		builder.WriteString("\n\nfinal output:\n" + toolutil.TruncateBytes(strings.TrimSpace(finalOutput), MaxRoundSummaryChars, "\n[truncated: round output exceeded 2000 characters]"))
	}
	return rig.ToolResult{Content: builder.String()}
}

// limitResult 渲染轮次预算耗尽的循环结果。
func limitResult(rounds []roundResult, maxRounds int) rig.ToolResult {
	var builder strings.Builder
	fmt.Fprintf(&builder, "ralph: reached max_rounds (%d) without a completion marker; the task is not confirmed complete\n", maxRounds)
	builder.WriteString(renderRounds(rounds))
	return rig.ToolResult{Content: builder.String(), IsError: true}
}

// runFailure 渲染在若干轮完成之后的子代理失败。调用方上下文被取消属于 Go 错误；
// 子代理自身失败则作为工具结果返回。
func runFailure(ctx context.Context, rounds []roundResult, round int, err error) (rig.ToolResult, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return rig.ToolResult{}, ctxErr
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "ralph: round %d failed: %v\n", round, err)
	builder.WriteString(renderRounds(rounds))
	return rig.ToolResult{Content: builder.String(), IsError: true}, nil
}

// renderRounds 渲染每一轮已完成迭代的摘要，并限制单轮长度。
func renderRounds(rounds []roundResult) string {
	var builder strings.Builder
	for _, round := range rounds {
		fmt.Fprintf(&builder, "\n== round %d ==\n%s", round.Number, toolutil.TruncateBytes(strings.TrimSpace(round.Output), MaxRoundSummaryChars, "\n[truncated: round output exceeded 2000 characters]"))
	}
	return strings.TrimSuffix(builder.String(), "\n")
}
