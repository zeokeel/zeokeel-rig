package workflow

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// ToolName 是 workflow 工具的模型可见名称。
const ToolName = "workflow"

// DefaultMaxParallel 是未提供 max_parallel 时单个并行组的并发上限。
const DefaultMaxParallel = 4

// MaxMaxParallel 是 max_parallel 允许的最大值。
const MaxMaxParallel = 8

// Step 表示一个工作流步骤。
type Step struct {
	// Name 用于模板、摘要与日志标识该步骤。空名称会变成 "step-N"。
	Name string `json:"name"`
	// Prompt 是子代理提示词；发送前会展开 {{prev}} 与 {{steps.<name>}}。
	Prompt string `json:"prompt"`
	// Parallel 标记应并发执行的相邻步骤。
	Parallel bool `json:"parallel,omitempty"`
}

// New 返回由 runner 驱动的 workflow 工具。runner 为 nil 时，构造出的工具会在调用时
// 返回配置错误。
func New(runner rig.SubagentRunner) rig.Tool { return &tool{runner: runner} }

// NewTool 是具名构造器，等同于 New。
func NewTool(runner rig.SubagentRunner) rig.Tool { return New(runner) }

type tool struct {
	runner rig.SubagentRunner
}

var _ rig.Tool = (*tool)(nil)

// Name 实现 rig.Tool。
func (t *tool) Name() string { return ToolName }

// Description 实现 rig.Tool。
func (t *tool) Description() string {
	return "Run a multi-step workflow, one subagent per step, and return every step's output. " +
		"Prompts may reference the previous step's output with {{prev}} and any earlier named step with " +
		"{{steps.<name>}}. Steps marked parallel:true and adjacent run concurrently (bounded by max_parallel). " +
		"On failure the tool returns the completed steps plus the failing step's error."
}

// Schema 实现 rig.Tool。
func (t *tool) Schema() map[string]any {
	step := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":     toolutil.StringProp("Step name (unique; default step-N)."),
			"prompt":   toolutil.StringProp("Complete subagent prompt; supports {{prev}} and {{steps.<name>}}."),
			"parallel": map[string]any{"type": "boolean", "description": "Run adjacent parallel steps concurrently."},
		},
		"required":             []string{"prompt"},
		"additionalProperties": false,
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"steps": map[string]any{
				"type":        "array",
				"description": "Ordered workflow steps (or use the tasks alias).",
				"items":       step,
			},
			"tasks": map[string]any{
				"type":        "array",
				"description": "Alias of steps for a simple sequential workflow.",
				"items":       step,
			},
			"max_parallel": map[string]any{
				"type":        "integer",
				"description": "Maximum concurrent subagents in a parallel group (default 4, maximum 8).",
			},
		},
		"additionalProperties": false,
	}
}

// workflowArgs 是工具参数对象。
type workflowArgs struct {
	Steps       []Step `json:"steps"`
	Tasks       []Step `json:"tasks"`
	MaxParallel int    `json:"max_parallel"`
}

// stepResult 表示一个已完成的步骤。
type stepResult struct {
	Name   string
	Output string
}

// Invoke 实现 rig.Tool。
func (t *tool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	var args workflowArgs
	if err := toolutil.DecodeArgsAs("workflow", call, &args); err != nil {
		return rig.ToolResult{}, err
	}
	steps := args.Steps
	if len(steps) == 0 {
		steps = args.Tasks
	}
	if len(steps) == 0 {
		return rig.ToolResult{Content: "workflow: steps is required (send at least one step)", IsError: true}, nil
	}
	if t.runner == nil {
		return rig.ToolResult{}, fmt.Errorf(
			"workflow: no rig.SubagentRunner configured (construct the tool with workflow.New(runner))")
	}
	if err := normalizeSteps(steps); err != nil {
		return rig.ToolResult{Content: "workflow: " + err.Error(), IsError: true}, nil
	}
	maxParallel := args.MaxParallel
	if maxParallel <= 0 {
		maxParallel = DefaultMaxParallel
	}
	maxParallel = min(maxParallel, MaxMaxParallel)

	outputs := make(map[string]string, len(steps))
	results := make([]stepResult, 0, len(steps))
	previous := ""
	for index := 0; index < len(steps); {
		step := steps[index]
		if !step.Parallel {
			rendered := renderTemplate(step.Prompt, outputs, previous)
			output, err := runStep(ctx, t.runner, step.Name, rendered)
			if err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return rig.ToolResult{}, ctxErr
				}
				return failureResult(results, step.Name, err), nil
			}
			outputs[step.Name] = output
			previous = output
			results = append(results, stepResult{Name: step.Name, Output: output})
			index++
			continue
		}

		// 一个连续的并行组。
		groupEnd := index
		for groupEnd < len(steps) && steps[groupEnd].Parallel {
			groupEnd++
		}
		group := steps[index:groupEnd]
		parallelResults, failedName, err := runParallelGroup(ctx, t.runner, group, outputs, previous, maxParallel)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return rig.ToolResult{}, ctxErr
			}
			return failureResult(append(results, parallelResults...), failedName, err), nil
		}
		for _, result := range parallelResults {
			outputs[result.Name] = result.Output
			results = append(results, result)
			previous = result.Output
		}
		index = groupEnd
	}
	return successResult(results), nil
}

