// DSH-compatible goal tools: get_goal, create_goal and update_goal.
//
// DeepSeek Harness exposes three goal tools over a revisioned compare-and-set
// goal domain: get_goal reads the current goal (including the exact id and
// revision), create_goal creates one, and update_goal applies one lifecycle
// action to an exact revision. Rig's frozen rig.GoalStore contract only exposes
// the goal text/status, so these tools use an optional capability a wired
// rig.GoalStore may also implement:
//
//	hosttools.GoalRevisionStore
//
// hosttools.NewMemoryHost implements it, as should a product that wants full DSH
// parity. A store that implements only rig.GoalStore/rig.GoalUpdater keeps the
// legacy `goal` tool but makes the three DSH-named tools report a readable
// "does not implement GoalRevisionStore" error instead of pretending revision
// state exists.
//
// The canonical model result is compact JSON, matching DSH:
//
//	{"goal":null}                                                        (no goal)
//	{"goal":{...},"activation":"armed"|"disarmed"}                      (current goal)
//
// Authority rules that depend on a live agent driver (a direct human turn, an
// active goal round, the consecutive-round self-block threshold) are not
// enforceable by a headless SDK tool; only the structural DSH rules are
// enforced here.
package hosttools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// DSH goal tool names. They are stable model-facing identifiers.
const (
	// ToolGetGoal is the read tool name.
	ToolGetGoal = "get_goal"
	// ToolCreateGoal is the create tool name.
	ToolCreateGoal = "create_goal"
	// ToolUpdateGoal is the update tool name.
	ToolUpdateGoal = "update_goal"
	// DefaultMaxGoalRounds is the DSH default automatic-continuation round cap
	// applied when create_goal does not supply max_goal_rounds.
	DefaultMaxGoalRounds = 256
)

// blockedReasonCode is the stable lower-kebab-case code the model-facing
// blocked action records, matching DSH's model-reported blocker.
const blockedReasonCode = "model-reported"

// GoalPhase is the DSH durable lifecycle phase of a session goal.
type GoalPhase string

const (
	// GoalPhaseActive is an active goal.
	GoalPhaseActive GoalPhase = "active"
	// GoalPhasePaused is a paused goal.
	GoalPhasePaused GoalPhase = "paused"
	// GoalPhaseBlocked is a blocked goal.
	GoalPhaseBlocked GoalPhase = "blocked"
	// GoalPhaseComplete is a completed goal.
	GoalPhaseComplete GoalPhase = "complete"
)

// GoalActivation is the DSH live continuation state. It is an observation, not
// durable state.
type GoalActivation string

const (
	// GoalActivationArmed means automatic continuation may run the goal.
	GoalActivationArmed GoalActivation = "armed"
	// GoalActivationDisarmed means automatic continuation is off.
	GoalActivationDisarmed GoalActivation = "disarmed"
)

// GoalBlockedReason is the DSH machine-routable and human-readable explanation
// attached to a blocked goal.
type GoalBlockedReason struct {
	// Code is a stable lower-kebab-case classification.
	Code string `json:"code"`
	// Message is the concrete explanation.
	Message string `json:"message"`
}

// GoalSnapshot is the DSH-compatible view of the current goal, including the
// revision that update_goal requires.
type GoalSnapshot struct {
	ID            string             `json:"id"`
	Revision      int                `json:"revision"`
	Objective     string             `json:"objective"`
	Phase         GoalPhase          `json:"phase"`
	RoundsStarted int                `json:"roundsStarted"`
	MaxGoalRounds int                `json:"maxGoalRounds"`
	BlockedReason *GoalBlockedReason `json:"blockedReason,omitempty"`
	Activation    GoalActivation     `json:"activation,omitempty"`
}

// GoalRef is the exact compare-and-set identity update_goal requires.
type GoalRef struct {
	ID       string `json:"goal_id"`
	Revision int    `json:"revision"`
}

// GoalUpdate is one validated goal mutation. Zero/empty fields mean "omitted":
// Objective and MaxGoalRounds are valid only with action edit, and BlockedReason
// is required only with action blocked.
type GoalUpdate struct {
	Action        rig.GoalUpdateAction
	Objective     string
	MaxGoalRounds int
	BlockedReason string
}

