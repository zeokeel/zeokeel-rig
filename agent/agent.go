// Package agent 实现 ZeoKeel-Rig 不依赖外部 agent 框架的 ReAct agent 循环。
//
// New 根据静态的 rig.AgentSpec 构造 rig.Agent；Run 在会话历史上执行模型↔工具循环
// 的一个轮次。循环直接与 rig.Provider 对话（Provider 契约只支持流式），因此不需要
// 任何 agent 框架依赖：它消费 rig.Chunk 流，从流式增量与工具调用片段装配助手
// 消息，分发工具（可选经过审批 Gate），并通过 rig.Event 回调报告一切。
//
// # 主要类型
//
//   - agent：rig.Agent 的实现；New 之后所有字段不可变，因此并发 Run 可以安全共享。
//   - DefaultMaxIterations：spec 与 RunRequest 都未配置轮数上限时的默认值
//     （0，表示不限制轮数）。
//   - run：单次 Run 的可变状态，除工具执行外只属于运行 goroutine。
//
// # 行为契约
//
// 循环在没有 agent 框架的情况下实现下述行为契约；这些契约由测试与
// docs/ARCHITECTURE.md 固定：
//
//   - 流式：每个分片的 Delta 变成 EventTextDelta，Reasoning 变成
//     EventReasoningDelta，Usage 变成 EventUsage（并累加到 RunResult.Usage），
//     非空 FinishReason 被记录。流总会被读尽到 io.EOF（rig.Stream 没有 Close；
//     最后一次 Recv 负责释放资源）。
//   - 工具调用：流式的 ToolCallDelta 片段按 Index 装配；ID 与 Name 在首次出现时
//     设置，Arguments 按到达顺序拼接。装配完成的调用会挂到助手消息上、作为
//     EventToolCall 发出，并在每个模型轮次按一批执行。
//   - 工具分发：工具按名字在 AgentSpec.Tools 中解析。未知名字不是致命错误：循环
//     会把工具错误回传给模型（"rig: tool not found"），让模型自行纠正。工具执行
//     错误与 panic 同样作为工具错误结果回传。
//   - 审批：nil Gate（以及 nil RunRequest.Gate）表示全部放行。门禁在运行
//     goroutine 上、执行之前按调用顺序被调用（rig.Gate 契约）。DecisionAllow
//     执行调用；DecisionDeny 把工具错误回传给模型而不中止运行；
//     DecisionAllowAlways 还会记住（工具名, 参数），使本次运行内后续相同调用跳过
//     门禁。allow-always 的作用域是一次 Run：新的 Run 会重新询问（契约未定义会话
//     级审批存储）。
//   - 并行工具：一个轮次中的工具体并发执行；事件流保持确定性，因为
//     EventToolCall 与 EventToolResult 在运行 goroutine 上按调用顺序发出，工具
//     结果消息也按调用顺序追加，与完成顺序无关（rig.Event 规定事件从运行
//     goroutine 同步发出）。并发数受 maxParallelToolCalls（8）限制。
//   - 轮数上限：默认不限制模型↔工具轮数（spec.MaxIterations <= 0 表示 unbounded；
//     RunRequest.MaxIterations 大于 0 时按次覆盖，小于等于 0 时回退到 spec 的
//     配置）。显式配置 >0 的上限时，如果最后一个允许的轮次仍然请求了工具，这些
//     工具会被执行且结果被追加（工具调用与结果不能被预算拆到两次 Run 中），随后
//     Run 返回部分 RunResult 以及包装 rig.ErrMaxIterations 的错误。
//   - 取消：context 被取消会停止运行并返回 ctx.Err()。工具批次被取消时，循环仍会
//     为每个已追加到历史的工具调用记录 tool 结果（合成错误），保证历史可重放。
//   - 历史：配置了 History 且 SessionID 非空时，循环会加载已有消息、追加
//     RunRequest 输入、每条助手消息以及每批工具结果。nil History 或空 SessionID
//     （按 rig.RunRequest 的约定表示“全新历史”）关闭持久化，但 RunResult.Messages
//     仍包含本次运行产出的全部消息。SystemPrompt 会加到每次请求前，且永不持久化。
//   - 事件：EventTurnStart/EventTurnEnd 包裹每个模型轮次（Iteration 从 1 开始）。
//     每条非空助手消息都会发出 EventMessage。EventError 用于致命且非取消类错误
//     （provider 失败、门禁失败、历史失败、轮数预算）；context 被取消时返回
//     ctx.Err() 而不发 EventError。Emit 可以为 nil。
//   - 完成通知：工具轮次结束后，循环会从实现 rig.NoticeSource 的工具取走通知，并
//     作为一条 user 消息追加（线协议风格的完成通知）。
//   - 脱敏：用 NewWithRedactor 注入 secrets.Redactor 后，工具结果（在写入历史、
//     History.Append、spill 落盘与发出事件之前）、助手正文与推理、压缩结果、
//     完成通知、所有 Emit 事件与 Run 返回的错误文本都会被脱敏。工具调用入参与
//     图片分片刻意保持原样（模型可能正当地把凭据交给工具）。nil 脱敏器时这条
//     契约完全不生效，也不会增加开销。增量事件是逐 chunk 脱敏的，见 redact.go
//     的"已知边界"。
//
// # 模型优先级
//
// Agent 总是向 Provider.Stream 发送非空模型名。RunRequest.Model 优先于
// AgentSpec.Model；空（或仅空白）的 RunRequest.Model 回退到 AgentSpec.Model，而
// New 要求后者非空。因此 provider 自己的默认模型（例如
// openai.Config.DefaultModel）在 Agent 路径上永远不会被使用：它只在直接调用
// Provider 时生效。
//
// 只要配置的 Provider、Tools 与 History 是并发安全的，Agent 就支持并发 Run 调用；
// 循环自身不保存可变状态。
package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"

	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/secrets"
)

// DefaultMaxIterations 是 spec 与 RunRequest 都未配置轮数上限时使用的模型轮数
// 上限默认值：0，表示不限制轮数（unlimited）。需要兜底上限的宿主应显式配置
// AgentSpec.MaxIterations 或 RunRequest.MaxIterations（>0）。
const DefaultMaxIterations = 0

