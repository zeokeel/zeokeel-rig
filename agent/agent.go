// Package agent implements ZeoKeel-Rig's eino-free ReAct agent loop.
//
// New builds a rig.Agent from a static rig.AgentSpec; Run executes one turn of
// the model<->tool loop over a session history. The loop talks to a rig.Provider
// directly (the Provider contract is stream-only), so it needs no agent framework dependency: it
// consumes rig.Chunk streams, assembles assistant messages from streamed deltas
// and tool-call deltas, dispatches tools (optionally through an approval Gate),
// and reports everything through rig.Event callbacks.
//
// # Behavior contract
//
// The loop reproduces the semantics documented in the eino audit
// (docs/research/rig-sdk/01-eino-audit.md §3) without eino:
//
//   - Streaming: each chunk's Delta becomes EventTextDelta, Reasoning becomes
//     EventReasoningDelta, Usage becomes EventUsage (and is summed into
//     RunResult.Usage), and a non-empty FinishReason is recorded. The stream is
//     always drained to io.EOF (rig.Stream has no Close; the final Recv releases
//     resources).
//   - Tool calls: streamed ToolCallDelta fragments are assembled by Index.
//     ID and Name are set when they arrive first; Arguments are concatenated in
//     arrival order. Complete calls are exposed on the assistant message,
//     emitted as EventToolCall, and executed in one batch per model round.
//   - Tool dispatch: tools are resolved by name against AgentSpec.Tools.
//     Unknown names are NOT fatal: the loop feeds a tool error back to the
//     model ("rig: tool not found") so the model can correct itself. Tool
//     execution errors and panics are likewise fed back as tool error results.
//   - Approval: a nil Gate (and nil RunRequest.Gate) allows everything. The
//     gate is called on the run goroutine, in call order, before execution
//     (rig.Gate contract). DecisionAllow runs the call, DecisionDeny feeds a
//     tool error back to the model without aborting the run, and
//     DecisionAllowAlways additionally remembers (tool name, arguments) for
//     the remainder of the run so identical calls skip the gate. The
//     allow-always scope is the run: a new Run asks again (the contract defines no
//     session-level approval storage).
//   - Parallel tools: tool bodies of one round run concurrently; the event
//     stream stays deterministic because EventToolCall and EventToolResult are
//     emitted on the run goroutine in call order, and tool result messages are
//     appended in call order regardless of completion order (rig.Event says
//     events are emitted synchronously from the run goroutine).
//   - Iterations: MaxIterations (spec default 20 when 0; RunRequest overrides
//     when >0) caps model rounds. If the last allowed round still requested
//     tools, those tools are executed and their results appended (a tool call
//     and its result must not be split across runs by the budget), then Run
//     returns the partial RunResult together with an error wrapping
//     rig.ErrMaxIterations.
//   - Cancellation: a cancelled context stops the run and returns ctx.Err().
//   - History: when a History is configured and SessionID is non-empty, the
//     loop loads prior messages, appends the RunRequest input, every assistant
//     message, and every tool-result batch. A nil History, or an empty
//     SessionID ("fresh history" per rig.RunRequest), disables persistence but
//     RunResult.Messages still contains everything the run produced. The
//     SystemPrompt is prepended to each request and is never persisted.
//   - Events: EventTurnStart/EventTurnEnd bracket each model round (Iteration is
//     1-based). EventMessage is emitted for each non-empty assistant message.
//     EventError is emitted for fatal, non-cancellation errors (provider
//     failure, gate failure, history failure, iteration budget). A cancelled
//     context returns ctx.Err() without an EventError. Emit may be nil.
//
// # Model precedence
//
// An Agent always sends a non-empty model to Provider.Stream. RunRequest.Model
// wins over AgentSpec.Model; an empty (or whitespace-only) RunRequest.Model
// falls back to AgentSpec.Model, which New requires to be non-empty. As a
// consequence a provider's own default model (for example
// openai.Config.DefaultModel) is never consulted on the Agent path: it applies
// only when the Provider is called directly.
//
// An Agent is safe for concurrent Run calls as long as the configured
// Provider, Tools and History are; the loop keeps no mutable state of its own.
package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/zeokeel/zeokeel-rig"
)

