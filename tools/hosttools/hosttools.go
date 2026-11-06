// Package hosttools provides the SDK versions of the tools that need a host
// capability: ask_user, present, plan_review, exit_plan_mode and goal.
//
// The tools are headless: they depend only on the frozen contracts/v1 host
// interfaces (v1.Asker, v1.Presenter, v1.PlanController, v1.GoalStore) and
// never import a UI or a store. A product injects its own implementations via
// Options; a standalone agent that just needs deterministic tools can use
// NewMemoryHost, whose *Host implements all four interfaces with
// concurrency-safe in-memory state.
//
// Every tool fails with a readable IsError when the host did not provide its
// interface: the model is told the capability is unavailable instead of the
// agent panicking. ask_user additionally honors Options.Interactive: a
// non-interactive run returns "no interactive UI" even when an Asker is wired.
//
// The tools are safe for concurrent calls; they hold no state themselves.
package hosttools

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
)

// Tool names. They are stable model-facing identifiers.
const (
	ToolAskUser      = "ask_user"
	ToolPresent      = "present"
	ToolPlanReview   = "plan_review"
	ToolExitPlanMode = "exit_plan_mode"
	ToolGoal         = "goal"
)

// Options wires the host capabilities used by the tools. Every field is
// optional: a nil interface makes the corresponding tool return a readable
// error instead of failing the run.
type Options struct {
	// Root is the workspace root the host tools refer to. It is reserved for
	// host stores that resolve workspace-relative paths; the SDK tools do not
	// touch the filesystem themselves.
	Root string
	// Asker answers ask_user questions (a UI, a chat prompt or a scripted test
	// host).
	Asker v1.Asker
	// Presenter shows present artifacts and returns their identifier.
	Presenter v1.Presenter
	// Plan is the plan-mode controller behind plan_review/exit_plan_mode.
	Plan v1.PlanController
	// Goal stores the session goal behind the goal tool.
	Goal v1.GoalStore
	// Interactive enables ask_user. When false (headless runs, evaluations,
	// batch jobs) ask_user returns "no interactive UI" without calling Asker.
	Interactive bool
}

// Tools returns the five host-capability tools for opts. Tools are stateless and
// safe for concurrent use.
func Tools(opts Options) []v1.Tool {
	return []v1.Tool{
		askUserTool(opts),
		presentTool(opts),
		planReviewTool(opts),
		exitPlanModeTool(opts),
		goalTool(opts),
	}
}

// hostTool is the common v1.Tool implementation of the host tool set.
type hostTool struct {
	name        string
	description string
	parameters  map[string]any
	run         func(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error)
}

func (t *hostTool) Name() string           { return t.name }
func (t *hostTool) Description() string    { return t.description }
func (t *hostTool) Schema() map[string]any { return t.parameters }
func (t *hostTool) Invoke(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error) {
	return t.run(ctx, call)
}

type askUserArgs struct {
	Question string   `json:"question"`
	Options  []string `json:"options"`
}

func askUserTool(opts Options) v1.Tool {
	return &hostTool{
		name: ToolAskUser,
		description: "Ask the human a question and return their answer. Use it when a decision cannot be made " +
			"from the code or the conversation and the user is available. options (optional) lists the choices " +
			"to present; the answer is plain text. This tool is unavailable in non-interactive runs.",
		parameters: objectSchema(map[string]any{
			"question": stringProp("The question to ask the user. Be specific and self-contained."),
			"options": map[string]any{
				"type":        "array",
				"description": "Optional list of choices for the user (for example [\"yes\",\"no\"]).",
				"items":       map[string]any{"type": "string"},
			},
		}, "question"),
		run: func(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error) {
			var args askUserArgs
			if err := decodeArgs(call, &args); err != nil {
				return v1.ToolResult{}, err
			}
			if strings.TrimSpace(args.Question) == "" {
				return errorResult("ask_user: question is required"), nil
			}
			if !opts.Interactive {
				return errorResult("ask_user: no interactive UI in this run; interactive questions are disabled"), nil
			}
			if isNil(opts.Asker) {
				return errorResult("ask_user: the host did not provide a v1.Asker implementation"), nil
			}
			if err := ctx.Err(); err != nil {
				return v1.ToolResult{}, err
			}
			answer, err := opts.Asker.Ask(ctx, args.Question, args.Options)
			if err != nil {
				return errorResult("ask_user: %v", err), nil
			}
			if strings.TrimSpace(answer) == "" {
				return textResult("(the user gave an empty answer)"), nil
			}
			return textResult(answer), nil
		},
	}
}

type presentArgs struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

func presentTool(opts Options) v1.Tool {
	return &hostTool{
		name: ToolPresent,
		description: "Present an artifact (a document, diff, table or report) to the human and return its artifact " +
			"id. Use it for content the user should see rendered rather than read in the chat text.",
		parameters: objectSchema(map[string]any{
			"title": stringProp("Short artifact title shown in the UI."),
			"body":  stringProp("Artifact content (Markdown is typical). May be omitted for an empty artifact."),
		}, "title"),
		run: func(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error) {
			var args presentArgs
			if err := decodeArgs(call, &args); err != nil {
				return v1.ToolResult{}, err
			}
			if strings.TrimSpace(args.Title) == "" {
				return errorResult("present: title is required"), nil
			}
			if isNil(opts.Presenter) {
				return errorResult("present: the host did not provide a v1.Presenter implementation"), nil
			}
			if err := ctx.Err(); err != nil {
				return v1.ToolResult{}, err
			}
			artifactID, err := opts.Presenter.Present(ctx, args.Title, args.Body)
			if err != nil {
				return errorResult("present: %v", err), nil
			}
			return textResult(artifactID), nil
		},
	}
}

