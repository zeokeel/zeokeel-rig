// Package workflow exposes the in-process subagent runner as a model-callable
// workflow tool: an ordered (or partly parallel) list of steps, each executed by
// a fresh subagent, with the previous and named step outputs available to later
// prompts through simple templates.
//
// Templates:
//
//	{{prev}}          output of the previous completed step
//	{{steps.<name>}}  output of the named step
//
// Steps marked parallel:true and adjacent are run as one concurrent group; a
// group sees the outputs completed before it, not its siblings'. The runner's
// own concurrency limit still applies on top of max_parallel.
package workflow

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// ToolName is the model-facing name of the workflow tool.
const ToolName = "workflow"

// DefaultMaxParallel bounds one parallel group when max_parallel is omitted.
const DefaultMaxParallel = 4

// MaxMaxParallel is the largest accepted max_parallel.
const MaxMaxParallel = 8

// Step is one workflow step.
type Step struct {
	// Name identifies the step for templates, the summary and logs. Empty names
	// become "step-N".
	Name string `json:"name"`
	// Prompt is the subagent prompt; {{prev}} and {{steps.<name>}} are expanded
	// before it is sent.
	Prompt string `json:"prompt"`
	// Parallel marks contiguous steps that should run concurrently.
	Parallel bool `json:"parallel,omitempty"`
}

// New returns the workflow tool backed by runner. A nil runner produces a tool
// that reports a configuration error when invoked.
func New(runner rig.SubagentRunner) rig.Tool { return &tool{runner: runner} }

// NewTool is the named constructor; it is New.
func NewTool(runner rig.SubagentRunner) rig.Tool { return New(runner) }

type tool struct {
	runner rig.SubagentRunner
}

var _ rig.Tool = (*tool)(nil)

// Name implements rig.Tool.
func (t *tool) Name() string { return ToolName }

// Description implements rig.Tool.
func (t *tool) Description() string {
	return "Run a multi-step workflow, one subagent per step, and return every step's output. " +
		"Prompts may reference the previous step's output with {{prev}} and any earlier named step with " +
		"{{steps.<name>}}. Steps marked parallel:true and adjacent run concurrently (bounded by max_parallel). " +
		"On failure the tool returns the completed steps plus the failing step's error."
}

// Schema implements rig.Tool.
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

// workflowArgs is the argument object.
type workflowArgs struct {
	Steps       []Step `json:"steps"`
	Tasks       []Step `json:"tasks"`
	MaxParallel int    `json:"max_parallel"`
}

// stepResult is one completed step.
type stepResult struct {
	Name   string
	Output string
}

// Invoke implements rig.Tool.
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
	if maxParallel > MaxMaxParallel {
		maxParallel = MaxMaxParallel
	}

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

		// One contiguous parallel group.
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

// normalizeSteps fills default names and validates the step list.
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

// runStep starts one subagent, waits for its result and releases it.
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

// runParallelGroup runs one group concurrently, bounded by maxParallel, and
// returns the completed results in step order. On failure it reports the failed
// step's name and error; results after the failure are dropped from the summary.
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
		index, step := index, step
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
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
		}()
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

// renderTemplate expands {{prev}} and {{steps.<name>}} in one step prompt.
// Longer names are replaced first so a name that is a prefix of another cannot
// corrupt the expansion.
func renderTemplate(prompt string, outputs map[string]string, previous string) string {
	expanded := strings.ReplaceAll(prompt, "{{prev}}", previous)
	names := make([]string, 0, len(outputs))
	for name := range outputs {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if len(names[i]) != len(names[j]) {
			return len(names[i]) > len(names[j])
		}
		return names[i] < names[j]
	})
	for _, name := range names {
		expanded = strings.ReplaceAll(expanded, "{{steps."+name+"}}", outputs[name])
	}
	return expanded
}

// successResult renders the completed workflow.
func successResult(results []stepResult) rig.ToolResult {
	return rig.ToolResult{Content: renderResults(results, "", nil)}
}

// failureResult renders the completed steps plus the failing step.
func failureResult(results []stepResult, failedName string, err error) rig.ToolResult {
	return rig.ToolResult{
		Content: renderResults(results, failedName, err),
		IsError: true,
	}
}

// renderResults formats the summary: every completed step, then (on failure) the
// step that failed and its error.
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