// GoalRevisionStore is the optional DSH revision half of a rig.GoalStore: a
// store that also implements it gets the DSH-named get_goal/create_goal/
// update_goal tools. A plain rig.GoalStore keeps working with the legacy `goal`
// tool; the DSH tools report that revision/CAS state is unavailable.
//
// Implementations must be safe for concurrent calls and must reject a GoalRef
// that does not exactly match the current idle/revision pair.
type GoalRevisionStore interface {
	// CurrentGoal returns the current goal, or ok=false when none is current.
	CurrentGoal() (snapshot GoalSnapshot, ok bool)
	// CreateGoal starts an active goal with the given objective and round cap.
	// maxGoalRounds <= 0 requests the implementation's default.
	CreateGoal(ctx context.Context, objective string, maxGoalRounds int) (GoalSnapshot, error)
	// UpdateGoal applies update to the exact ref, incrementing the revision.
	UpdateGoal(ctx context.Context, ref GoalRef, update GoalUpdate) (GoalSnapshot, error)
}

// Compile-time proof that the in-memory host implements the DSH revision store.
var _ GoalRevisionStore = (*Host)(nil)

// goalToolValue is one goal in the canonical model result.
type goalToolValue struct {
	ID            string             `json:"id"`
	Revision      int                `json:"revision"`
	Objective     string             `json:"objective"`
	Phase         GoalPhase          `json:"phase"`
	RoundsStarted int                `json:"roundsStarted"`
	MaxGoalRounds int                `json:"maxGoalRounds"`
	BlockedReason *GoalBlockedReason `json:"blockedReason,omitempty"`
}

// goalToolOutput is the canonical model result: {"goal":null} or
// {"goal":{...},"activation":"..."}.
type goalToolOutput struct {
	Goal       *goalToolValue `json:"goal"`
	Activation GoalActivation `json:"activation,omitempty"`
}

// getGoalTool implements get_goal.
func getGoalTool(opts Options) rig.Tool {
	return &hostTool{
		name: ToolGetGoal,
		description: "Read the current session goal, including the id and revision that update_goal requires. " +
			"Returns {\"goal\":null} when no goal is current.",
		parameters: toolutil.ObjectSchema(map[string]any{}),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args struct{}
			if err := toolutil.DecodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			store, unavailable := dshGoalStore(opts, ToolGetGoal, "read")
			if unavailable != nil {
				return *unavailable, nil
			}
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			snapshot, ok := store.CurrentGoal()
			return goalResult(ToolGetGoal, snapshot, ok)
		},
	}
}

// createGoalArgs is the decoded argument object of create_goal.
type createGoalArgs struct {
	Objective     string `json:"objective"`
	MaxGoalRounds int    `json:"max_goal_rounds"`
}

// createGoalTool implements create_goal.
func createGoalTool(opts Options) rig.Tool {
	return &hostTool{
		name: ToolCreateGoal,
		description: "Create a persisted goal that keeps this session working across automatic continuation rounds. " +
			"Use it when the direct human request is a long-running objective, even if the user did not say \"goal\"; " +
			"not for single-turn work.",
		parameters: toolutil.ObjectSchema(map[string]any{
			"objective": toolutil.StringProp("The concrete completion objective inferred from the direct human request."),
			"max_goal_rounds": toolutil.IntegerProp(fmt.Sprintf(
				"Optional positive integer limit on automatic continuation rounds; defaults to %d.", DefaultMaxGoalRounds)),
		}, "objective"),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args createGoalArgs
			if err := toolutil.DecodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			objective := strings.TrimSpace(args.Objective)
			if objective == "" {
				return toolutil.ErrorResultf("%s: objective is required and must be non-empty after trimming", ToolCreateGoal), nil
			}
			if args.MaxGoalRounds < 0 {
				return toolutil.ErrorResultf("%s: max_goal_rounds must be a positive integer", ToolCreateGoal), nil
			}
			store, unavailable := dshGoalStore(opts, ToolCreateGoal, "create")
			if unavailable != nil {
				return *unavailable, nil
			}
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			snapshot, err := store.CreateGoal(ctx, objective, args.MaxGoalRounds)
			if err != nil {
				return toolutil.ErrorResultf("%s: %v", ToolCreateGoal, err), nil
			}
			return goalResult(ToolCreateGoal, snapshot, true)
		},
	}
}

// updateGoalArgs is the decoded argument object of update_goal.
type updateGoalArgs struct {
	GoalID        string `json:"goal_id"`
	Revision      int    `json:"revision"`
	Action        string `json:"action"`
	Objective     string `json:"objective"`
	MaxGoalRounds int    `json:"max_goal_rounds"`
	BlockedReason string `json:"blocked_reason"`
}

