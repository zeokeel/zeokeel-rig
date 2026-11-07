// Package agent 实现 ZeoKeel-Rig 不依赖 eino 的 ReAct agent 循环。
//
// New 根据静态的 rig.AgentSpec 构造 rig.Agent；Run 在会话历史上执行模型↔工具循环
// 的一个轮次。循环直接与 rig.Provider 对话（Provider 契约只支持流式），因此不需要
// 任何 agent 框架依赖：它消费 rig.Chunk 流，从流式增量与工具调用片段装配助手
// 消息，分发工具（可选经过审批 Gate），并通过 rig.Event 回调报告一切。
//
// # 主要类型
//
//   - agent：rig.Agent 的实现；New 之后所有字段不可变，因此并发 Run 可以安全共享。
//   - DefaultMaxIterations：spec 与 RunRequest 都未配置轮数上限时使用的默认值（20）。
//   - run：单次 Run 的可变状态，除工具执行外只属于运行 goroutine。
//
// # 行为契约
//
// 循环在没有 agent 框架的情况下实现下述行为契约；它是 docs/AUDIT-REPORT.md 中
// 集成审计结论的可移植实现：
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
//   - 轮数上限：MaxIterations（spec 默认 20，为 0 时；RunRequest 大于 0 时覆盖）
//     限制模型轮数。如果最后一个允许的轮次仍然请求了工具，这些工具会被执行且结果
//     被追加（工具调用与结果不能被预算拆到两次 Run 中），随后 Run 返回部分
//     RunResult 以及包装 rig.ErrMaxIterations 的错误。
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
//     作为一条 user 消息追加（DSH 风格的完成通知）。
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

	"strings"
	"sync"
	"sync/atomic"

	"github.com/zeokeel/zeokeel-rig"
)

// DefaultMaxIterations 是 spec 与 RunRequest 都未配置轮数上限时使用的模型轮数
// 上限。
const DefaultMaxIterations = 20

// agent 是不依赖 eino 的 rig.Agent 实现。New 之后所有字段不可变，因此并发 Run
// 调用可以安全共享它们。
type agent struct {
	provider             rig.Provider
	model                string
	systemPrompt         string
	defaultMaxIterations int
	tools                map[string]rig.Tool
	toolSpecs            []rig.ToolSpec
	history              rig.History
	gate                 rig.Gate
	temperature          *float64
	topP                 *float64
	maxTokens            int
	logger               rig.Logger
}

// New 校验 spec 并构造一个 Agent。
//
// agent 的默认模型是 spec.Model（必填）；RunRequest.Model 可以按次运行覆盖它。
// 参见包文档的“模型优先级”一节。校验错误在存在对应契约哨兵时会包装它：nil
// Provider 返回 ErrProviderRequired，空默认 Model 返回 ErrModelRequired。每个工具
// 条目都必须非 nil、名字非空且唯一；重复或未命名的工具属于配置错误，会在这里而
// 不是在分发时失败。
func New(spec rig.AgentSpec) (rig.Agent, error) {
	if spec.Provider == nil {
		return nil, fmt.Errorf("%w", rig.ErrProviderRequired)
	}
	model := strings.TrimSpace(spec.Model)
	if model == "" {
		return nil, fmt.Errorf("%w", rig.ErrModelRequired)
	}
	maxIterations := spec.MaxIterations
	if maxIterations <= 0 {
		maxIterations = DefaultMaxIterations
	}
	a := &agent{
		provider:             spec.Provider,
		model:                model,
		systemPrompt:         strings.TrimSpace(spec.SystemPrompt),
		defaultMaxIterations: maxIterations,
		tools:                make(map[string]rig.Tool, len(spec.Tools)),
		toolSpecs:            make([]rig.ToolSpec, 0, len(spec.Tools)),
		history:              spec.History,
		gate:                 spec.Gate,
		temperature:          spec.Temperature,
		topP:                 spec.TopP,
		maxTokens:            spec.MaxTokens,
		logger:               spec.Logger,
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
		a.toolSpecs = append(a.toolSpecs, rig.ToolSpec{
			Name:        name,
			Description: tool.Description(),
			Parameters:  tool.Schema(),
		})
	}
	return a, nil
}

// Run 实现 rig.Agent。发送给 provider 的模型在 req.Model 非空（去空白）时为
// req.Model，否则为 agent 配置的模型；参见包文档的“模型优先级”一节。Run 永远不
// 会发送空模型名。
func (a *agent) Run(ctx context.Context, req rig.RunRequest) (rig.RunResult, error) {
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
		if iteration > maxIterations {
			err := fmt.Errorf("%w: %d round(s) without a final answer", rig.ErrMaxIterations, maxIterations)
			r.emitError(err)
			return r.result, err
		}
		r.result.Iterations = iteration
		r.emitEvent(rig.Event{Type: rig.EventTurnStart, Iteration: iteration})

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

		if iteration >= maxIterations {
			err := fmt.Errorf("%w: %d round(s) without a final answer", rig.ErrMaxIterations, maxIterations)
			r.emitError(err)
			return r.result, err
		}
	}
}

// run 承载一次 Run 调用的可变状态。除特别说明外，它只属于运行 goroutine
// （工具体只写入各自的结果槽位）。
type run struct {
	a            *agent
	ctx          context.Context
	model        string
	gate         rig.Gate
	emit         func(rig.Event)
	sessionID    string
	persist      bool
	conversation []rig.Message
	result       rig.RunResult
	// allowAlways 记住本次运行中经 DecisionAllowAlways 批准的（工具名, 参数），
	// 作用域持续到本次运行结束。
	allowAlways map[string]struct{}
}

func (r *run) loadHistory() error {
	if r.a.history == nil || !r.persist {
		return nil
	}
	msgs, err := r.a.history.Load(r.ctx, r.sessionID)
	if err != nil {
		return fmt.Errorf("rig: load history: %w", err)
	}
	for _, msg := range msgs {
		r.conversation = append(r.conversation, cloneMessage(msg))
	}
	return nil
}

// appendMessages 把消息追加到对话与 RunResult.Messages，并在配置了 History 与会话
// 时持久化它们。
func (r *run) appendMessages(msgs []rig.Message) error {
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
		Tools:       r.a.toolSpecs,
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
			deniedReason[i] = fmt.Sprintf("rig: tool call denied by approval gate: %s", calls[i].Name)
		default:
			return abort(i, fmt.Sprintf("rig: approval gate returned unknown decision %q", decision),
				fmt.Errorf("rig: approval gate returned unknown decision %q for tool %q", decision, calls[i].Name))
		}
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

// appendPendingNotices 从实现 rig.NoticeSource 的工具取走 DSH 风格的完成通知，
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
	for _, notice := range notices {
		builder.WriteString("\n- ")
		builder.WriteString(strings.TrimSpace(notice))
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
	res, err := tool.Invoke(r.ctx, call)
	if err != nil {
		return rig.ToolResult{
			Content: fmt.Sprintf("rig: tool %q: %v", call.Name, err),
			IsError: true,
		}
	}
	return res
}

func (r *run) emitEvent(event rig.Event) {
	if r.emit != nil {
		r.emit(event)
	}
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
