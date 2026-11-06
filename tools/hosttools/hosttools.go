// Package hosttools provides the SDK versions of the tools that need a host
// capability: ask_user, present, plan_review, exit_plan_mode and goal.
//
// The tools are headless: they depend only on the frozen root-package host
// interfaces (rig.Asker, rig.Presenter, rig.PlanController, rig.GoalStore) and
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
	"fmt"
	"reflect"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
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
	Asker rig.Asker
	// Presenter shows present artifacts and returns their identifier.
	Presenter rig.Presenter
	// Plan is the plan-mode controller behind plan_review/exit_plan_mode.
	Plan rig.PlanController
	// Goal stores the session goal behind the goal tool.
	Goal rig.GoalStore
	// Interactive enables ask_user. When false (headless runs, evaluations,
	// batch jobs) ask_user returns "no interactive UI" without calling Asker.
	Interactive bool
}

// Tools returns the five host-capability tools for opts. Tools are stateless and
// safe for concurrent use.
func Tools(opts Options) []rig.Tool {
	return []rig.Tool{
		askUserTool(opts),
		presentTool(opts),
		planReviewTool(opts),
		exitPlanModeTool(opts),
		goalTool(opts),
	}
}

// hostTool is the common rig.Tool implementation of the host tool set.
type hostTool struct {
	name        string
	description string
	parameters  map[string]any
	run         func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error)
}

func (t *hostTool) Name() string           { return t.name }
func (t *hostTool) Description() string    { return t.description }
func (t *hostTool) Schema() map[string]any { return t.parameters }
func (t *hostTool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	return t.run(ctx, call)
}

type askUserArgs struct {
	Question string   `json:"question"`
	Options  []string `json:"options"`
}

func askUserTool(opts Options) rig.Tool {
	return &hostTool{
		name: ToolAskUser,
		description: "Ask the human a question and return their answer. Use it when a decision cannot be made " +
			"from the code or the conversation and the user is available. options (optional) lists the choices " +
			"to present; the answer is plain text. This tool is unavailable in non-interactive runs.",
		parameters: toolutil.ObjectSchema(map[string]any{
			"question": toolutil.StringProp("The question to ask the user. Be specific and self-contained."),
			"options": map[string]any{
				"type":        "array",
				"description": "Optional list of choices for the user (for example [\"yes\",\"no\"]).",
				"items":       map[string]any{"type": "string"},
			},
		}, "question"),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args askUserArgs
			if err := toolutil.DecodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			if strings.TrimSpace(args.Question) == "" {
				return toolutil.ErrorResultf("ask_user: question is required"), nil
			}
			if !opts.Interactive {
				return toolutil.ErrorResultf("ask_user: no interactive UI in this run; interactive questions are disabled"), nil
			}
			if isNil(opts.Asker) {
				return toolutil.ErrorResultf("ask_user: the host did not provide a rig.Asker implementation"), nil
			}
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			answer, err := opts.Asker.Ask(ctx, args.Question, args.Options)
			if err != nil {
				return toolutil.ErrorResultf("ask_user: %v", err), nil
			}
			if strings.TrimSpace(answer) == "" {
				return toolutil.TextResult("(the user gave an empty answer)"), nil
			}
			return toolutil.TextResult(answer), nil
		},
	}
}

type presentArgs struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

func presentTool(opts Options) rig.Tool {
	return &hostTool{
		name: ToolPresent,
		description: "Present an artifact (a document, diff, table or report) to the human and return its artifact " +
			"id. Use it for content the user should see rendered rather than read in the chat text.",
		parameters: toolutil.ObjectSchema(map[string]any{
			"title": toolutil.StringProp("Short artifact title shown in the UI."),
			"body":  toolutil.StringProp("Artifact content (Markdown is typical). May be omitted for an empty artifact."),
		}, "title"),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args presentArgs
			if err := toolutil.DecodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			if strings.TrimSpace(args.Title) == "" {
				return toolutil.ErrorResultf("present: title is required"), nil
			}
			if isNil(opts.Presenter) {
				return toolutil.ErrorResultf("present: the host did not provide a rig.Presenter implementation"), nil
			}
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			artifactID, err := opts.Presenter.Present(ctx, args.Title, args.Body)
			if err != nil {
				return toolutil.ErrorResultf("present: %v", err), nil
			}
			return toolutil.TextResult(artifactID), nil
		},
	}
}

type planReviewArgs struct {
	Content string `json:"content"`
}

func planReviewTool(opts Options) rig.Tool {
	return &hostTool{
		name: ToolPlanReview,
		description: "Submit a plan to the host for review and return the verdict. Plan mode usually starts from a " +
			"plan_review call; use exit_plan_mode to approve, cancel or query the plan state.",
		parameters: toolutil.ObjectSchema(map[string]any{
			"content": toolutil.StringProp("The plan text (goals, steps, risks) for the reviewer to evaluate."),
		}, "content"),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args planReviewArgs
			if err := toolutil.DecodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			if strings.TrimSpace(args.Content) == "" {
				return toolutil.ErrorResultf("plan_review: content is required"), nil
			}
			if isNil(opts.Plan) {
				return toolutil.ErrorResultf("plan_review: the host did not provide a rig.PlanController implementation"), nil
			}
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			verdict, err := opts.Plan.Review(ctx, args.Content)
			if err != nil {
				return toolutil.ErrorResultf("plan_review: %v", err), nil
			}
			if strings.TrimSpace(verdict) == "" {
				return toolutil.TextResult("(reviewer returned an empty verdict)"), nil
			}
			return toolutil.TextResult(verdict), nil
		},
	}
}

type exitPlanModeArgs struct {
	Action string `json:"action"`
}

