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

// 工具名。它们是稳定的模型可见标识。
const (
	// ToolName 是新上下文委派工具的模型可见名称。
	ToolName = "subagent"
	// ToolNameFork 是 fork 委派工具的模型可见名称；它继承父对话的一份副本。
	ToolNameFork = "subagent_fork"
)

// New 返回由 runner 驱动的 subagent 工具。runner 为 nil 时会构造出一个在调用时
// 返回配置错误的工具（宿主可以先构造工具、之后再接线 runner）。
func New(runner rig.SubagentRunner) rig.Tool {
	return &tool{runner: runner}
}

// NewTool 是具名构造器，等同于 New。
func NewTool(runner rig.SubagentRunner) rig.Tool { return New(runner) }

// NewForkTool 返回由 runner 驱动的 subagent_fork 工具：它委派给一个以父会话已完成
// 历史副本为种子的子代理（rig.SubagentSpec.Fork = true）。runner 必须配置了父会话；
// 否则其 Start 错误会原样传给调用方。
func NewForkTool(runner rig.SubagentRunner) rig.Tool {
	return &tool{runner: runner, fork: true}
}

// ForkTool 是 subagent_fork 的具名构造器，等同于 NewForkTool。
func ForkTool(runner rig.SubagentRunner) rig.Tool { return NewForkTool(runner) }

// Tools 返回 runner 的委派工具：subagent 后跟 subagent_fork。它是完整工具集的
// 注册入口。
func Tools(runner rig.SubagentRunner) []rig.Tool {
	return []rig.Tool{New(runner), NewForkTool(runner)}
}

// tool 是 subagent 与 subagent_fork 共用的 rig.Tool 实现。
type tool struct {
	runner rig.SubagentRunner
	// fork 强制 rig.SubagentSpec.Fork。subagent 工具为 false（调用方仍可传
	// fork=true），subagent_fork 工具为 true。
	fork bool
}

var _ rig.Tool = (*tool)(nil)

// Name 实现 rig.Tool。
func (t *tool) Name() string {
	if t.fork {
		return ToolNameFork
	}
	return ToolName
}

// Description 实现 rig.Tool。
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

// Schema 实现 rig.Tool。
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

// subagentArgs 是两个工具共用的参数对象。
type subagentArgs struct {
	Prompt        string          `json:"prompt"`
	Name          string          `json:"name"`
	Model         string          `json:"model"`
	MaxIterations int             `json:"max_iterations"`
	Fork          bool            `json:"fork"`
	OutputSchema  json.RawMessage `json:"output_schema"`
}

// Invoke 实现 rig.Tool。
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
	// 无论运行成功与否，都必须释放子代理的并发额度。
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

// parseOutputSchema 校验并解码可选的 output_schema 参数。
// 缺失或 null 表示“不要结构化输出”；其他任何非对象 JSON 值都会变成可读的工具错误。
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

// renderResult 渲染一个子代理结果：有结构化输出时在子代理文本摘要之后附上紧凑 JSON，
// 否则只返回文本。
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
			// 只有结构化输出的结果：直接返回 JSON，便于调用方解码工具内容。
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