// Agent 是不依赖外部 agent 框架的 rig.Agent 实现。New 之后除**按会话的 active 工具集合**外
// 所有字段不可变，因此并发 Run 调用可以安全共享它们（active 集合由 mutex 保护）。
//
// 它同时实现 rig.ToolActivator：可以按会话纯增量地激活已注册的工具。
type Agent struct {
	provider             rig.Provider
	model                string
	systemPrompt         string
	defaultMaxIterations int
	tools                map[string]rig.Tool
	toolSpecs            []rig.ToolSpec
	// toolOrder 是工具的注册顺序：同一批激活按它归一化（与参数顺序无关），
	// 全部 active 时也按它返回。请求里的 tools 实际用会话的 active 顺序
	// （activeSet.order，激活时 append），因此激活只在末尾追加，已激活部分
	// 逐字节稳定（provider 的 prefix cache 依赖这一点）。
	toolOrder []string
	// initialActive 是"会话初始 active 集合"；nil 表示全部 active（v0.20 行为）。
	initialActive *activeSet
	// toolIndex 是 name → 注册下标（active 列表映射回 specs 用）。
	toolIndex map[string]int
	// activeMu 保护 activeBySession。键为空串表示默认会话。
	activeMu        sync.RWMutex
	activeBySession map[string]*activeSet
	history         rig.History
	gate            rig.Gate
	temperature     *float64
	topP            *float64
	maxTokens       int
	contextBudget   int
	compactor       rig.Compactor
	spill           *rig.SpillPolicy
	checkpointStore rig.CheckpointStore
	logger          rig.Logger
	// redactor 非 nil 时，工具结果、助手文本/推理、事件与错误文本都会在离开 agent
	// 之前脱敏（见 redact.go）。nil 表示不脱敏，且不增加任何开销。
	redactor *secrets.Redactor
}

// New 校验 spec 并构造一个 Agent。
//
// agent 的默认模型是 spec.Model（必填）；RunRequest.Model 可以按次运行覆盖它。
// 参见包文档的“模型优先级”一节。校验错误在存在对应契约哨兵时会包装它：nil
// Provider 返回 ErrProviderRequired，空默认 Model 返回 ErrModelRequired。每个工具
// 条目都必须非 nil、名字非空且唯一；重复或未命名的工具属于配置错误，会在这里而
// 不是在分发时失败。
//
// New 不做输出脱敏；需要把凭据挡在模型上下文、历史、落盘文件与 UI 之外时，用
// NewWithRedactor 注入一个 secrets.Redactor（见 redact.go）。
func New(spec rig.AgentSpec) (rig.Agent, error) {
	return newAgent(spec, nil)
}

// newAgent 是 New 与 NewWithRedactor 的共同实现。
func newAgent(spec rig.AgentSpec, redactor *secrets.Redactor) (rig.Agent, error) {
	if spec.Provider == nil {
		return nil, fmt.Errorf("%w", rig.ErrProviderRequired)
	}
	model := strings.TrimSpace(spec.Model)
	if model == "" {
		return nil, fmt.Errorf("%w", rig.ErrModelRequired)
	}
	// <=0 表示不限制轮数（unbounded）；负值归一化为 0。显式 >0 的上限原样保留。
	maxIterations := max(spec.MaxIterations, 0)
	a := &Agent{
		provider:             spec.Provider,
		model:                model,
		systemPrompt:         strings.TrimSpace(spec.SystemPrompt),
		defaultMaxIterations: maxIterations,
		tools:                make(map[string]rig.Tool, len(spec.Tools)),
		toolSpecs:            make([]rig.ToolSpec, 0, len(spec.Tools)),
		toolOrder:            make([]string, 0, len(spec.Tools)),
		toolIndex:            make(map[string]int, len(spec.Tools)),
		activeBySession:      make(map[string]*activeSet),
		history:              spec.History,
		gate:                 spec.Gate,
		temperature:          spec.Temperature,
		topP:                 spec.TopP,
		maxTokens:            spec.MaxTokens,
		contextBudget:        spec.ContextBudget,
		compactor:            spec.Compactor,
		spill:                spec.Spill,
		checkpointStore:      spec.CheckpointStore,
		logger:               spec.Logger,
		redactor:             redactor,
	}
	for i, tool := range spec.Tools {
		if tool == nil {
			return nil, fmt.Errorf("rig: tool %d is nil", i)
		}
		name := strings.TrimSpace(tool.Name())
		if name == "" {
			return nil, fmt.Errorf("rig: tool %d has an empty name", i)
		}
		if _, exists := a.tools[name]; exists {
			return nil, fmt.Errorf("rig: duplicate tool name %q", name)
		}
		a.tools[name] = tool
		a.toolIndex[name] = len(a.toolOrder)
		a.toolOrder = append(a.toolOrder, name)
		a.toolSpecs = append(a.toolSpecs, rig.ToolSpec{
			Name:        name,
			Description: tool.Description(),
			Parameters:  tool.Schema(),
		})
	}
	// 初始 active 集合：nil 表示全部（v0.20 行为）；非 nil 时只认已注册的名字，
	// 未知名字静默忽略（配置里的笔误不该让构造失败，宿主可用 ActiveTools 核对）。
	if spec.InitialActiveTools != nil {
		wanted := make(map[string]struct{}, len(spec.InitialActiveTools))
		for _, name := range spec.InitialActiveTools {
			trimmed := strings.TrimSpace(name)
			if trimmed == "" {
				continue
			}
			if _, registered := a.tools[trimmed]; !registered {
				continue
			}
			wanted[trimmed] = struct{}{}
		}
		a.initialActive = &activeSet{names: wanted, order: make([]string, 0, len(wanted))}
		// 初始顺序固定为注册顺序（与 InitialActiveTools 的书写顺序无关）。
		for _, name := range a.toolOrder {
			if _, ok := wanted[name]; ok {
				a.initialActive.order = append(a.initialActive.order, name)
			}
		}
	}
	return a, nil
}

// activeSet 是一个会话的 active 工具：**有序列表** + 集合。
//
// 为什么要有序列表：请求里的 tools 数组必须**只会追加**。如果按注册顺序重新过滤，
// 激活一个"注册得比已 active 工具更早"的工具就会把它插到中间，prefix cache 从
// 那一项开始全部失效。有序列表把"什么时候激活"直接变成"加在末尾"，因此无论注册
// 顺序如何，已发出的前缀都逐字节不变。
type activeSet struct {
	order []string
	names map[string]struct{}
}

// ActivateTools 实现 rig.ToolActivator：按会话**纯增量**激活已注册的工具。
//
// 语义（缓存契约）：
//   - 只加不减。已 active 的名字仍出现在 activated 里（幂等），但内部的顺序与
//     集合不变，因此请求里的 tools 数组不会重排；
//   - 同一批里新增的名字按**注册顺序**追加（与参数顺序无关），保证同样的激活集合
//     得到同样的数组；
//   - 未知名字进 unknown，状态不变；空调用是 no-op。
func (a *Agent) ActivateTools(sessionID string, names ...string) (activated, unknown []string) {
	if a == nil || len(names) == 0 {
		return nil, nil
	}
	wanted := make(map[string]struct{}, len(names))
	for _, name := range names {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			continue
		}
		if _, registered := a.tools[trimmed]; !registered {
			unknown = append(unknown, trimmed)
			continue
		}
		wanted[trimmed] = struct{}{}
	}
	if len(wanted) == 0 {
		return nil, unknown
	}
	a.activeMu.Lock()
	defer a.activeMu.Unlock()
	set := a.activeSetLocked(sessionID)
	// 按注册顺序遍历：同一批激活与参数顺序无关。
	for _, name := range a.toolOrder {
		if _, ok := wanted[name]; !ok {
			continue
		}
		activated = append(activated, name)
		if _, already := set.names[name]; already {
			continue // 幂等：已 active 只报告，不改顺序
		}
		set.names[name] = struct{}{}
		set.order = append(set.order, name)
	}
	return activated, unknown
}

