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

// DSH 目标工具名。它们是稳定的模型可见标识。
const (
	// ToolGetGoal 是读取工具名。
	ToolGetGoal = "get_goal"
	// ToolCreateGoal 是创建工具名。
	ToolCreateGoal = "create_goal"
	// ToolUpdateGoal 是更新工具名。
	ToolUpdateGoal = "update_goal"
	// DefaultMaxGoalRounds 是 create_goal 未提供 max_goal_rounds 时应用的 DSH 默认自动
	// 续跑轮次上限。
	DefaultMaxGoalRounds = 256
)

// blockedReasonCode 是模型可见 blocked 动作记录的稳定小写短横线格式代码，与 DSH 的
// 模型报告阻塞原因一致。
const blockedReasonCode = "model-reported"

// GoalPhase 是会话目标的 DSH 持久生命周期阶段。
type GoalPhase string

const (
	// GoalPhaseActive 表示活跃目标。
	GoalPhaseActive GoalPhase = "active"
	// GoalPhasePaused 表示已暂停目标。
	GoalPhasePaused GoalPhase = "paused"
	// GoalPhaseBlocked 表示被阻塞目标。
	GoalPhaseBlocked GoalPhase = "blocked"
	// GoalPhaseComplete 表示已完成目标。
	GoalPhaseComplete GoalPhase = "complete"
)

// GoalActivation 是 DSH 的实时续跑状态。它是观测值，不是持久状态。
type GoalActivation string

const (
	// GoalActivationArmed 表示自动续跑可以执行该目标。
	GoalActivationArmed GoalActivation = "armed"
	// GoalActivationDisarmed 表示自动续跑已关闭。
	GoalActivationDisarmed GoalActivation = "disarmed"
)

// GoalBlockedReason 是挂在被阻塞目标上的、既可机器路由又可人读的说明。
type GoalBlockedReason struct {
	// Code 是稳定的小写短横线格式分类。
	Code string `json:"code"`
	// Message 是具体说明。
	Message string `json:"message"`
}

// GoalSnapshot 是当前目标的 DSH 兼容视图，包含 update_goal 所需的 revision。
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

// GoalRef 是 update_goal 所需的精确 compare-and-set 身份。
type GoalRef struct {
	ID       string `json:"goal_id"`
	Revision int    `json:"revision"`
}

// GoalUpdate 是一次校验过的目标变更。零值/空字段表示“省略”：Objective 与
// MaxGoalRounds 仅在 action=edit 时有效，BlockedReason 仅在 action=blocked 时必需。
type GoalUpdate struct {
	Action        rig.GoalUpdateAction
	Objective     string
	MaxGoalRounds int
	BlockedReason string
}

// GoalRevisionStore 是 rig.GoalStore 可选的 DSH 修订能力：额外实现它的 store 会获得
// DSH 同名的 get_goal/create_goal/update_goal 工具。普通 rig.GoalStore 仍可继续使用
// 旧版 `goal` 工具；DSH 命名工具会报告修订/CAS 状态不可用。
//
// 实现必须可安全并发调用，并且必须拒绝与当前 idle/revision 对不完全匹配的 GoalRef。
type GoalRevisionStore interface {
	// CurrentGoal 返回调用方上下文的当前目标；没有目标时 ok=false。ctx 携带调用
	// agent 的身份，多会话宿主据此返回该会话自己的目标。
	CurrentGoal(ctx context.Context) (snapshot GoalSnapshot, ok bool)
	// CreateGoal 以给定目标与轮次上限启动一个活跃目标。maxGoalRounds <= 0 表示使用实现
	// 的默认值。
	CreateGoal(ctx context.Context, objective string, maxGoalRounds int) (GoalSnapshot, error)
	// UpdateGoal 对精确的 ref 应用 update，并在成功后递增修订号。
	UpdateGoal(ctx context.Context, ref GoalRef, update GoalUpdate) (GoalSnapshot, error)
}

// 编译期证明：进程内 host 实现了 DSH 修订存储。
var _ GoalRevisionStore = (*Host)(nil)

// goalToolValue 是规范模型结果中的单个目标。
type goalToolValue struct {
	ID            string             `json:"id"`
	Revision      int                `json:"revision"`
	Objective     string             `json:"objective"`
	Phase         GoalPhase          `json:"phase"`
	RoundsStarted int                `json:"roundsStarted"`
	MaxGoalRounds int                `json:"maxGoalRounds"`
	BlockedReason *GoalBlockedReason `json:"blockedReason,omitempty"`
}

// goalToolOutput 是规范模型结果：{"goal":null} 或
// {"goal":{...},"activation":"..."}。
type goalToolOutput struct {
	Goal       *goalToolValue `json:"goal"`
	Activation GoalActivation `json:"activation,omitempty"`
}

// getGoalTool 实现 get_goal。
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
			snapshot, ok := store.CurrentGoal(ctx)
			return goalResult(ToolGetGoal, snapshot, ok)
		},
	}
}

// createGoalArgs 是 create_goal 解码后的参数对象。
type createGoalArgs struct {
	Objective     string `json:"objective"`
	MaxGoalRounds int    `json:"max_goal_rounds"`
}

// createGoalTool 实现 create_goal。
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

// updateGoalArgs 是 update_goal 解码后的参数对象。
type updateGoalArgs struct {
	GoalID        string `json:"goal_id"`
	Revision      int    `json:"revision"`
	Action        string `json:"action"`
	Objective     string `json:"objective"`
	MaxGoalRounds int    `json:"max_goal_rounds"`
	BlockedReason string `json:"blocked_reason"`
}

// updateGoalTool 实现 update_goal。
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

// parseGoalUpdate 校验 update_goal 参数并构建 CAS 请求。它遵循 DSH 工具规则：有意义的
// （非空）可选值只能与其对应 action 搭配，blocked 必须带原因。
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

// dshGoalStore 解析 opts.Goal 背后的可选 DSH 修订能力。能力不可用时返回可读的工具
// 错误结果。
func dshGoalStore(opts Options, toolName, operation string) (GoalRevisionStore, *rig.ToolResult) {
	if !isNil(opts.GoalRevision) {
		return opts.GoalRevision, nil
	}
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

// goalResult 渲染一个规范的 DSH 目标结果。
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

// --- 进程内 Host 对 GoalRevisionStore 的实现 -----------------------------

// CurrentGoal 实现 GoalRevisionStore。
func (h *Host) CurrentGoal(_ context.Context) (GoalSnapshot, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.goalID == "" {
		return GoalSnapshot{}, false
	}
	return h.goalSnapshotLocked(), true
}

// CreateGoal 实现 GoalRevisionStore。已完成的目标可以被替换；其他任何当前阶段都必须
// 先清除或恢复，与 DSH 行为一致。
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

// UpdateGoal 实现 GoalRevisionStore：先比较精确修订号，再应用一个 DSH 生命周期动作，
// 成功后递增修订号。
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

// goalPhaseLocked 把旧版存储状态映射到 DSH 阶段词汇。
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

// goalSnapshotLocked 构建当前目标的 DSH 视图。
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

// bumpGoalRevisionLocked 在一次被接受的变更后推进修订号。
func (h *Host) bumpGoalRevisionLocked() {
	if h.goalRevision < 1 {
		h.goalRevision = 1
	}
	h.goalRevision++
}