// normalizeSteps 填充默认步骤名并校验步骤列表。
func normalizeSteps(steps []Step) error {
	seen := make(map[string]struct{}, len(steps))
	for index := range steps {
		name := strings.TrimSpace(steps[index].Name)
		if name == "" {
			name = fmt.Sprintf("step-%d", index+1)
			steps[index].Name = name
		} else {
			steps[index].Name = name
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("duplicate step name %q", name)
		}
		seen[name] = struct{}{}
		if strings.TrimSpace(steps[index].Prompt) == "" {
			return fmt.Errorf("step %q has an empty prompt", name)
		}
	}
	return nil
}

// runStep 启动一个子代理、等待其结果并释放它。
func runStep(ctx context.Context, runner rig.SubagentRunner, name, prompt string) (string, error) {
	sub, err := runner.Start(ctx, rig.SubagentSpec{Name: name, Prompt: prompt})
	if err != nil {
		return "", err
	}
	defer func() { _ = sub.Stop() }()
	result, err := sub.Wait(ctx)
	if err != nil {
		return "", err
	}
	return result.Output, nil
}

// runParallelGroup 以 maxParallel 为上限并发执行一个组，并按步骤顺序返回已完成结果。
// 失败时返回失败步骤的名称与错误；失败之后的结果不会进入摘要。
func runParallelGroup(
	ctx context.Context,
	runner rig.SubagentRunner,
	group []Step,
	outputs map[string]string,
	previous string,
	maxParallel int,
) ([]stepResult, string, error) {
	type outcome struct {
		index  int
		output string
		err    error
	}
	slots := make(chan struct{}, maxParallel)
	outcomes := make([]outcome, len(group))
	var waitGroup sync.WaitGroup
	for index, step := range group {
		waitGroup.Go(func() {
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				outcomes[index] = outcome{index: index, err: ctx.Err()}
				return
			}
			defer func() { <-slots }()
			prompt := renderTemplate(step.Prompt, outputs, previous)
			output, err := runStep(ctx, runner, step.Name, prompt)
			outcomes[index] = outcome{index: index, output: output, err: err}
		})
	}
	waitGroup.Wait()

	results := make([]stepResult, 0, len(group))
	for _, result := range outcomes {
		if result.err != nil {
			return results, group[result.index].Name, result.err
		}
		results = append(results, stepResult{Name: group[result.index].Name, Output: result.output})
	}
	return results, "", nil
}

// renderTemplate 展开单个步骤提示词中的 {{prev}} 与 {{steps.<name>}}。
// 先替换较长的名称，避免某个名称是另一个名称的前缀时破坏展开结果。
func renderTemplate(prompt string, outputs map[string]string, previous string) string {
	expanded := strings.ReplaceAll(prompt, "{{prev}}", previous)
	names := make([]string, 0, len(outputs))
	for name := range outputs {
		names = append(names, name)
	}
	slices.SortFunc(names, func(a, b string) int {
		return cmp.Or(cmp.Compare(len(b), len(a)), strings.Compare(a, b))
	})
	for _, name := range names {
		expanded = strings.ReplaceAll(expanded, "{{steps."+name+"}}", outputs[name])
	}
	return expanded
}

// successResult 渲染正常完成的工作流。
func successResult(results []stepResult) rig.ToolResult {
	return rig.ToolResult{Content: renderResults(results, "", nil)}
}

// failureResult 渲染已完成步骤及失败的步骤。
func failureResult(results []stepResult, failedName string, err error) rig.ToolResult {
	return rig.ToolResult{
		Content: renderResults(results, failedName, err),
		IsError: true,
	}
}

// renderResults 格式化摘要：先列出所有已完成步骤，失败时再给出失败步骤及其错误。
func renderResults(results []stepResult, failedName string, failure error) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "workflow: %d step(s) completed", len(results))
	for _, result := range results {
		fmt.Fprintf(&builder, "\n\n== %s ==\n%s", result.Name, strings.TrimSpace(result.Output))
	}
	if failure != nil {
		fmt.Fprintf(&builder, "\n\nFAILED at step %q: %v", failedName, failure)
	}
	return builder.String()
}