func exitPlanModeTool(opts Options) rig.Tool {
	return &hostTool{
		name: ToolExitPlanMode,
		description: "Act on plan mode: action=status returns the current state without changing it; " +
			"action=approve approves the reviewed plan and leaves the mode; action=cancel discards it and leaves " +
			"the mode.",
		parameters: toolutil.ObjectSchema(map[string]any{
			"action": map[string]any{
				"type":        "string",
				"description": "approve or cancel to leave plan mode, status to query it.",
				"enum":        []string{"approve", "cancel", "status"},
			},
		}, "action"),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args exitPlanModeArgs
			if err := toolutil.DecodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			action := strings.ToLower(strings.TrimSpace(args.Action))
			switch action {
			case "approve", "cancel", "status":
			default:
				return toolutil.ErrorResultf("exit_plan_mode: unknown action %q (want approve, cancel or status)", args.Action), nil
			}
			if isNil(opts.Plan) {
				return toolutil.ErrorResultf("exit_plan_mode: the host did not provide a rig.PlanController implementation"), nil
			}
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			if action == "status" {
				return toolutil.TextResult(opts.Plan.State()), nil
			}
			message, err := opts.Plan.Exit(ctx, action)
			if err != nil {
				return toolutil.ErrorResultf("exit_plan_mode: %v", err), nil
			}
			return toolutil.TextResult(message), nil
		},
	}
}

type goalArgs struct {
	Action string `json:"action"`
	Update string `json:"update"`
	Text   string `json:"text"`
}

func goalTool(opts Options) rig.Tool {
	return &hostTool{
		name: ToolGoal,
		description: "Manage the session goal. action=set stores (or replaces) the goal text and returns its id; " +
			"action=get returns the current goal; action=clear removes it; action=update moves the goal through " +
			"its lifecycle (complete/pause/resume/blocked/edit) when the host goal store supports updates. " +
			"The goal survives for the session and is visible to the host UI.",
		parameters: toolutil.ObjectSchema(map[string]any{
			"action": map[string]any{
				"type":        "string",
				"description": "set, get, clear or update.",
				"enum":        []string{"set", "get", "clear", "update"},
			},
			"update": map[string]any{
				"type":        "string",
				"description": "Lifecycle transition for action=update: complete, blocked, pause, resume or edit.",
				"enum":        []string{"complete", "blocked", "pause", "resume", "edit"},
			},
			"text": toolutil.StringProp("Goal text for action=set; the reason for update=blocked; the new objective for update=edit."),
		}, "action"),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args goalArgs
			if err := toolutil.DecodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			action := strings.ToLower(strings.TrimSpace(args.Action))
			switch action {
			case "set", "get", "clear", "update":
			default:
				return toolutil.ErrorResultf("goal: unknown action %q (want set, get, clear or update)", args.Action), nil
			}
			if action == "set" && strings.TrimSpace(args.Text) == "" {
				return toolutil.ErrorResultf("goal: text is required for action=set"), nil
			}
			if isNil(opts.Goal) {
				return toolutil.ErrorResultf("goal: the host did not provide a rig.GoalStore implementation"), nil
			}
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			switch action {
			case "set":
				id, err := opts.Goal.Set(args.Text)
				if err != nil {
					return toolutil.ErrorResultf("goal: %v", err), nil
				}
				return toolutil.TextResult(id), nil
			case "clear":
				if opts.Goal.Clear() {
					return toolutil.TextResult("goal cleared"), nil
				}
				return toolutil.TextResult("no goal to clear"), nil
			case "update":
				return invokeGoalUpdate(ctx, opts.Goal, args.Update, args.Text)
			default:
				id, text, status, ok := opts.Goal.Get()
				if !ok {
					return toolutil.TextResult("no goal set"), nil
				}
				return toolutil.TextResult(fmt.Sprintf("id: %s\ntext: %s\nstatus: %s", id, text, status)), nil
			}
		},
	}
}

// invokeGoalUpdate validates and applies one goal lifecycle transition. It is
// separated from the tool so the validation rules are easy to test: blocked
// requires a reason, edit requires new text, and the store must implement
// rig.GoalUpdater (a plain GoalStore cannot change lifecycle state).
func invokeGoalUpdate(ctx context.Context, store rig.GoalStore, update, text string) (rig.ToolResult, error) {
	action := strings.ToLower(strings.TrimSpace(update))
	switch action {
	case string(rig.GoalComplete), string(rig.GoalBlocked), string(rig.GoalPause),
		string(rig.GoalResume), string(rig.GoalEdit):
	default:
		return toolutil.ErrorResultf("goal: unknown update %q (want complete, blocked, pause, resume or edit)", update), nil
	}
	switch action {
	case string(rig.GoalBlocked):
		if strings.TrimSpace(text) == "" {
			return toolutil.ErrorResultf("goal: text (blocked reason) is required for update=blocked"), nil
		}
	case string(rig.GoalEdit):
		if strings.TrimSpace(text) == "" {
			return toolutil.ErrorResultf("goal: text (new objective) is required for update=edit"), nil
		}
	}
	updater, ok := store.(rig.GoalUpdater)
	if !ok || isNil(updater) {
		return toolutil.ErrorResultf("goal store does not support lifecycle updates (it does not implement rig.GoalUpdater)"), nil
	}
	status, err := updater.Update(ctx, rig.GoalUpdateAction(action), text)
	if err != nil {
		return toolutil.ErrorResultf("goal: %v", err), nil
	}
	if strings.TrimSpace(status) == "" {
		return toolutil.TextResult("(goal store returned an empty status)"), nil
	}
	return toolutil.TextResult(status), nil
}

// --- shared helpers --------------------------------------------------------

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