type planReviewArgs struct {
	Content string `json:"content"`
}

func planReviewTool(opts Options) v1.Tool {
	return &hostTool{
		name: ToolPlanReview,
		description: "Submit a plan to the host for review and return the verdict. Plan mode usually starts from a " +
			"plan_review call; use exit_plan_mode to approve, cancel or query the plan state.",
		parameters: objectSchema(map[string]any{
			"content": stringProp("The plan text (goals, steps, risks) for the reviewer to evaluate."),
		}, "content"),
		run: func(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error) {
			var args planReviewArgs
			if err := decodeArgs(call, &args); err != nil {
				return v1.ToolResult{}, err
			}
			if strings.TrimSpace(args.Content) == "" {
				return errorResult("plan_review: content is required"), nil
			}
			if isNil(opts.Plan) {
				return errorResult("plan_review: the host did not provide a v1.PlanController implementation"), nil
			}
			if err := ctx.Err(); err != nil {
				return v1.ToolResult{}, err
			}
			verdict, err := opts.Plan.Review(ctx, args.Content)
			if err != nil {
				return errorResult("plan_review: %v", err), nil
			}
			if strings.TrimSpace(verdict) == "" {
				return textResult("(reviewer returned an empty verdict)"), nil
			}
			return textResult(verdict), nil
		},
	}
}

type exitPlanModeArgs struct {
	Action string `json:"action"`
}

func exitPlanModeTool(opts Options) v1.Tool {
	return &hostTool{
		name: ToolExitPlanMode,
		description: "Act on plan mode: action=status returns the current state without changing it; " +
			"action=approve approves the reviewed plan and leaves the mode; action=cancel discards it and leaves " +
			"the mode.",
		parameters: objectSchema(map[string]any{
			"action": map[string]any{
				"type":        "string",
				"description": "approve or cancel to leave plan mode, status to query it.",
				"enum":        []string{"approve", "cancel", "status"},
			},
		}, "action"),
		run: func(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error) {
			var args exitPlanModeArgs
			if err := decodeArgs(call, &args); err != nil {
				return v1.ToolResult{}, err
			}
			action := strings.ToLower(strings.TrimSpace(args.Action))
			switch action {
			case "approve", "cancel", "status":
			default:
				return errorResult("exit_plan_mode: unknown action %q (want approve, cancel or status)", args.Action), nil
			}
			if isNil(opts.Plan) {
				return errorResult("exit_plan_mode: the host did not provide a v1.PlanController implementation"), nil
			}
			if err := ctx.Err(); err != nil {
				return v1.ToolResult{}, err
			}
			if action == "status" {
				return textResult(opts.Plan.State()), nil
			}
			message, err := opts.Plan.Exit(ctx, action)
			if err != nil {
				return errorResult("exit_plan_mode: %v", err), nil
			}
			return textResult(message), nil
		},
	}
}

type goalArgs struct {
	Action string `json:"action"`
	Text   string `json:"text"`
}

func goalTool(opts Options) v1.Tool {
	return &hostTool{
		name: ToolGoal,
		description: "Manage the session goal: action=set stores (or replaces) the goal text and returns its id; " +
			"action=get returns the current goal; action=clear removes it. The goal survives for the session and " +
			"is visible to the host UI.",
		parameters: objectSchema(map[string]any{
			"action": map[string]any{
				"type":        "string",
				"description": "set, get or clear.",
				"enum":        []string{"set", "get", "clear"},
			},
			"text": stringProp("Goal text; required for action=set."),
		}, "action"),
		run: func(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error) {
			var args goalArgs
			if err := decodeArgs(call, &args); err != nil {
				return v1.ToolResult{}, err
			}
			action := strings.ToLower(strings.TrimSpace(args.Action))
			switch action {
			case "set", "get", "clear":
			default:
				return errorResult("goal: unknown action %q (want set, get or clear)", args.Action), nil
			}
			if action == "set" && strings.TrimSpace(args.Text) == "" {
				return errorResult("goal: text is required for action=set"), nil
			}
			if isNil(opts.Goal) {
				return errorResult("goal: the host did not provide a v1.GoalStore implementation"), nil
			}
			if err := ctx.Err(); err != nil {
				return v1.ToolResult{}, err
			}
			switch action {
			case "set":
				id, err := opts.Goal.Set(args.Text)
				if err != nil {
					return errorResult("goal: %v", err), nil
				}
				return textResult(id), nil
			case "clear":
				if opts.Goal.Clear() {
					return textResult("goal cleared"), nil
				}
				return textResult("no goal to clear"), nil
			default:
				id, text, status, ok := opts.Goal.Get()
				if !ok {
					return textResult("no goal set"), nil
				}
				return textResult(fmt.Sprintf("id: %s\ntext: %s\nstatus: %s", id, text, status)), nil
			}
		},
	}
}

// --- shared helpers --------------------------------------------------------

func decodeArgs(call v1.ToolCall, target any) error {
	raw := strings.TrimSpace(call.Arguments)
	if raw == "" {
		raw = "{}"
	}
	if err := json.Unmarshal([]byte(raw), target); err != nil {
		return fmt.Errorf("invalid arguments JSON: %w", err)
	}
	return nil
}

func textResult(content string) v1.ToolResult { return v1.ToolResult{Content: content} }

func errorResult(format string, args ...any) v1.ToolResult {
	return v1.ToolResult{Content: fmt.Sprintf(format, args...), IsError: true}
}

// isNil reports whether an interface value is nil, including a typed nil
// pointer that would panic when its method is called.
func isNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func objectSchema(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func stringProp(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}