// ActiveTools 实现 rig.ToolActivator：返回会话当前可调用的工具名（按 active 顺序）。
func (a *Agent) ActiveTools(sessionID string) []string {
	if a == nil {
		return nil
	}
	if a.initialActive == nil {
		// 全部 active：注册顺序。
		return append([]string(nil), a.toolOrder...)
	}
	a.activeMu.RLock()
	defer a.activeMu.RUnlock()
	set := a.sessionActiveLocked(sessionID)
	return append([]string(nil), set.order...)
}

// sessionActiveLocked 返回会话的 active 集合：已创建的用会话副本，否则回退到初始
// 集合（只读语义，不创建状态）。调用方必须持锁。
func (a *Agent) sessionActiveLocked(sessionID string) *activeSet {
	if set, ok := a.activeBySession[sessionID]; ok {
		return set
	}
	return a.initialActive
}

// activeSetLocked 返回会话的 active 集合（必要时惰性创建），调用方必须持锁。
func (a *Agent) activeSetLocked(sessionID string) *activeSet {
	if set, ok := a.activeBySession[sessionID]; ok {
		return set
	}
	// 首次写入：从初始集合**拷贝**一份（initialActive 在 New 之后只读，绝不能被
	// 会话改动污染）。initialActive 为 nil 表示"全部 active"（v0.20 行为），此时
	// 副本就是全部注册工具。
	set := &activeSet{}
	if a.initialActive == nil {
		set.order = append([]string(nil), a.toolOrder...)
		set.names = make(map[string]struct{}, len(a.toolOrder))
		for _, name := range a.toolOrder {
			set.names[name] = struct{}{}
		}
	} else {
		set.order = append([]string(nil), a.initialActive.order...)
		set.names = make(map[string]struct{}, len(a.initialActive.names))
		for name := range a.initialActive.names {
			set.names[name] = struct{}{}
		}
	}
	a.activeBySession[sessionID] = set
	return set
}

// activeSpecs 返回本次请求要发送的 tools：全部 active 时直接复用注册表（同一份
// 切片，零分配、逐字节稳定）；否则按 active 顺序映射。
func (a *Agent) activeSpecs(sessionID string) []rig.ToolSpec {
	if a.initialActive == nil {
		return a.toolSpecs
	}
	a.activeMu.RLock()
	defer a.activeMu.RUnlock()
	set := a.sessionActiveLocked(sessionID)
	// 捷径：只有"全部 active **且顺序恰好等于注册顺序**"时才能直接复用注册表切片
	// （零分配、逐字节稳定）。只判断数量是不够的——凑齐全集时活跃顺序可能是
	// [gamma, alpha, beta] 这样的激活顺序，直接返回注册顺序会让整个数组重排，
	// prefix cache 从那一位开始全部失效。
	if len(set.order) == len(a.toolSpecs) && slices.Equal(set.order, a.toolOrder) {
		return a.toolSpecs
	}
	out := make([]rig.ToolSpec, 0, len(set.order))
	for _, name := range set.order {
		out = append(out, a.toolSpecs[a.toolIndex[name]])
	}
	return out
}

// toolIsActive 报告一次调用是否可以直接执行。
func (a *Agent) toolIsActive(sessionID, name string) bool {
	if a.initialActive == nil {
		return true
	}
	a.activeMu.RLock()
	defer a.activeMu.RUnlock()
	_, ok := a.sessionActiveLocked(sessionID).names[name]
	return ok
}

// Run 实现 rig.Agent。发送给 provider 的模型在 req.Model 非空（去空白）时为
// req.Model，否则为 agent 配置的模型；参见包文档的“模型优先级”一节。Run 永远不
// 会发送空模型名。
func (a *Agent) Run(ctx context.Context, req rig.RunRequest) (rig.RunResult, error) {
	result, err := a.runTurn(ctx, req)
	// 错误文本也会进 UI/日志（例如 provider 把请求体回显在 400 里），因此与事件
	// 一样统一脱敏；没有变化时返回原错误，保持 errors.Is/As 与错误身份。
	return result, redactError(a.redactor, err)
}

// runTurn 是 Run 的实现体；错误文本在 Run 里统一脱敏。
func (a *Agent) runTurn(ctx context.Context, req rig.RunRequest) (rig.RunResult, error) {
	if ctx == nil {
		return rig.RunResult{}, errors.New("rig: nil context")
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = a.model
	}
	if model == "" { // 防御：New 会拒绝这种 spec，但 RunRequest 也可能被直接使用
		return rig.RunResult{}, fmt.Errorf("%w", rig.ErrModelRequired)
	}
	// <=0 回退到 agent 默认（spec 未配置时为 0，即不限制轮数）。
	maxIterations := req.MaxIterations
	if maxIterations <= 0 {
		maxIterations = a.defaultMaxIterations
	}
	gate := req.Gate
	if gate == nil {
		gate = a.gate
	}
	r := &run{
		a:           a,
		ctx:         ctx,
		model:       model,
		gate:        gate,
		emit:        req.Emit,
		sessionID:   req.SessionID,
		persist:     a.history != nil && req.SessionID != "",
		allowAlways: make(map[string]struct{}),
	}

	if err := r.loadHistory(); err != nil {
		r.emitError(err)
		return r.result, err
	}
	r.turnID = newTurnID()
	if err := r.recoverCheckpoint(); err != nil {
		r.emitError(err)
		return r.result, err
	}
	if len(req.Input) > 0 {
		input := make([]rig.Message, len(req.Input))
		for i, msg := range req.Input {
			input[i] = cloneMessage(msg)
		}
		if err := r.appendMessages(input); err != nil {
			r.emitError(err)
			return r.result, err
		}
	}

	for iteration := 1; ; iteration++ {
		if err := ctx.Err(); err != nil {
			return r.result, err
		}
		if maxIterations > 0 && iteration > maxIterations {
			err := fmt.Errorf("%w: %d round(s) without a final answer", rig.ErrMaxIterations, maxIterations)
			r.emitError(err)
			return r.result, err
		}
		r.result.Iterations = iteration
		r.emitEvent(rig.Event{Type: rig.EventTurnStart, Iteration: iteration})
		if err := r.prepareContext(); err != nil {
			r.emitError(err)
			return r.result, err
		}
		if err := r.saveCheckpoint(rig.CheckpointPhaseRequested); err != nil {
			r.emitError(err)
			return r.result, err
		}

		assistant, finishReason, toolCalls, err := r.modelRound(iteration)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return r.result, ctxErr
			}
			r.emitError(err)
			return r.result, err
		}
		r.result.FinishReason = finishReason
		// 一次性归一化工具调用名，使分发、审批门禁、allow-always 缓存、流式事件
		// 与持久化历史对同一个标识达成一致（前导空格不能绕过名字匹配）。
		for i := range assistant.ToolCalls {
			assistant.ToolCalls[i].Name = strings.TrimSpace(assistant.ToolCalls[i].Name)
		}
		for i := range toolCalls {
			toolCalls[i].Name = strings.TrimSpace(toolCalls[i].Name)
		}
		// 空补全（没有正文、推理与工具调用）不是一条对话消息：丢弃它能保持历史
		// 可重放，并避免把空的 assistant 轮次注入下一次请求。
		if !isEmptyMessage(assistant) {
			if err := r.appendMessages([]rig.Message{assistant}); err != nil {
				r.emitError(err)
				return r.result, err
			}
			r.emitEvent(rig.Event{Type: rig.EventMessage, Iteration: iteration, Message: messagePtr(assistant)})
		}

		if len(toolCalls) == 0 {
			r.emitEvent(rig.Event{Type: rig.EventTurnEnd, Iteration: iteration})
			if err := r.saveCheckpoint(rig.CheckpointPhaseCompleted); err != nil {
				r.emitError(err)
				return r.result, err
			}
			return r.result, nil
		}

		if err := r.toolRound(toolCalls, iteration); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return r.result, ctxErr
			}
			r.emitError(err)
			return r.result, err
		}
		r.emitEvent(rig.Event{Type: rig.EventTurnEnd, Iteration: iteration})

		if maxIterations > 0 && iteration >= maxIterations {
			err := fmt.Errorf("%w: %d round(s) without a final answer", rig.ErrMaxIterations, maxIterations)
			r.emitError(err)
			return r.result, err
		}
	}
}