// updateGoalTool implements update_goal.
func updateGoalTool(opts Options) rig.Tool {
	return &hostTool{
		name: ToolUpdateGoal,
		description: "Update the current goal on its exact revision. get_goal first and copy the exact goal_id and revision; " +
			"a stale pair is rejected instead of overwriting newer state. action=edit replaces the objective and/or round cap, " +
			"pause/resume/complete move the lifecycle, and blocked requires blocked_reason.",
		parameters: toolutil.ObjectSchema(map[string]any{
			"goal_id":  toolutil.StringProp("Exact id returned by get_goal."),
			"revision": toolutil.IntegerProp("Exact positive revision returned by get_goal."),
			"action": map[string]any{
				"type":        "string",
				"description": "Lifecycle action to apply to the exact goal revision.",
				"enum":        []string{string(rig.GoalEdit), string(rig.GoalPause), string(rig.GoalResume), string(rig.GoalComplete), string(rig.GoalBlocked)},
			},
			"objective":       toolutil.StringProp("Replacement objective; valid only with action edit."),
			"max_goal_rounds": toolutil.IntegerProp("Replacement round cap; valid only with action edit."),
			"blocked_reason":  toolutil.StringProp("Required only with action blocked: the concrete condition that persisted across rounds and blocks progress."),
		}, "goal_id", "revision", "action"),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args updateGoalArgs
			if err := toolutil.DecodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			ref, update, err := parseGoalUpdate(args)
			if err != nil {
				return toolutil.ErrorResultf("%s: %v", ToolUpdateGoal, err), nil
			}
			store, unavailable := dshGoalStore(opts, ToolUpdateGoal, "update")
			if unavailable != nil {
				return *unavailable, nil
			}
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			snapshot, err := store.UpdateGoal(ctx, ref, update)
			if err != nil {
				return toolutil.ErrorResultf("%s: %v", ToolUpdateGoal, err), nil
			}
			return goalResult(ToolUpdateGoal, snapshot, true)
		},
	}
}

// parseGoalUpdate validates the update_goal arguments and builds the CAS
// request. It mirrors the DSH tool rules: a meaningful (non-empty) optional
// value is valid only with its own action, and blocked requires a reason.
func parseGoalUpdate(args updateGoalArgs) (GoalRef, GoalUpdate, error) {
	goalID := args.GoalID
	if goalID == "" || strings.TrimSpace(goalID) != goalID {
		return GoalRef{}, GoalUpdate{}, errors.New("goal_id must be non-empty without surrounding whitespace")
	}
	if args.Revision < 1 {
		return GoalRef{}, GoalUpdate{}, errors.New("revision must be a positive integer")
	}
	action := rig.GoalUpdateAction(strings.ToLower(strings.TrimSpace(args.Action)))
	switch action {
	case rig.GoalEdit, rig.GoalPause, rig.GoalResume, rig.GoalComplete, rig.GoalBlocked:
	default:
		return GoalRef{}, GoalUpdate{}, fmt.Errorf("unknown action %q (want edit, pause, resume, complete or blocked)", args.Action)
	}
	objective := args.Objective
	hasObjective := objective != ""
	if hasObjective && strings.TrimSpace(objective) == "" {
		return GoalRef{}, GoalUpdate{}, errors.New("objective must be non-empty after trimming")
	}
	hasRoundCap := args.MaxGoalRounds != 0
	if args.MaxGoalRounds < 0 {
		return GoalRef{}, GoalUpdate{}, errors.New("max_goal_rounds must be a positive integer")
	}
	blockedReason := args.BlockedReason
	hasBlockedReason := blockedReason != ""
	switch action {
	case rig.GoalEdit:
		if hasBlockedReason {
			return GoalRef{}, GoalUpdate{}, errors.New("blocked_reason is valid only with action blocked")
		}
		if !hasObjective && !hasRoundCap {
			return GoalRef{}, GoalUpdate{}, errors.New("action edit requires objective and/or max_goal_rounds")
		}
	case rig.GoalPause, rig.GoalResume:
		if hasObjective || hasRoundCap || hasBlockedReason {
			return GoalRef{}, GoalUpdate{}, errors.New("objective and max_goal_rounds are valid only with action edit; blocked_reason is valid only with action blocked")
		}
	case rig.GoalComplete:
		if hasObjective || hasRoundCap {
			return GoalRef{}, GoalUpdate{}, errors.New("objective and max_goal_rounds are valid only with action edit")
		}
		if hasBlockedReason {
			return GoalRef{}, GoalUpdate{}, errors.New("blocked_reason is valid only with action blocked")
		}
	case rig.GoalBlocked:
		if hasObjective || hasRoundCap {
			return GoalRef{}, GoalUpdate{}, errors.New("objective and max_goal_rounds are valid only with action edit")
		}
		if strings.TrimSpace(blockedReason) == "" {
			return GoalRef{}, GoalUpdate{}, errors.New("blocked_reason is required with action blocked")
		}
	}
	return GoalRef{ID: goalID, Revision: args.Revision}, GoalUpdate{
		Action:        action,
		Objective:     objective,
		MaxGoalRounds: args.MaxGoalRounds,
		BlockedReason: blockedReason,
	}, nil
}