// DefaultMaxIterations is the model-round limit used when neither the spec nor
// the run request configures one.
const DefaultMaxIterations = 20

// agent is the eino-free rig.Agent implementation. All fields are immutable
// after New, so concurrent Run calls share them safely.
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

// New validates spec and builds an Agent.
//
// The agent's default model is spec.Model (required); RunRequest.Model overrides
// it per run. See the package documentation's "Model precedence" section.
// Validation errors wrap the contract sentinels where one exists:
// ErrProviderRequired for a nil Provider and ErrModelRequired for an empty
// default Model. Tool entries must be non-nil, have non-empty unique names;
// duplicate or unnamed tools are configuration errors and fail here rather
// than at dispatch time.
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

// Run implements rig.Agent. The model sent to the provider is req.Model when
// non-empty (trimmed), otherwise the agent's configured model; see the package
// documentation's "Model precedence" section. Run never sends an empty model.
func (a *agent) Run(ctx context.Context, req rig.RunRequest) (rig.RunResult, error) {
	if ctx == nil {
		return rig.RunResult{}, errors.New("rig: nil context")
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = a.model
	}
	if model == "" { // defensive: New rejects this, but RunRequest may be used directly
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
		// An empty completion (no text, reasoning or tool calls) is not a
		// conversation message: dropping it keeps history replayable and avoids
		// injecting empty assistant turns into the next request.
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

// run carries the mutable state of one Run call. It is confined to the run
// goroutine except where noted (tool bodies only touch their own result slot).
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
	// allowAlways remembers (tool name, arguments) approved with
	// DecisionAllowAlways for the remainder of this run.
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

// appendMessages appends messages to the conversation and to RunResult.Messages,
// persisting them when a History and session are configured.
func (r *run) appendMessages(msgs []rig.Message) error {
	r.conversation = append(r.conversation, msgs...)
	r.result.Messages = append(r.result.Messages, msgs...)
	if !r.persist {
		return nil
	}
	if err := r.a.history.Append(r.ctx, r.sessionID, msgs...); err != nil {
		return fmt.Errorf("rig: append history: %w", err)
	}
	return nil
}

// modelRound performs one model call and returns the assembled assistant
// message, the model finish reason, and any requested tool calls. Streaming
// deltas are emitted as they arrive; the assistant message is returned (not
// emitted) so the caller can append it to history and emit EventMessage.
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
	sort.Ints(indexes)
	toolCalls := make([]rig.ToolCall, 0, len(indexes))
	for _, index := range indexes {
		toolCalls = append(toolCalls, accumulators[index].call())
	}
	if len(toolCalls) > 0 {
		assistant.ToolCalls = toolCalls
	}
	return assistant, finishReason, toolCalls, nil
}

// requestMessages builds the message list for one model call: the configured
// system prompt (never persisted) followed by the current conversation.
func (r *run) requestMessages() []rig.Message {
	msgs := make([]rig.Message, 0, len(r.conversation)+1)
	if r.a.systemPrompt != "" {
		msgs = append(msgs, rig.NewMessage(rig.RoleSystem, r.a.systemPrompt))
	}
	return append(msgs, r.conversation...)
}

// toolRound resolves, approves, executes and records one batch of tool calls.
// Approval happens on the run goroutine in call order (rig.Gate contract);
// execution happens concurrently; events and conversation entries are emitted
// and appended in call order.
func (r *run) toolRound(calls []rig.ToolCall, iteration int) error {
	tools := make([]rig.Tool, len(calls))
	resolved := make([]bool, len(calls))
	for i, call := range calls {
		tools[i], resolved[i] = r.a.tools[strings.TrimSpace(call.Name)]
	}
	for _, call := range calls {
		callCopy := call
		r.emitEvent(rig.Event{Type: rig.EventToolCall, Iteration: iteration, Call: &callCopy})
	}

	// Approval runs on the run goroutine, in call order. Denied and unknown
	// calls produce tool error results instead of aborting the run.
	decisions := make([]rig.Decision, len(calls))
	deniedReason := make([]string, len(calls))
	for i, call := range calls {
		if !resolved[i] {
			decisions[i] = rig.DecisionDeny
			deniedReason[i] = fmt.Sprintf("rig: tool not found: %s", call.Name)
			continue
		}
		if _, ok := r.allowAlways[callKey(call)]; ok {
			decisions[i] = rig.DecisionAllow
			continue
		}
		if r.gate == nil {
			decisions[i] = rig.DecisionAllow
			continue
		}
		decision, err := r.gate.Approve(r.ctx, tools[i], call)
		if err != nil {
			if ctxErr := r.ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return fmt.Errorf("rig: approval gate for tool %q: %w", call.Name, err)
		}
		switch decision {
		case rig.DecisionAllow:
			decisions[i] = rig.DecisionAllow
		case rig.DecisionAllowAlways:
			decisions[i] = rig.DecisionAllow
			r.allowAlways[callKey(call)] = struct{}{}
		case rig.DecisionDeny, "":
			decisions[i] = rig.DecisionDeny
			deniedReason[i] = fmt.Sprintf("rig: tool call denied by approval gate: %s", call.Name)
		default:
			return fmt.Errorf("rig: approval gate returned unknown decision %q for tool %q", decision, call.Name)
		}
	}

	// Execute allowed calls concurrently. Each goroutine writes only its own
	// result slot, so no lock is needed.
	results := make([]rig.ToolResult, len(calls))
	var wg sync.WaitGroup
	for i, call := range calls {
		if decisions[i] != rig.DecisionAllow {
			continue
		}
		wg.Add(1)
		go func(index int, tool rig.Tool, c rig.ToolCall) {
			defer wg.Done()
			results[index] = r.invokeTool(tool, c)
		}(i, tools[i], call)
	}
	wg.Wait()
	if err := r.ctx.Err(); err != nil {
		return err
	}

	toolMessages := make([]rig.Message, 0, len(calls))
	var attachments []rig.Message
	for i, call := range calls {
		result := results[i]
		if decisions[i] != rig.DecisionAllow {
			if result.Content == "" {
				result.Content = deniedReason[i]
			}
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
		// Chat-completions tool messages are text-only. Non-text attachments
		// (images) are delivered as a follow-up user message so the model sees
		// them in the next round.
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

// invokeTool runs one tool, converting errors and panics into tool error
// results. A cancelled context is reported to the caller of toolRound through
// ctx.Err() (the check after wg.Wait): here it is simply a tool error.
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

// toolCallAccumulator assembles one streamed tool call from rig.ToolCallDelta
// fragments. ID and Name are set when they arrive; Arguments are concatenated
// in arrival order.
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

// callKey identifies an approval decision target for allow-always caching.
func callKey(call rig.ToolCall) string {
	return call.Name + "\x00" + call.Arguments
}

// cloneMessage returns a copy of msg whose Content and ToolCalls slices are not
// aliased with the input, so a caller (or a History implementation) cannot
// mutate stored state through the copy.
func cloneMessage(msg rig.Message) rig.Message {
	out := msg
	if msg.Content != nil {
		out.Content = append([]rig.ContentPart(nil), msg.Content...)
	}
	if msg.ToolCalls != nil {
		out.ToolCalls = append([]rig.ToolCall(nil), msg.ToolCalls...)
	}
	return out
}

// isEmptyMessage reports whether msg carries no content, reasoning or tool
// calls (an empty model completion).
func isEmptyMessage(msg rig.Message) bool {
	return len(msg.Content) == 0 && msg.Reasoning == "" && len(msg.ToolCalls) == 0
}

func messagePtr(msg rig.Message) *rig.Message {
	msgCopy := msg
	return &msgCopy
}