// run 承载一次 Run 调用的可变状态。除特别说明外，它只属于运行 goroutine
// （工具体只写入各自的结果槽位）。
type run struct {
	a            *Agent
	ctx          context.Context
	model        string
	gate         rig.Gate
	emit         func(rig.Event)
	sessionID    string
	turnID       string
	persist      bool
	conversation []rig.Message
	// turnMessages 是本回合**追加**到 conversation 的消息（按追加顺序）。它存在的
	// 唯一理由是检查点的 PriorMessages：压缩会用压缩器的输出整体替换 conversation，
	// 所以"本回合从哪一条开始"不能保存成一个静态下标（压缩前后长度会变），必须用
	// 本回合自己的消息列表事后再定位一次。见 priorMessages。
	turnMessages []rig.Message
	result       rig.RunResult
	// allowAlways 记住本次运行中经 DecisionAllowAlways 批准的（工具名, 参数），
	// 作用域持续到本次运行结束。
	allowAlways map[string]struct{}
	// lastCallSignature / sameCallStreak 是"模型抽风"守卫（见 toolRound）：
	// 连续多少轮发出**完全相同**的一批工具调用。
	lastCallSignature string
	sameCallStreak    int
	// repeatedNotice 非空时，在下一批工具结果之后注入一条提示消息。
	repeatedNotice string
}

func (r *run) loadHistory() error {
	if r.a.history == nil || !r.persist {
		return nil
	}
	msgs, err := r.a.history.Load(r.ctx, r.sessionID)
	if err != nil {
		return fmt.Errorf("rig: load history: %w", err)
	}
	// 历史可能来自崩溃中途、跨端同步或人工编辑：孤立 tool 消息会让 provider 对
	// 这个会话的**每一次**请求都返回 400，所以进请求之前先修好序列。
	repaired := repairConversation(msgs)
	if summary := repairSummary(len(msgs), len(repaired)); summary != "" && r.a.logger != nil {
		r.a.logger.Printf("rig: %s", summary)
	}
	r.conversation = append(r.conversation, repaired...)
	return nil
}

// appendMessages 把消息追加到对话与 RunResult.Messages，并在配置了 History 与会话
// 时持久化它们。它们属于**本回合**，因此同时记入 turnMessages（检查点分界用）。
func (r *run) appendMessages(msgs []rig.Message) error {
	if err := r.appendConversation(msgs); err != nil {
		return err
	}
	r.turnMessages = append(r.turnMessages, msgs...)
	return nil
}

// appendRecoveredMessages 把检查点恢复出来的、属于**上一个被中断回合**的消息写回
// 对话与 History。它们不计入 turnMessages：对本次 Run 而言它们是 seeded 历史，
// 必须落在检查点的 prior 一侧。
func (r *run) appendRecoveredMessages(msgs []rig.Message) error {
	return r.appendConversation(msgs)
}

// appendConversation 是两条追加路径的共同实现：只负责 conversation、RunResult 与
// History，不参与本回合分界的计数。
func (r *run) appendConversation(msgs []rig.Message) error {
	r.conversation = append(r.conversation, msgs...)
	r.result.Messages = append(r.result.Messages, msgs...)
	if !r.persist {
		return nil
	}
	// 即使运行已被取消也照样持久化：历史写入不能被半途中止，否则被取消的工具
	// 轮次可能在磁盘上留下一条没有工具结果的 assistant tool_calls 消息。
	if err := r.a.history.Append(context.WithoutCancel(r.ctx), r.sessionID, msgs...); err != nil {
		return fmt.Errorf("rig: append history: %w", err)
	}
	return nil
}

// modelRound 执行一次模型调用，返回装配好的助手消息、模型结束原因以及请求的工具
// 调用。流式增量会在到达时立即发出；助手消息是返回而不是直接发出，以便调用方
// 先把它追加到历史并发出 EventMessage。
func (r *run) modelRound(iteration int) (rig.Message, string, []rig.ToolCall, error) {
	req := rig.Request{
		Model:       r.model,
		Messages:    r.requestMessages(),
		Tools:       r.a.activeSpecs(r.sessionID),
		Temperature: r.a.temperature,
		TopP:        r.a.topP,
		MaxTokens:   r.a.maxTokens,
	}
	stream, err := r.a.provider.Stream(r.ctx, req)
	if err != nil {
		if ctxErr := r.ctx.Err(); ctxErr != nil {
			return rig.Message{}, "", nil, ctxErr
		}
		return rig.Message{}, "", nil, fmt.Errorf("rig: provider %q stream: %w", r.a.provider.Name(), err)
	}
	if stream == nil {
		return rig.Message{}, "", nil, fmt.Errorf("rig: provider %q returned a nil stream", r.a.provider.Name())
	}

	var text strings.Builder
	var reasoning strings.Builder
	accumulators := make(map[int]*toolCallAccumulator)
	finishReason := ""
	for {
		chunk, recvErr := stream.Recv()
		if recvErr != nil {
			if errors.Is(recvErr, io.EOF) {
				break
			}
			if ctxErr := r.ctx.Err(); ctxErr != nil {
				return rig.Message{}, "", nil, ctxErr
			}
			return rig.Message{}, "", nil, fmt.Errorf("rig: provider %q stream: %w", r.a.provider.Name(), recvErr)
		}
		if chunk.Delta != "" {
			text.WriteString(chunk.Delta)
			// emitEvent 统一脱敏（逐 chunk；跨 chunk 的凭据靠累积消息兜底，见 redact.go）。
			r.emitEvent(rig.Event{Type: rig.EventTextDelta, Text: chunk.Delta, Iteration: iteration})
		}
		if chunk.Reasoning != "" {
			reasoning.WriteString(chunk.Reasoning)
			r.emitEvent(rig.Event{Type: rig.EventReasoningDelta, Reasoning: chunk.Reasoning, Iteration: iteration})
		}
		for _, delta := range chunk.ToolCalls {
			acc := accumulators[delta.Index]
			if acc == nil {
				acc = &toolCallAccumulator{}
				accumulators[delta.Index] = acc
			}
			acc.merge(delta)
		}
		if chunk.Usage != nil {
			usage := *chunk.Usage
			r.result.Usage.Add(usage)
			r.emitEvent(rig.Event{Type: rig.EventUsage, Usage: &usage, Iteration: iteration})
		}
		if chunk.FinishReason != "" {
			finishReason = chunk.FinishReason
		}
	}

	assistant := rig.Message{Role: rig.RoleAssistant, Reasoning: reasoning.String()}
	if text.Len() > 0 {
		assistant.Content = []rig.ContentPart{rig.TextPart(text.String())}
	}
	indexes := make([]int, 0, len(accumulators))
	for index := range accumulators {
		indexes = append(indexes, index)
	}
	slices.Sort(indexes)
	toolCalls := make([]rig.ToolCall, 0, len(indexes))
	for _, index := range indexes {
		toolCalls = append(toolCalls, accumulators[index].call())
	}
	if len(toolCalls) > 0 {
		assistant.ToolCalls = toolCalls
	}
	// 在返回之前脱敏正文与推理：调用方随后会把它追加到历史、持久化并作为
	// EventMessage 发出，三条路径都不应看到明文。工具调用入参保持原样。
	r.redactMessage(&assistant)
	return assistant, finishReason, toolCalls, nil
}