// dshGoalStore resolves the optional DSH revision capability behind opts.Goal.
// It returns a readable tool error result when the capability is unavailable.
func dshGoalStore(opts Options, toolName, operation string) (GoalRevisionStore, *rig.ToolResult) {
	if isNil(opts.Goal) {
		result := toolutil.ErrorResultf("%s: the host did not provide a rig.GoalStore implementation", toolName)
		return nil, &result
	}
	store, ok := opts.Goal.(GoalRevisionStore)
	if !ok || isNil(store) {
		result := toolutil.ErrorResultf("%s: the host goal store does not implement hosttools.GoalRevisionStore, which the DSH goal tools need for revision-checked %s; the legacy goal tool still works through rig.GoalStore/rig.GoalUpdater and hosttools.NewMemoryHost implements the revision store", toolName, operation)
		return nil, &result
	}
	return store, nil
}

// goalResult renders one canonical DSH goal result.
func goalResult(toolName string, snapshot GoalSnapshot, ok bool) (rig.ToolResult, error) {
	output := goalToolOutput{}
	if ok {
		output.Goal = &goalToolValue{
			ID:            snapshot.ID,
			Revision:      snapshot.Revision,
			Objective:     snapshot.Objective,
			Phase:         snapshot.Phase,
			RoundsStarted: snapshot.RoundsStarted,
			MaxGoalRounds: snapshot.MaxGoalRounds,
			BlockedReason: snapshot.BlockedReason,
		}
		output.Activation = snapshot.Activation
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		return toolutil.ErrorResultf("%s: %v", toolName, err), nil
	}
	return toolutil.TextResult(string(encoded)), nil
}

// --- in-memory Host implementation of GoalRevisionStore -------------------

// CurrentGoal implements GoalRevisionStore.
func (h *Host) CurrentGoal() (GoalSnapshot, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.goalID == "" {
		return GoalSnapshot{}, false
	}
	return h.goalSnapshotLocked(), true
}

// CreateGoal implements GoalRevisionStore. A completed goal may be replaced; any
// other current phase must be cleared or resumed instead, matching DSH.
func (h *Host) CreateGoal(ctx context.Context, objective string, maxGoalRounds int) (GoalSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return GoalSnapshot{}, err
	}
	objective = strings.TrimSpace(objective)
	if objective == "" {
		return GoalSnapshot{}, errors.New("hosttools: goal objective is required")
	}
	if maxGoalRounds < 0 {
		return GoalSnapshot{}, errors.New("hosttools: maxGoalRounds must be a positive integer")
	}
	if maxGoalRounds == 0 {
		maxGoalRounds = DefaultMaxGoalRounds
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.goalID != "" {
		if phase := h.goalPhaseLocked(); phase != GoalPhaseComplete {
			return GoalSnapshot{}, fmt.Errorf("hosttools: goal %q already exists with phase %q", h.goalID, phase)
		}
	}
	h.nextGoal++
	h.goalID = fmt.Sprintf("goal-%d", h.nextGoal)
	h.goalText = objective
	h.goalStatus = string(GoalPhaseActive)
	h.goalReason = ""
	h.goalRevision = 1
	h.goalRounds = 0
	h.goalMaxRounds = maxGoalRounds
	return h.goalSnapshotLocked(), nil
}

