package agent

// 本文件实现 goal 回合驱动：把“一次回合结束、且会话仍有活跃目标”转换为一次
// 自动续跑，并执行每个会话/每个目标实例的续跑预算。它只保存计数器与配置，不拥有
// controller、UI 或持久化状态；真正的 Run 循环、GoalStore 读取与通知接线由调用方
// （kit/agent 装配层）完成。

import (
	"fmt"
	"strings"
	"sync"
)

const (
	// DefaultGoalRounds 是未配置 GoalRoundDriverConfig.MaxRounds 时的自动续跑
	// 预算，与 DSH 的 defaultMaxGoalRounds 一致（256）。
	DefaultGoalRounds = 256
	// MaxGoalRounds 是单次目标实例续跑预算的硬上限；更大的配置或运行时设置会被
	// 钳到该值，而不是被拒绝。它与 DSH 的目标轮次上限一致。
	MaxGoalRounds = 256
)

// GoalRoundState 是一次回合结束时对会话目标的最小快照。字段来自
// rig.GoalStore/GoalUpdater（或 hosttools 的 GoalRecord），由调用方读取后传入，
// 驱动本身不访问任何存储。
type GoalRoundState struct {
	// ID 是目标实例 id；旧目标可能为空。ID 稳定时用于区分“同一目标的进展更新”
	// 与“新目标”，但预算是否重置由调用方通过 changed 参数显式告知。
	ID string
	// Status 是目标生命周期状态：active、paused、blocked、complete 等。驱动对
	// 状态做 trim + 小写归一化后交给策略判断。
	Status string
	// Reason 是 blocked 等状态附带的可读原因，供 Continuable 与续跑 prompt 使用。
	Reason string
}

// GoalRoundDriverConfig 配置 GoalRoundDriver。零值可用：默认预算
// （DefaultGoalRounds）、默认续跑策略与默认 prompt。
type GoalRoundDriverConfig struct {
	// MaxRounds 是一个目标实例允许的自动续跑轮数。0 表示 DefaultGoalRounds；
	// 大于 MaxGoalRounds 的值会被钳到 MaxGoalRounds；负数同样回退到默认值。
	MaxRounds int
	// Continuable 判断某个目标状态是否还应继续自动推进；nil 表示使用
	// DefaultGoalContinuable（只有 active 继续）。参数已 trim；status 已转小写。
	Continuable func(status, reason string) bool
	// ContinuationPrompt 生成自动续跑使用的 user prompt；nil 表示使用
	// DefaultGoalPrompt。
	ContinuationPrompt func(goal GoalRoundState) string
}

// GoalRoundDriver 是 goal 回合驱动，可安全并发使用（零值也可用）。典型接线：
//
//   - 宿主在每个回合结束后读取目标快照，调用
//     TurnFinished(sessionID, goal, changed)；
//   - 返回 continueTurn=true 时，把 prompt 作为下一条 user 消息继续 agent.Run；
//   - 目标被新建/替换（changed=true）时预算重置；paused/blocked/complete 停止
//     自动续跑，人工恢复（resume）后由下一次 TurnFinished 重新判断；
//   - Rounds(sessionID) 可展示已用自动轮数，Reset 在会话结束或新目标时清理。
//
// 驱动不重试、不轮询：每次回合结束最多返回一次续跑决定。
type GoalRoundDriver struct {
	mu        sync.Mutex
	maxRounds int            // 配置的默认预算；0 表示 DefaultGoalRounds
	rounds    map[string]int // sessionID -> 已提交的自动续跑次数

	continuable        func(status, reason string) bool
	continuationPrompt func(goal GoalRoundState) string
}

// NewGoalRoundDriver 返回一个驱动。config 可选；多余参数会被忽略。
func NewGoalRoundDriver(configs ...GoalRoundDriverConfig) *GoalRoundDriver {
	cfg := GoalRoundDriverConfig{}
	if len(configs) > 0 {
		cfg = configs[0]
	}
	return &GoalRoundDriver{
		maxRounds:          cfg.MaxRounds,
		rounds:             make(map[string]int),
		continuable:        cfg.Continuable,
		continuationPrompt: cfg.ContinuationPrompt,
	}
}