// requestMessages 构造一次模型调用的消息列表：配置的系统提示词（永不持久化）
// 在前，当前对话在后。
func (r *run) requestMessages() []rig.Message {
	msgs := make([]rig.Message, 0, len(r.conversation)+1)
	if r.a.systemPrompt != "" {
		msgs = append(msgs, rig.NewMessage(rig.RoleSystem, r.a.systemPrompt))
	}
	return append(msgs, r.conversation...)
}

// newTurnID 生成每个 Run 唯一的回合 ID（时间戳 + 进程内序号）。
var turnCounter atomic.Uint64

func newTurnID() string {
	return fmt.Sprintf("turn-%d-%d", time.Now().UTC().UnixNano(), turnCounter.Add(1))
}

// recoverCheckpoint 在 Run 开始、加载 History 之后恢复被中断的回合。检查点只
// 作为恢复提示：History 是已完成回合的事实来源。没有检查点或恢复函数无修复时
// 是 no-op。恢复结果会写回 History（如果配置），并保存 completed 检查点使恢复幂等。
func (r *run) recoverCheckpoint() error {
	if r.a.checkpointStore == nil || r.sessionID == "" {
		return nil
	}
	repair, err := RecoverInterruptedTurn(r.ctx, r.a.checkpointStore, r.sessionID)
	if err != nil {
		return fmt.Errorf("rig: recover interrupted turn: %w", err)
	}
	if repair == nil {
		return nil
	}
	if len(r.conversation) == 0 && len(repair.Messages) > 0 {
		// 冷启动：History 里没有这个会话（会话存储丢失/迁移，或宿主只配了
		// checkpoint）。修复出来的会话必须写回 History —— 否则它只活在本次 Run 的
		// RunResult 里，而随后的 completed 检查点又让下一次恢复变成 no-op，被中断
		// 回合的上下文就永久丢了。这些消息属于**上一个**回合，所以用
		// appendRecoveredMessages（不进 turnMessages，检查点的 prior 侧要包含它们）。
		if err := r.appendRecoveredMessages(repair.Messages); err != nil {
			return err
		}
	} else if len(repair.Appended) > 0 {
		// History 里已经有这个会话：只补缺失的尾部（合成工具结果 + 中断标记）。
		// 同样属于上一个回合。
		if err := r.appendRecoveredMessages(repair.Appended); err != nil {
			return err
		}
	}
	if repair.RepairedCheckpoint != nil {
		if err := r.a.checkpointStore.Save(r.ctx, r.sessionID, repair.RepairedCheckpoint); err != nil {
			if r.a.logger != nil {
				r.a.logger.Printf("rig: save repaired checkpoint: %v", err)
			}
		}
	}
	return nil
}

// saveCheckpoint 在非终结阶段写检查点；配置失败时 fail-closed，避免在无恢复
// 记录时继续执行模型或有副作用的工具。
func (r *run) saveCheckpoint(phase rig.CheckpointPhase) error {
	if r.a.checkpointStore == nil || r.sessionID == "" || r.turnID == "" {
		return nil
	}
	checkpoint := &rig.Checkpoint{
		SessionID:     r.sessionID,
		TurnID:        r.turnID,
		Phase:         phase,
		Messages:      cloneMessages(r.conversation),
		PriorMessages: r.priorMessages(),
		UpdatedAt:     time.Now().UTC(),
	}
	if err := r.a.checkpointStore.Save(r.ctx, r.sessionID, checkpoint); err != nil {
		return fmt.Errorf("rig: save checkpoint (%s): %w", phase, err)
	}
	return nil
}

// priorMessages 返回当前对话中"属于本回合之前"的消息条数（检查点的
// PriorMessages）。
//
// 为什么不能存一个静态值：prepareContext 会把 conversation 整体替换成压缩器的
// 输出（对话变短、头部被摘要替代），压缩前算好的下标/条数在压缩后既不指向同一条
// 消息，还可能超过对话长度 —— 于是检查点写成 PriorMessages > len(Messages)，
// 任何按 rig.Checkpoint.Validate 契约校验的 CheckpointStore 都会拒收，run 以
// "save checkpoint" 失败告终（kit 默认开压缩，宿主一接 CheckpointStore 就必现）。
//
// 正确性来源是"本回合的消息列表"而不是减法猜测：本回合追加过的消息都在
// turnMessages 里，压缩只在头部做裁剪/摘要，所以本回合的消息仍然占据 conversation
// 的尾部。从尾部逐条比对 turnMessages，匹配到的条数就是本回合在压缩结果里仍然
// 存在的尾部长度，剩下的都属于 prior（压缩摘要、保留下来的旧消息）。
//
//	未压缩：conversation = seeded + 本回合消息，全部匹配 => prior = len(seeded)
//	压缩后：conversation = [system, 摘要, 保留的尾部...]，尾部匹配 => prior = 摘要+旧消息
//
// 结尾的钳制是**防御性收敛**，不是正确性来源：rig.Compactor 是宿主可实现的接口，
// 它可以任意重排、增删消息，我们无法从它的输出反推分界。钳制只保证写出的检查点
// 至少是合法的（rig.Checkpoint.Validate 要求 0 <= PriorMessages <= len(Messages)），
// 不让压缩器的怪异输出把整个 Run 变成硬失败。
func (r *run) priorMessages() int {
	matched := 0
	for matched < len(r.turnMessages) && matched < len(r.conversation) {
		if !sameMessageForBoundary(
			r.conversation[len(r.conversation)-1-matched],
			r.turnMessages[len(r.turnMessages)-1-matched],
		) {
			break
		}
		matched++
	}
	prior := len(r.conversation) - matched
	if prior < 0 {
		prior = 0
	}
	if prior > len(r.conversation) {
		prior = len(r.conversation)
	}
	return prior
}