// UpdateGoal implements GoalRevisionStore: it compares the exact revision before
// applying one DSH lifecycle action and increments the revision on success.
func (h *Host) UpdateGoal(ctx context.Context, ref GoalRef, update GoalUpdate) (GoalSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return GoalSnapshot{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.goalID == "" {
		return GoalSnapshot{}, errors.New("hosttools: no goal is set")
	}
	if ref.ID != h.goalID || ref.Revision != h.goalRevision {
		return GoalSnapshot{}, fmt.Errorf("hosttools: stale goal ref %q revision %d; current is %q revision %d",
			ref.ID, ref.Revision, h.goalID, h.goalRevision)
	}
	phase := h.goalPhaseLocked()
	switch update.Action {
	case rig.GoalEdit:
		if update.Objective == "" && update.MaxGoalRounds == 0 {
			return GoalSnapshot{}, errors.New("hosttools: edit requires objective and/or max_goal_rounds")
		}
		if update.Objective != "" {
			objective := strings.TrimSpace(update.Objective)
			if objective == "" {
				return GoalSnapshot{}, errors.New("hosttools: goal objective must be non-empty")
			}
			h.goalText = objective
		}
		if update.MaxGoalRounds != 0 {
			if update.MaxGoalRounds < 1 {
				return GoalSnapshot{}, errors.New("hosttools: maxGoalRounds must be a positive integer")
			}
			h.goalMaxRounds = update.MaxGoalRounds
		}
	case rig.GoalPause:
		if phase != GoalPhaseActive {
			return GoalSnapshot{}, fmt.Errorf("hosttools: cannot pause goal %q from phase %q", h.goalID, phase)
		}
		h.goalStatus = string(GoalPhasePaused)
	case rig.GoalResume:
		switch phase {
		case GoalPhaseActive:
			return GoalSnapshot{}, fmt.Errorf("hosttools: goal %q is already active and armed", h.goalID)
		case GoalPhasePaused, GoalPhaseBlocked:
		default:
			return GoalSnapshot{}, fmt.Errorf("hosttools: cannot resume goal %q from phase %q", h.goalID, phase)
		}
		if h.goalRounds >= h.goalMaxRounds {
			return GoalSnapshot{}, fmt.Errorf("hosttools: goal %q exhausted %d goal rounds; increase maxGoalRounds before resuming", h.goalID, h.goalMaxRounds)
		}
		h.goalStatus = string(GoalPhaseActive)
		h.goalReason = ""
	case rig.GoalComplete:
		switch phase {
		case GoalPhaseActive, GoalPhasePaused, GoalPhaseBlocked:
		default:
			return GoalSnapshot{}, fmt.Errorf("hosttools: cannot complete goal %q from phase %q", h.goalID, phase)
		}
		h.goalStatus = "completed"
		h.goalReason = ""
	case rig.GoalBlocked:
		if phase != GoalPhaseActive {
			return GoalSnapshot{}, fmt.Errorf("hosttools: cannot block goal %q from phase %q", h.goalID, phase)
		}
		reason := strings.TrimSpace(update.BlockedReason)
		if reason == "" {
			return GoalSnapshot{}, errors.New("hosttools: blocked action requires a non-empty reason")
		}
		h.goalStatus = string(GoalPhaseBlocked)
		h.goalReason = reason
	default:
		return GoalSnapshot{}, fmt.Errorf("hosttools: unknown goal update %q", update.Action)
	}
	h.bumpGoalRevisionLocked()
	return h.goalSnapshotLocked(), nil
}

// goalPhaseLocked maps the legacy stored status onto the DSH phase vocabulary.
func (h *Host) goalPhaseLocked() GoalPhase {
	switch h.goalStatus {
	case "active":
		return GoalPhaseActive
	case "paused":
		return GoalPhasePaused
	case "blocked":
		return GoalPhaseBlocked
	case "completed", "complete":
		return GoalPhaseComplete
	default:
		return GoalPhase(h.goalStatus)
	}
}

// goalSnapshotLocked builds the DSH view of the current goal.
func (h *Host) goalSnapshotLocked() GoalSnapshot {
	phase := h.goalPhaseLocked()
	revision := h.goalRevision
	if revision < 1 {
		revision = 1
	}
	maxGoalRounds := h.goalMaxRounds
	if maxGoalRounds < 1 {
		maxGoalRounds = DefaultMaxGoalRounds
	}
	snapshot := GoalSnapshot{
		ID:            h.goalID,
		Revision:      revision,
		Objective:     h.goalText,
		Phase:         phase,
		RoundsStarted: h.goalRounds,
		MaxGoalRounds: maxGoalRounds,
		Activation:    GoalActivationDisarmed,
	}
	if phase == GoalPhaseActive {
		snapshot.Activation = GoalActivationArmed
	}
	if phase == GoalPhaseBlocked && strings.TrimSpace(h.goalReason) != "" {
		snapshot.BlockedReason = &GoalBlockedReason{Code: blockedReasonCode, Message: h.goalReason}
	}
	return snapshot
}

// bumpGoalRevisionLocked advances the revision after one accepted mutation.
func (h *Host) bumpGoalRevisionLocked() {
	if h.goalRevision < 1 {
		h.goalRevision = 1
	}
	h.goalRevision++
}