// DefaultGoalContinuable 是默认续跑策略：只有 active 目标继续；paused、blocked、
// complete（以及任何未知/空状态）都停止，因此人工暂停、阻塞与完成不会被自动
// 续跑覆盖。
func DefaultGoalContinuable(status, reason string) bool {
	return normalizeGoalStatus(status) == "active"
}

// DefaultGoalPrompt 是默认续跑 prompt。它不包含目标正文（驱动不读取存储），需要
// 正文的宿主通过 ContinuationPrompt 注入。
func DefaultGoalPrompt(goal GoalRoundState) string {
	var builder strings.Builder
	builder.WriteString("Continue working toward the active goal")
	if id := strings.TrimSpace(goal.ID); id != "" {
		fmt.Fprintf(&builder, " (id: %s)", id)
	}
	if reason := strings.TrimSpace(goal.Reason); reason != "" {
		fmt.Fprintf(&builder, "; latest status note: %s", reason)
	}
	builder.WriteString(". Mark the goal complete when it is done.")
	return builder.String()
}

// Rounds 返回 sessionID 已提交的自动续跑次数。nil 接收者返回 0。
func (d *GoalRoundDriver) Rounds(sessionID string) int {
	if d == nil {
		return 0
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.rounds[sessionID]
}

// Reset 清空 sessionID 的续跑计数（新目标、新会话或人工重置时调用）。nil 接收者
// 与空 sessionID 都是安全的 no-op。
func (d *GoalRoundDriver) Reset(sessionID string) {
	if d == nil {
		return
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	d.mu.Lock()
	delete(d.rounds, sessionID)
	d.mu.Unlock()
}

// SetMaxRounds 修改默认续跑预算，对之后开始的判断生效。n<=0 恢复
// DefaultGoalRounds；n>MaxGoalRounds 会被钳到 MaxGoalRounds。
func (d *GoalRoundDriver) SetMaxRounds(n int) {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.maxRounds = n
	d.mu.Unlock()
}

// TurnFinished 在次回合结束后调用一次，返回是否应自动续跑以及要注入的 user
// prompt。changed=true 表示这是新目标（新建、替换或重新打开），会先重置该会话的
// 预算；同一目标实例的进展更新/编辑不重置。
//
// 返回 continueTurn=false 的常见原因：会话没有活跃目标（空/停用状态）、状态被
// paused/blocked/complete、预算已用尽，或 sessionID 为空。该调用不修改目标状态，
// 只有预算计数会变化。
func (d *GoalRoundDriver) TurnFinished(sessionID string, goal GoalRoundState, changed bool) (continueTurn bool, prompt string) {
	if d == nil {
		return false, ""
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return false, ""
	}
	if changed {
		d.Reset(sessionID)
	}

	d.mu.Lock()
	continuable := d.continuable
	promptFn := d.continuationPrompt
	limit := d.limitLocked()
	d.mu.Unlock()

	normalized := normalizeGoalStatus(goal.Status)
	if continuable == nil {
		if !DefaultGoalContinuable(normalized, goal.Reason) {
			return false, ""
		}
	} else if !continuable(normalized, goal.Reason) {
		return false, ""
	}

	d.mu.Lock()
	if d.rounds == nil {
		// 零值可用：首次写入时惰性初始化计数表。
		d.rounds = make(map[string]int)
	}
	used := d.rounds[sessionID]
	if used < 0 {
		used = 0
	}
	if used >= limit {
		d.mu.Unlock()
		return false, ""
	}
	d.rounds[sessionID] = used + 1
	d.mu.Unlock()

	normalizedGoal := goal
	normalizedGoal.ID = strings.TrimSpace(goal.ID)
	normalizedGoal.Status = normalized
	if promptFn != nil {
		prompt = promptFn(normalizedGoal)
	} else {
		prompt = DefaultGoalPrompt(normalizedGoal)
	}
	return true, prompt
}

// limitLocked 解析当前默认预算；调用方必须持有 d.mu。
func (d *GoalRoundDriver) limitLocked() int {
	limit := d.maxRounds
	if limit <= 0 {
		limit = DefaultGoalRounds
	}
	if limit > MaxGoalRounds {
		limit = MaxGoalRounds
	}
	if limit < 1 {
		limit = 1
	}
	return limit
}

// normalizeGoalStatus 归一化目标状态：trim 后转小写，使 "ACTIVE"、" Active "
// 与 "active" 等价。
func normalizeGoalStatus(status string) string {
	return strings.ToLower(strings.TrimSpace(status))
}