// sameMessageForBoundary 判断两条消息是否是"同一条"（用于压缩后重新定位本回合
// 消息的起点）。压缩器可能克隆、重排、截断正文或脱敏改写内容，所以这里比较的是
// 稳定的可观察字段；被改写的那条会被判为"不是同一条"，效果是分界偏保守（prior
// 偏大、仍有界），不会越界。
func sameMessageForBoundary(left, right rig.Message) bool {
	if left.Role != right.Role || left.Name != right.Name || left.ToolCallID != right.ToolCallID {
		return false
	}
	if left.Reasoning != right.Reasoning || len(left.ToolCalls) != len(right.ToolCalls) {
		return false
	}
	return left.Text() == right.Text()
}

// prepareContext 在每次模型调用前检查对话是否超过 ContextBudget，超预算时
// 调用 Compactor 压缩内存中的对话。压缩结果只影响后续请求，不自动写回 History；
// 需要持久化的宿主应自行替换历史。
//
// 压缩失败**不中止运行**：预算只是估算（EstimateTokens），真正的裁判是 provider。
// 一次瞬时故障（摘要模型超时、自定义 compactor 的 bug）不该打断用户正在做的事，
// 因此这里发一条 EventError 作为提示，然后带着未压缩的对话继续——最坏情况是
// provider 报"上下文超限"，那比"压缩器内部错误"更能指导用户。只有 context 自己
// 被取消/超时才向上返回。
func (r *run) prepareContext() error {
	if r.a.compactor == nil || r.a.contextBudget <= 0 {
		return nil
	}
	systemTokens := rig.EstimateTextTokens(r.a.systemPrompt)
	if rig.EstimateTokens(r.conversation)+systemTokens <= r.a.contextBudget {
		return nil
	}
	before := len(r.conversation)
	compacted, err := r.a.compactor.Compact(r.ctx, r.sessionID, cloneMessages(r.conversation), r.a.contextBudget-systemTokens)
	if err != nil {
		if ctxErr := r.ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		r.emitEvent(rig.Event{Type: rig.EventError, Err: fmt.Errorf(
			"rig: compaction failed, continuing without it: %w", err)})
		return nil
	}
	if compacted == nil {
		return nil
	}
	// 压缩结果会进入后续请求：同样过一遍脱敏（写时复制，不就地改压缩器的切片）。
	RedactMessages(r.a.redactor, compacted)
	r.conversation = compacted
	// 让 UI 能说"历史被折叠了，没有丢"：个人客户端最怕用户以为上下文被删了。
	r.emitEvent(rig.Event{
		Type:       rig.EventContextCompacted,
		Iteration:  r.result.Iterations,
		Compaction: &rig.CompactionInfo{Before: before, After: len(compacted)},
	})
	return nil
}

// maxParallelToolCalls 限制一个模型轮次中同时执行的工具体数量。恶意或混乱的模型
// 可能在一次响应中发出成千上万个工具调用；不加限制的话每个调用都会变成一个
// goroutine，成为资源耗尽攻击面。超出上限的调用会排在 worker 之后，因此该轮次
// 仍会按调用顺序完成历史与事件的记录。
const maxParallelToolCalls = 8

// toolRound 解析、审批、执行并记录一批工具调用。审批在运行 goroutine 上按调用
// 顺序进行（rig.Gate 契约）；执行并发但受 maxParallelToolCalls 限制；事件与对话
// 记录按调用顺序发出与追加。
//
// 作为 assistant 消息一部分被追加到对话的每个工具调用，都会在本函数返回之前得到
// 一条 role=tool 消息回复——包括该轮次因 context 取消或审批门禁错误而中止时。
// 已持久化但工具调用未被回复的 assistant 消息会让会话无法重放（OpenAI 兼容端点
// 会以 HTTP 400 拒绝），因此中止路径会记录合成的工具错误。
func (r *run) toolRound(calls []rig.ToolCall, iteration int) error {
	tools := make([]rig.Tool, len(calls))
	resolved := make([]bool, len(calls))
	results := make([]rig.ToolResult, len(calls))
	done := make([]bool, len(calls))
	for i := range calls {
		calls[i].Name = strings.TrimSpace(calls[i].Name)
		tools[i], resolved[i] = r.a.tools[calls[i].Name]
	}
	for i := range calls {
		callCopy := calls[i]
		r.emitEvent(rig.Event{Type: rig.EventToolCall, Iteration: iteration, Call: &callCopy})
	}
	blocked := r.trackRepetition(calls)

	// 审批在运行 goroutine 上按调用顺序执行。被拒绝与未知的调用产生工具错误结果，
	// 而不是中止运行。
	decisions := make([]rig.Decision, len(calls))
	deniedReason := make([]string, len(calls))
	// abort 为每个尚未有决定的调用记录工具结果，然后返回原始原因，使调用方以
	// 可重放的历史中止运行。
	abort := func(from int, reason string, cause error) error {
		for j := from; j < len(calls); j++ {
			if decisions[j] == "" {
				decisions[j] = rig.DecisionDeny
				deniedReason[j] = reason
			}
		}
		if recordErr := r.recordToolRound(calls, decisions, results, done, deniedReason, iteration); recordErr != nil {
			return fmt.Errorf("%w (also failed to record tool results: %v)", cause, recordErr)
		}
		return cause
	}

	for i := range calls {
		if err := r.ctx.Err(); err != nil {
			return abort(i, "rig: tool call cancelled: the run stopped before this call was approved", err)
		}
		if !resolved[i] {
			decisions[i] = rig.DecisionDeny
			deniedReason[i] = fmt.Sprintf("rig: tool not found: %s", calls[i].Name)
			continue
		}
		if !r.a.toolIsActive(r.sessionID, calls[i].Name) {
			// 已注册但本会话未激活：给出可执行的下一步（用 tool_search 激活），
			// 而不是让它猜"为什么不认识这个工具"。不执行、不过 gate。
			decisions[i] = rig.DecisionDeny
			deniedReason[i] = fmt.Sprintf(
				"rig: tool %q is registered but not active in this session; activate it with tool_search first",
				calls[i].Name)
			continue
		}
		if blocked[i] {
			// 抽风守卫：完全相同的调用已经执行过太多次，直接给它一个工具错误，
			// 而不是再烧一次钱（也不去打扰用户审批）。
			decisions[i] = rig.DecisionDeny
			deniedReason[i] = repeatedCallBlockedMessage
			continue
		}
		if _, ok := r.allowAlways[callKey(calls[i])]; ok {
			decisions[i] = rig.DecisionAllow
			continue
		}
		if r.gate == nil {
			decisions[i] = rig.DecisionAllow
			continue
		}
		decision, err := r.gate.Approve(r.ctx, tools[i], calls[i])
		if err != nil {
			if ctxErr := r.ctx.Err(); ctxErr != nil {
				return abort(i, "rig: tool call cancelled: the approval gate was interrupted", ctxErr)
			}
			return abort(i, fmt.Sprintf("rig: approval gate for tool %q failed: %v", calls[i].Name, err),
				fmt.Errorf("rig: approval gate for tool %q: %w", calls[i].Name, err))
		}
		switch decision {
		case rig.DecisionAllow:
			decisions[i] = rig.DecisionAllow
		case rig.DecisionAllowAlways:
			decisions[i] = rig.DecisionAllow
			r.allowAlways[callKey(calls[i])] = struct{}{}
		case rig.DecisionDeny, "":
			decisions[i] = rig.DecisionDeny
			// 门禁可以在拒绝时解释原因（可选接口 DenialExplainer）；没有解释时
			// 保持历史文案逐字节不变。
			deniedReason[i] = deniedToolCallMessage(r.gate, tools[i], calls[i])
		default:
			return abort(i, fmt.Sprintf("rig: approval gate returned unknown decision %q", decision),
				fmt.Errorf("rig: approval gate returned unknown decision %q for tool %q", decision, calls[i].Name))
		}
	}

	if err := r.saveCheckpoint(rig.CheckpointPhaseToolPending); err != nil {
		return abort(0, "rig: checkpoint save failed before tool execution", fmt.Errorf("rig: save tool-pending checkpoint: %w", err))
	}
	r.executeToolCalls(tools, calls, decisions, results, done)
	if err := r.recordToolRound(calls, decisions, results, done, deniedReason, iteration); err != nil {
		r.emitError(err)
		return err
	}
	if err := r.appendPendingNotices(tools); err != nil {
		r.emitError(err)
		return err
	}
	if notice := r.repeatedNotice; notice != "" {
		r.repeatedNotice = ""
		if err := r.appendMessages([]rig.Message{rig.NewMessage(rig.RoleUser, notice)}); err != nil {
			r.emitError(err)
			return err
		}
	}
	// 工具执行期间发生的取消，只有在工具消息被记录之后（历史不变量）才向调用方
	// 报告。
	if err := r.ctx.Err(); err != nil {
		return err
	}
	return nil
}

// executeToolCalls 用有界 worker 池执行被允许的调用。每个 worker 只写自己的
// 结果/done 槽位，因此不需要加锁；done[i] 在结果之后设置，使 recordToolRound 能
// 区分“已执行”与“从未开始”的调用。
func (r *run) executeToolCalls(tools []rig.Tool, calls []rig.ToolCall, decisions []rig.Decision, results []rig.ToolResult, done []bool) {
	workers := min(maxParallelToolCalls, len(calls))
	if workers == 0 {
		return
	}
	var next atomic.Int64
	next.Store(-1)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Go(func() {
			for {
				index := int(next.Add(1))
				if index >= len(calls) {
					return
				}
				if decisions[index] != rig.DecisionAllow {
					continue
				}
				results[index] = r.invokeTool(tools[index], calls[index])
				done[index] = true
			}
		})
	}
	wg.Wait()
}

// recordToolRound 按调用顺序为每个调用发出一个工具结果事件，并把工具消息追加到
// 对话。被拒绝、从未执行或已取消的调用会得到错误结果，因此 assistant tool_calls
// 消息总能得到回复。
func (r *run) recordToolRound(calls []rig.ToolCall, decisions []rig.Decision, results []rig.ToolResult, done []bool, deniedReason []string, iteration int) error {
	toolMessages := make([]rig.Message, 0, len(calls))
	var attachments []rig.Message
	for i, call := range calls {
		result := results[i]
		switch {
		case decisions[i] != rig.DecisionAllow:
			if result.Content == "" {
				result.Content = deniedReason[i]
			}
			if result.Content == "" {
				result.Content = "rig: tool call was not approved"
			}
			result.IsError = true
		case !done[i] && result.Content == "":
			result.Content = "rig: tool call was not completed (the run was cancelled)"
			result.IsError = true
		}
		// 先脱敏再落盘/落事件/进历史：spill 出来的文件同样不能有明文。
		r.redactResult(&result)
		if r.a.spill != nil {
			spilled, spillErr := spillToolResult(withToolSession(r.ctx, r.sessionID), *r.a.spill, call, result.Content)
			if spillErr != nil {
				if r.a.logger != nil {
					r.a.logger.Printf("rig: spill tool %q result: %v", call.Name, spillErr)
				}
			} else if spilled != "" {
				result.Content = spilled
			}
		}
		callCopy := call
		resultCopy := result
		r.emitEvent(rig.Event{Type: rig.EventToolResult, Iteration: iteration, Call: &callCopy, Result: &resultCopy})
		toolMessages = append(toolMessages, rig.Message{
			Role:       rig.RoleTool,
			Content:    []rig.ContentPart{rig.TextPart(result.Content)},
			ToolCallID: call.ID,
			Name:       call.Name,
		})
		// chat-completions 的 tool 消息只能是文本。非文本附件（图片）会作为一条
		// 后续 user 消息投递，使模型在下一轮能看到它们。
		if len(result.Parts) > 0 {
			parts := make([]rig.ContentPart, 0, len(result.Parts)+1)
			parts = append(parts, rig.TextPart(fmt.Sprintf("Tool %q returned an attachment:", call.Name)))
			parts = append(parts, result.Parts...)
			attachments = append(attachments, rig.Message{Role: rig.RoleUser, Content: parts})
		}
	}
	toolMessages = append(toolMessages, attachments...)
	return r.appendMessages(toolMessages)
}

// appendPendingNotices 从实现 rig.NoticeSource 的工具取风格的完成通知，
// 并作为一条 user 消息追加。该消息加在工具消息之后，因此下一轮模型能看到它。
func (r *run) appendPendingNotices(tools []rig.Tool) error {
	var notices []string
	for _, tool := range tools {
		source, ok := tool.(rig.NoticeSource)
		if !ok {
			continue
		}
		notices = append(notices, source.PendingNotices()...)
	}
	if len(notices) == 0 {
		return nil
	}
	var builder strings.Builder
	builder.WriteString("Background notifications:")
	for _, notice := range r.redactNotices(notices) {
		builder.WriteString("\n- ")
		builder.WriteString(notice)
	}
	return r.appendMessages([]rig.Message{rig.NewMessage(rig.RoleUser, builder.String())})
}

// invokeTool 执行一个工具，把错误与 panic 转换为工具错误结果。被取消的 context
// 由 toolRound 的调用方通过 ctx.Err() 报告（wg.Wait 之后的检查）：在这里它只是
// 一个工具错误。
func (r *run) invokeTool(tool rig.Tool, call rig.ToolCall) (result rig.ToolResult) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if r.a.logger != nil {
				r.a.logger.Printf("rig: tool %q panicked: %v", call.Name, recovered)
			}
			result = rig.ToolResult{
				Content: fmt.Sprintf("rig: tool %q panicked: %v", call.Name, recovered),
				IsError: true,
			}
		}
	}()
	res, err := tool.Invoke(withToolSession(r.ctx, r.sessionID), call)
	if err != nil {
		return rig.ToolResult{
			Content: fmt.Sprintf("rig: tool %q: %v", call.Name, err),
			IsError: true,
		}
	}
	return res
}

func (r *run) emitEvent(event rig.Event) {
	if r.emit == nil {
		return
	}
	// Emit 是 UI/日志通道：所有事件在这里统一脱敏（redactor 为 nil 时原样返回）。
	r.emit(redactEvent(r.a.redactor, event))
}

func (r *run) emitError(err error) {
	r.emitEvent(rig.Event{Type: rig.EventError, Err: err, Iteration: r.result.Iterations})
}

// toolCallAccumulator 从 rig.ToolCallDelta 片段装配一个流式工具调用。ID 与 Name
// 在到达时设置；Arguments 按到达顺序拼接。
type toolCallAccumulator struct {
	id        string
	name      string
	arguments strings.Builder
}

func (a *toolCallAccumulator) merge(delta rig.ToolCallDelta) {
	if delta.ID != "" {
		a.id = delta.ID
	}
	if delta.Name != "" {
		a.name = delta.Name
	}
	a.arguments.WriteString(delta.Arguments)
}

func (a *toolCallAccumulator) call() rig.ToolCall {
	return rig.ToolCall{ID: a.id, Name: a.name, Arguments: a.arguments.String()}
}

// callKey 标识 allow-always 缓存中的一个审批目标。
func callKey(call rig.ToolCall) string {
	return call.Name + "\x00" + call.Arguments
}

// cloneMessage 返回 msg 的副本，其 Content 与 ToolCalls 切片不与输入共享底层数组，
// 因此调用方（或 History 实现）无法通过副本改写已存储的状态。
func cloneMessage(msg rig.Message) rig.Message {
	out := msg
	if msg.Content != nil {
		out.Content = slices.Clone(msg.Content)
	}
	if msg.ToolCalls != nil {
		out.ToolCalls = slices.Clone(msg.ToolCalls)
	}
	return out
}

// isEmptyMessage 报告 msg 是否既无正文、也无推理与工具调用（空模型补全）。
func isEmptyMessage(msg rig.Message) bool {
	return len(msg.Content) == 0 && msg.Reasoning == "" && len(msg.ToolCalls) == 0
}

func messagePtr(msg rig.Message) *rig.Message {
	msgCopy := msg
	return &msgCopy
}

// "模型抽风"守卫的阈值。个人客户端默认不设轮数上限（agent 层仍是 0 = 不限），
// 因此这里提供两道与"重复"有关的闸：
//
//	G1 连续 repeatedCallNoticeStreak 轮发出完全相同的调用 -> 注入一条提示（不打断）
//	G2 同一签名连续 repeatedCallBlockStreak 轮，或一轮里有 repeatedCallBlockPerRound
//	   个完全相同的调用 -> 该调用返回工具错误 "rig: repeated call blocked"
//
// 提示先于熔断：先给模型一个自己跳出来的机会，再省下它的钱。
const (
	repeatedCallNoticeStreak  = 3
	repeatedCallBlockStreak   = 5
	repeatedCallBlockPerRound = 3
)

// repeatedCallBlockedMessage 是重复调用被拦下时模型看到的工具错误。
const repeatedCallBlockedMessage = "rig: repeated call blocked: this call is identical to one " +
	"already executed in this round (or the same call has repeated several rounds in a row). " +
	"Change the arguments or take a different approach."

// repeatedCallNoticeMessage 是注入给模型的提示文案（user 角色，跟在工具结果之后）。
const repeatedCallNoticeMessage = "rig: notice: you have called %s with identical arguments %d " +
	"round(s) in a row and the result has not changed. Try a different approach, or answer the " +
	"user with what you already have."

// callSignature 返回一批工具调用的稳定签名：把每个调用的"名字 + 去空白后的参数"
// 排序后拼接。参数顺序不同不影响签名（模型换一个 key 的顺序不算新调用）。
func callSignature(calls []rig.ToolCall) string {
	parts := make([]string, len(calls))
	for i, call := range calls {
		parts[i] = strings.TrimSpace(call.Name) + "\x00" + strings.TrimSpace(call.Arguments)
	}
	sort.Strings(parts)
	return strings.Join(parts, "\x01")
}

// trackRepetition 更新"连续相同调用"计数并按需设置提示/返回要拦下的调用。
//
// 返回的 blocked[i] 为 true 表示第 i 个调用应与"被拒绝"同样处理（产生工具错误
// 结果，而不是执行）。它不修改 calls，也不发事件：调用方已经把 EventToolCall
// 发过了，历史配对照旧成立。
func (r *run) trackRepetition(calls []rig.ToolCall) []bool {
	signature := callSignature(calls)
	if signature == r.lastCallSignature {
		r.sameCallStreak++
	} else {
		r.lastCallSignature = signature
		r.sameCallStreak = 1
	}
	blocked := make([]bool, len(calls))
	if len(calls) == 0 {
		return blocked
	}
	// 同轮内完全相同的调用：第 repeatedCallBlockPerRound 个起拦下。
	seen := make(map[string]int, len(calls))
	for i, call := range calls {
		key := strings.TrimSpace(call.Name) + "\x00" + strings.TrimSpace(call.Arguments)
		seen[key]++
		if seen[key] >= repeatedCallBlockPerRound {
			blocked[i] = true
		}
	}
	if r.sameCallStreak >= repeatedCallBlockStreak {
		for i := range blocked {
			blocked[i] = true
		}
	}
	// G1：连续相同轮次的第 3 轮起提示一次（每轮都提示会污染上下文）。
	if r.sameCallStreak == repeatedCallNoticeStreak {
		names := make([]string, 0, len(calls))
		for _, call := range calls {
			names = append(names, strings.TrimSpace(call.Name))
		}
		r.repeatedNotice = fmt.Sprintf(repeatedCallNoticeMessage, strings.Join(names, ", "), r.sameCallStreak)
	}
	return blocked
}

// toolSessionKey 是会话 ID 在工具 context 里的键。
type toolSessionKey struct{}

// withToolSession 把会话 ID 注入工具调用的 context。
func withToolSession(ctx context.Context, sessionID string) context.Context {
	if sessionID == "" || ctx == nil {
		return ctx
	}
	return context.WithValue(ctx, toolSessionKey{}, sessionID)
}

// ToolSessionID 返回当前工具调用所属的会话 ID；不在工具调用里时返回空串。
//
// 工具的 Invoke 只拿到 ctx，而"激活属于哪个会话"是运行期状态（同一个 agent 可以
// 被多个会话共享）。需要按会话做事的工具（例如 tools/discovery 的 tool_search）
// 用它取会话，而不是猜一个全局默认值。
func ToolSessionID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	sessionID, _ := ctx.Value(toolSessionKey{}).(string)
	return sessionID
}
