package agent_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/agent"
	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
	"github.com/zeokeel/zeokeel-rig/session"
)

// ---------------------------------------------------------------------------
// fakes: provider / stream
// ---------------------------------------------------------------------------

// chunkStream replays a fixed chunk list and then returns io.EOF.
type chunkStream struct {
	chunks []v1.Chunk
	next   int
}

func (s *chunkStream) Recv() (v1.Chunk, error) {
	if s.next >= len(s.chunks) {
		return v1.Chunk{}, io.EOF
	}
	chunk := s.chunks[s.next]
	s.next++
	return chunk, nil
}

// errorStream replays chunks and then returns a fixed error instead of io.EOF.
type errorStream struct {
	chunks []v1.Chunk
	next   int
	err    error
}

func (s *errorStream) Recv() (v1.Chunk, error) {
	if s.next >= len(s.chunks) {
		return v1.Chunk{}, s.err
	}
	chunk := s.chunks[s.next]
	s.next++
	return chunk, nil
}

// blockingStream blocks until the context is done, then returns its error.
type blockingStream struct{ ctx context.Context }

func (s *blockingStream) Recv() (v1.Chunk, error) {
	<-s.ctx.Done()
	return v1.Chunk{}, s.ctx.Err()
}

// scriptProvider is a v1.Provider that replays chunk scripts, one per Stream
// call, and records the requests it received.
type scriptProvider struct {
	mu         sync.Mutex
	scripts    [][]v1.Chunk
	repeatLast bool
	calls      []v1.Request
	fn         func(ctx context.Context, req v1.Request) (v1.Stream, error)
}

func (p *scriptProvider) Name() string { return "script" }

func (p *scriptProvider) Stream(ctx context.Context, req v1.Request) (v1.Stream, error) {
	p.mu.Lock()
	p.calls = append(p.calls, req)
	fn := p.fn
	var chunks []v1.Chunk
	if fn == nil {
		if len(p.scripts) == 0 {
			p.mu.Unlock()
			return nil, errors.New("scriptProvider: no script left")
		}
		chunks = p.scripts[0]
		if len(p.scripts) > 1 {
			p.scripts = p.scripts[1:]
		} else if !p.repeatLast {
			p.scripts = nil
		}
	}
	p.mu.Unlock()
	if fn != nil {
		return fn(ctx, req)
	}
	return &chunkStream{chunks: chunks}, nil
}

func (p *scriptProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

func (p *scriptProvider) request(index int) v1.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[index]
}

// ---------------------------------------------------------------------------
// fakes: tools / gate / history / events
// ---------------------------------------------------------------------------

type fakeTool struct {
	name   string
	desc   string
	schema map[string]any
	fn     func(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error)

	mu    sync.Mutex
	calls []v1.ToolCall
}

func (t *fakeTool) Name() string           { return t.name }
func (t *fakeTool) Description() string    { return t.desc }
func (t *fakeTool) Schema() map[string]any { return t.schema }
func (t *fakeTool) Invoke(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error) {
	t.mu.Lock()
	t.calls = append(t.calls, call)
	t.mu.Unlock()
	if t.fn != nil {
		return t.fn(ctx, call)
	}
	return v1.ToolResult{Content: "ok:" + call.Name}, nil
}

func (t *fakeTool) callCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.calls)
}

func (t *fakeTool) lastCall(tb testing.TB) v1.ToolCall {
	tb.Helper()
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.calls) == 0 {
		tb.Fatal("tool was not invoked")
	}
	return t.calls[len(t.calls)-1]
}

// fakeGate records the calls it was asked about and delegates the decision.
type fakeGate struct {
	mu        sync.Mutex
	calls     []v1.ToolCall
	decisions []v1.Decision
	fn        func(call v1.ToolCall) (v1.Decision, error)
}

func (g *fakeGate) Approve(_ context.Context, _ v1.Tool, call v1.ToolCall) (v1.Decision, error) {
	g.mu.Lock()
	g.calls = append(g.calls, call)
	index := len(g.calls) - 1
	g.mu.Unlock()
	if g.fn != nil {
		return g.fn(call)
	}
	if index < len(g.decisions) {
		return g.decisions[index], nil
	}
	return v1.DecisionAllow, nil
}

func (g *fakeGate) callCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.calls)
}

// captureHistory is a v1.History that records writes and can be preloaded.
type captureHistory struct {
	mu       sync.Mutex
	sessions map[string][]v1.Message
	loadErr  error
}

func newCaptureHistory(seed map[string][]v1.Message) *captureHistory {
	h := &captureHistory{sessions: map[string][]v1.Message{}}
	for id, msgs := range seed {
		h.sessions[id] = append([]v1.Message(nil), msgs...)
	}
	return h
}

func (h *captureHistory) Load(_ context.Context, sessionID string) ([]v1.Message, error) {
	if h.loadErr != nil {
		return nil, h.loadErr
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]v1.Message(nil), h.sessions[sessionID]...), nil
}

func (h *captureHistory) Append(_ context.Context, sessionID string, messages ...v1.Message) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sessions[sessionID] = append(h.sessions[sessionID], messages...)
	return nil
}

func (h *captureHistory) stored(sessionID string) []v1.Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]v1.Message(nil), h.sessions[sessionID]...)
}

// recorder captures events for assertions.
type recorder struct {
	mu     sync.Mutex
	events []v1.Event
}

func (r *recorder) emit(event v1.Event) {
	r.mu.Lock()
	r.events = append(r.events, event)
	r.mu.Unlock()
}

func (r *recorder) list() []v1.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]v1.Event(nil), r.events...)
}

func (r *recorder) count(eventType v1.EventType) int {
	n := 0
	for _, event := range r.list() {
		if event.Type == eventType {
			n++
		}
	}
	return n
}

func (r *recorder) ofType(eventType v1.EventType) []v1.Event {
	var out []v1.Event
	for _, event := range r.list() {
		if event.Type == eventType {
			out = append(out, event)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func mustNewAgent(tb testing.TB, spec v1.AgentSpec) v1.Agent {
	tb.Helper()
	a, err := agent.New(spec)
	if err != nil {
		tb.Fatalf("agent.New: %v", err)
	}
	return a
}

func userMessage(text string) v1.Message { return v1.NewMessage(v1.RoleUser, text) }

func textScript(parts ...string) []v1.Chunk {
	chunks := make([]v1.Chunk, 0, len(parts)+1)
	for _, part := range parts {
		chunks = append(chunks, v1.Chunk{Delta: part})
	}
	return append(chunks, v1.Chunk{FinishReason: "stop"})
}

func finishChunk(reason string) v1.Chunk { return v1.Chunk{FinishReason: reason} }

// toolCallScript builds one model response that requests a single tool call.
func toolCallScript(id, name string, argParts ...string) []v1.Chunk {
	chunks := []v1.Chunk{{ToolCalls: []v1.ToolCallDelta{{Index: 0, ID: id, Name: name}}}}
	for _, part := range argParts {
		chunks = append(chunks, v1.Chunk{ToolCalls: []v1.ToolCallDelta{{Index: 0, Arguments: part}}})
	}
	chunks = append(chunks, finishChunk("tool_calls"))
	return chunks
}

func requireNoError(tb testing.TB, err error) {
	tb.Helper()
	if err != nil {
		tb.Fatalf("unexpected error: %v", err)
	}
}

func requireErrorIs(tb testing.TB, err, target error) {
	tb.Helper()
	if !errors.Is(err, target) {
		tb.Fatalf("error %v does not wrap %v", err, target)
	}
}

// ---------------------------------------------------------------------------
// New / validation
// ---------------------------------------------------------------------------

func TestNewValidation(t *testing.T) {
	tool := &fakeTool{name: "echo"}
	goodSpec := v1.AgentSpec{Model: "m", Provider: &scriptProvider{}}

	t.Run("nil provider", func(t *testing.T) {
		_, err := agent.New(v1.AgentSpec{Model: "m"})
		requireErrorIs(t, err, v1.ErrProviderRequired)
	})
	t.Run("empty model", func(t *testing.T) {
		_, err := agent.New(v1.AgentSpec{Provider: &scriptProvider{}})
		requireErrorIs(t, err, v1.ErrModelRequired)
	})
	t.Run("blank model", func(t *testing.T) {
		_, err := agent.New(v1.AgentSpec{Model: "   ", Provider: &scriptProvider{}})
		requireErrorIs(t, err, v1.ErrModelRequired)
	})
	t.Run("nil tool", func(t *testing.T) {
		spec := goodSpec
		spec.Tools = []v1.Tool{nil}
		if _, err := agent.New(spec); err == nil {
			t.Fatal("want error for nil tool")
		}
	})
	t.Run("empty tool name", func(t *testing.T) {
		spec := goodSpec
		spec.Tools = []v1.Tool{&fakeTool{name: " "}}
		if _, err := agent.New(spec); err == nil {
			t.Fatal("want error for empty tool name")
		}
	})
	t.Run("duplicate tool name", func(t *testing.T) {
		spec := goodSpec
		spec.Tools = []v1.Tool{tool, &fakeTool{name: "echo"}}
		if _, err := agent.New(spec); err == nil || !strings.Contains(err.Error(), "duplicate") {
			t.Fatalf("want duplicate-tool error, got %v", err)
		}
	})
	t.Run("valid", func(t *testing.T) {
		spec := goodSpec
		spec.Tools = []v1.Tool{tool}
		mustNewAgent(t, spec)
	})
}

// ---------------------------------------------------------------------------
// pure text
// ---------------------------------------------------------------------------

func TestRunPureText(t *testing.T) {
	provider := &scriptProvider{scripts: [][]v1.Chunk{{
		{Delta: "Hel"},
		{Delta: "lo"},
		{Delta: "!", Usage: &v1.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5}},
		{FinishReason: "stop"},
	}}}
	rec := &recorder{}
	a := mustNewAgent(t, v1.AgentSpec{Model: "test-model", Provider: provider})

	result, err := a.Run(context.Background(), v1.RunRequest{
		SessionID: "s1",
		Input:     []v1.Message{userMessage("hi")},
		Emit:      rec.emit,
	})
	requireNoError(t, err)

	if result.Iterations != 1 {
		t.Fatalf("Iterations = %d, want 1", result.Iterations)
	}
	if result.FinishReason != "stop" {
		t.Fatalf("FinishReason = %q, want stop", result.FinishReason)
	}
	if result.Usage != (v1.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5}) {
		t.Fatalf("Usage = %+v, want accumulated 3/2/5", result.Usage)
	}
	if len(result.Messages) != 2 {
		t.Fatalf("Messages = %d, want user+assistant (2)", len(result.Messages))
	}
	if result.Messages[1].Role != v1.RoleAssistant || result.Messages[1].Text() != "Hello!" {
		t.Fatalf("assistant message = %+v", result.Messages[1])
	}
	if provider.callCount() != 1 {
		t.Fatalf("provider calls = %d, want 1", provider.callCount())
	}
	request := provider.request(0)
	if request.Model != "test-model" || len(request.Messages) != 1 || request.Messages[0].Role != v1.RoleUser {
		t.Fatalf("request = %+v", request)
	}

	types := eventTypes(rec.list())
	want := []v1.EventType{v1.EventTurnStart, v1.EventTextDelta, v1.EventTextDelta, v1.EventTextDelta, v1.EventUsage, v1.EventMessage, v1.EventTurnEnd}
	if !equalEventTypes(types, want) {
		t.Fatalf("event types = %v, want %v", types, want)
	}
	var joined strings.Builder
	for _, event := range rec.ofType(v1.EventTextDelta) {
		joined.WriteString(event.Text)
	}
	if joined.String() != "Hello!" {
		t.Fatalf("streamed text = %q", joined.String())
	}
	if rec.ofType(v1.EventMessage)[0].Message == nil {
		t.Fatal("EventMessage has nil Message")
	}
}

// ---------------------------------------------------------------------------
// tools
// ---------------------------------------------------------------------------

func TestRunSingleToolRound(t *testing.T) {
	provider := &scriptProvider{scripts: [][]v1.Chunk{
		toolCallScript("call_1", "echo", `{"text":`, `"hi"}`),
		textScript("done"),
	}}
	echo := &fakeTool{
		name: "echo",
		fn: func(_ context.Context, call v1.ToolCall) (v1.ToolResult, error) {
			return v1.ToolResult{Content: "echoed:" + call.Arguments}, nil
		},
	}
	rec := &recorder{}
	a := mustNewAgent(t, v1.AgentSpec{Model: "m", Provider: provider, Tools: []v1.Tool{echo}})

	result, err := a.Run(context.Background(), v1.RunRequest{
		Input: []v1.Message{userMessage("use echo")},
		Emit:  rec.emit,
	})
	requireNoError(t, err)
	if result.Iterations != 2 {
		t.Fatalf("Iterations = %d, want 2", result.Iterations)
	}
	if len(result.Messages) != 4 {
		t.Fatalf("Messages = %d, want user/assistant/tool/assistant (4): %+v", len(result.Messages), result.Messages)
	}
	assistant := result.Messages[1]
	if len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].ID != "call_1" || assistant.ToolCalls[0].Name != "echo" {
		t.Fatalf("assistant tool calls = %+v", assistant.ToolCalls)
	}
	toolMessage := result.Messages[2]
	if toolMessage.Role != v1.RoleTool || toolMessage.ToolCallID != "call_1" || toolMessage.Name != "echo" {
		t.Fatalf("tool message = %+v", toolMessage)
	}
	if toolMessage.Text() != `echoed:{"text":"hi"}` {
		t.Fatalf("tool message text = %q", toolMessage.Text())
	}
	if result.Messages[3].Text() != "done" {
		t.Fatalf("final assistant text = %q", result.Messages[3].Text())
	}
	if echo.callCount() != 1 {
		t.Fatalf("echo calls = %d, want 1", echo.callCount())
	}
	if got := echo.lastCall(t); got.Arguments != `{"text":"hi"}` {
		t.Fatalf("assembled arguments = %q", got.Arguments)
	}
	if rec.count(v1.EventToolCall) != 1 || rec.count(v1.EventToolResult) != 1 {
		t.Fatalf("tool events = %d/%d", rec.count(v1.EventToolCall), rec.count(v1.EventToolResult))
	}
	second := provider.request(1)
	if len(second.Messages) != 3 || second.Messages[2].Role != v1.RoleTool {
		t.Fatalf("second request messages = %+v", second.Messages)
	}
	if len(second.Tools) != 1 || second.Tools[0].Name != "echo" {
		t.Fatalf("second request tools = %+v", second.Tools)
	}
}

func TestRunTwoToolRounds(t *testing.T) {
	provider := &scriptProvider{scripts: [][]v1.Chunk{
		toolCallScript("c1", "echo", `{}`),
		toolCallScript("c2", "echo", `{}`),
		textScript("all done"),
	}}
	echo := &fakeTool{name: "echo"}
	a := mustNewAgent(t, v1.AgentSpec{Model: "m", Provider: provider, Tools: []v1.Tool{echo}})

	result, err := a.Run(context.Background(), v1.RunRequest{Input: []v1.Message{userMessage("go")}})
	requireNoError(t, err)
	if result.Iterations != 3 {
		t.Fatalf("Iterations = %d, want 3", result.Iterations)
	}
	if len(result.Messages) != 6 {
		t.Fatalf("Messages = %d, want 6", len(result.Messages))
	}
	if echo.callCount() != 2 {
		t.Fatalf("echo calls = %d, want 2", echo.callCount())
	}
	if result.FinishReason != "stop" {
		t.Fatalf("FinishReason = %q", result.FinishReason)
	}
}

func TestRunParallelTools(t *testing.T) {
	provider := &scriptProvider{scripts: [][]v1.Chunk{
		{
			{ToolCalls: []v1.ToolCallDelta{
				{Index: 0, ID: "call_a", Name: "t0"},
				{Index: 1, ID: "call_b", Name: "t1"},
			}},
			{ToolCalls: []v1.ToolCallDelta{
				{Index: 0, Arguments: `{"n":0}`},
				{Index: 1, Arguments: `{"n":1}`},
			}},
			finishChunk("tool_calls"),
		},
		textScript("done"),
	}}

	var started sync.WaitGroup
	started.Add(2)
	bothStarted := make(chan struct{})
	go func() {
		started.Wait()
		close(bothStarted)
	}()
	// Each tool waits until both are running; if the agent executed them
	// sequentially the first one times out and the test sees an error result.
	waitForPeer := func(_ context.Context, call v1.ToolCall) (v1.ToolResult, error) {
		started.Done()
		select {
		case <-bothStarted:
			return v1.ToolResult{Content: "parallel:" + call.Name}, nil
		case <-time.After(2 * time.Second):
			return v1.ToolResult{Content: "sequential-execution-detected", IsError: true}, nil
		}
	}
	t0 := &fakeTool{name: "t0", fn: waitForPeer}
	t1 := &fakeTool{name: "t1", fn: waitForPeer}
	rec := &recorder{}
	a := mustNewAgent(t, v1.AgentSpec{Model: "m", Provider: provider, Tools: []v1.Tool{t0, t1}})

	result, err := a.Run(context.Background(), v1.RunRequest{Input: []v1.Message{userMessage("run both")}, Emit: rec.emit})
	requireNoError(t, err)
	if len(result.Messages) != 5 {
		t.Fatalf("Messages = %d, want 5 (user/assistant/2x tool/assistant): %+v", len(result.Messages), result.Messages)
	}
	// Results are appended in call order even though call_a/call_b are
	// requested as one batch; execution order is concurrently free.
	if result.Messages[2].ToolCallID != "call_a" || result.Messages[3].ToolCallID != "call_b" {
		t.Fatalf("tool message order = %q, %q; want call_a, call_b",
			result.Messages[2].ToolCallID, result.Messages[3].ToolCallID)
	}
	if strings.Contains(result.Messages[2].Text(), "sequential") || strings.Contains(result.Messages[3].Text(), "sequential") {
		t.Fatalf("tools did not run in parallel: %q / %q", result.Messages[2].Text(), result.Messages[3].Text())
	}
	results := rec.ofType(v1.EventToolResult)
	if len(results) != 2 || results[0].Call.ID != "call_a" || results[1].Call.ID != "call_b" {
		t.Fatalf("tool result events = %+v", results)
	}
	for _, event := range results {
		if event.Result == nil || event.Result.IsError {
			t.Fatalf("tool result event = %+v", event)
		}
	}
	if t0.callCount() != 1 || t1.callCount() != 1 {
		t.Fatalf("tool call counts = %d/%d", t0.callCount(), t1.callCount())
	}
}

// ---------------------------------------------------------------------------
// iterations / gate
// ---------------------------------------------------------------------------

func TestRunMaxIterations(t *testing.T) {
	provider := &scriptProvider{
		scripts:    [][]v1.Chunk{toolCallScript("c1", "echo", `{}`)},
		repeatLast: true,
	}
	echo := &fakeTool{name: "echo"}
	rec := &recorder{}
	a := mustNewAgent(t, v1.AgentSpec{Model: "m", Provider: provider, Tools: []v1.Tool{echo}})

	result, err := a.Run(context.Background(), v1.RunRequest{
		Input:         []v1.Message{userMessage("loop")},
		MaxIterations: 2,
		Emit:          rec.emit,
	})
	requireErrorIs(t, err, v1.ErrMaxIterations)
	if result.Iterations != 2 {
		t.Fatalf("Iterations = %d, want 2", result.Iterations)
	}
	if len(result.Messages) != 5 {
		t.Fatalf("Messages = %d, want 5 (user + 2x assistant + 2x tool): %+v", len(result.Messages), result.Messages)
	}
	if echo.callCount() != 2 {
		t.Fatalf("echo calls = %d, want 2", echo.callCount())
	}
	if rec.count(v1.EventToolCall) != 2 || rec.count(v1.EventToolResult) != 2 {
		t.Fatalf("tool events = %d/%d", rec.count(v1.EventToolCall), rec.count(v1.EventToolResult))
	}
	if rec.count(v1.EventTurnEnd) != 2 {
		t.Fatalf("turn_end events = %d, want 2", rec.count(v1.EventTurnEnd))
	}
	errorEvents := rec.ofType(v1.EventError)
	if len(errorEvents) != 1 || !errors.Is(errorEvents[0].Err, v1.ErrMaxIterations) {
		t.Fatalf("error events = %+v", errorEvents)
	}
}

func TestRunGateDeny(t *testing.T) {
	provider := &scriptProvider{scripts: [][]v1.Chunk{
		toolCallScript("c1", "echo", `{}`),
		textScript("recovered"),
	}}
	echo := &fakeTool{name: "echo"}
	gate := &fakeGate{decisions: []v1.Decision{v1.DecisionDeny}}
	rec := &recorder{}
	a := mustNewAgent(t, v1.AgentSpec{Model: "m", Provider: provider, Tools: []v1.Tool{echo}})

	result, err := a.Run(context.Background(), v1.RunRequest{
		Input: []v1.Message{userMessage("try")},
		Gate:  gate,
		Emit:  rec.emit,
	})
	requireNoError(t, err)
	if echo.callCount() != 0 {
		t.Fatalf("denied tool was executed %d times", echo.callCount())
	}
	if gate.callCount() != 1 {
		t.Fatalf("gate calls = %d, want 1", gate.callCount())
	}
	if len(result.Messages) != 4 {
		t.Fatalf("Messages = %d, want 4", len(result.Messages))
	}
	toolMessage := result.Messages[2]
	if toolMessage.Role != v1.RoleTool || !strings.Contains(toolMessage.Text(), "denied") {
		t.Fatalf("denied tool message = %+v", toolMessage)
	}
	results := rec.ofType(v1.EventToolResult)
	if len(results) != 1 || results[0].Result == nil || !results[0].Result.IsError {
		t.Fatalf("tool result events = %+v", results)
	}
	// The run continued and produced a final answer after the denial.
	if result.Messages[3].Text() != "recovered" || result.Iterations != 2 {
		t.Fatalf("final message = %+v, iterations=%d", result.Messages[3], result.Iterations)
	}
}

func TestRunGateAllowAlways(t *testing.T) {
	provider := &scriptProvider{scripts: [][]v1.Chunk{
		toolCallScript("c1", "echo", `{"n":1}`),
		toolCallScript("c2", "echo", `{"n":1}`),
		textScript("done"),
	}}
	echo := &fakeTool{name: "echo"}
	gate := &fakeGate{fn: func(v1.ToolCall) (v1.Decision, error) {
		return v1.DecisionAllowAlways, nil
	}}
	a := mustNewAgent(t, v1.AgentSpec{Model: "m", Provider: provider, Tools: []v1.Tool{echo}})

	result, err := a.Run(context.Background(), v1.RunRequest{
		Input: []v1.Message{userMessage("twice")},
		Gate:  gate,
	})
	requireNoError(t, err)
	if gate.callCount() != 1 {
		t.Fatalf("gate calls = %d, want 1 (identical call auto-allowed)", gate.callCount())
	}
	if echo.callCount() != 2 {
		t.Fatalf("echo calls = %d, want 2", echo.callCount())
	}
	if result.Iterations != 3 {
		t.Fatalf("Iterations = %d, want 3", result.Iterations)
	}
}

func TestRunGateError(t *testing.T) {
	provider := &scriptProvider{scripts: [][]v1.Chunk{toolCallScript("c1", "echo", `{}`)}}
	echo := &fakeTool{name: "echo"}
	gate := &fakeGate{fn: func(v1.ToolCall) (v1.Decision, error) {
		return "", errors.New("gate exploded")
	}}
	rec := &recorder{}
	a := mustNewAgent(t, v1.AgentSpec{Model: "m", Provider: provider, Tools: []v1.Tool{echo}})

	_, err := a.Run(context.Background(), v1.RunRequest{Input: []v1.Message{userMessage("x")}, Gate: gate, Emit: rec.emit})
	if err == nil || !strings.Contains(err.Error(), "gate exploded") {
		t.Fatalf("error = %v, want gate failure", err)
	}
	if echo.callCount() != 0 {
		t.Fatalf("tool executed after gate error")
	}
	if rec.count(v1.EventError) != 1 {
		t.Fatalf("error events = %d, want 1", rec.count(v1.EventError))
	}
}

// ---------------------------------------------------------------------------
// cancellation / provider failures
// ---------------------------------------------------------------------------

func TestRunContextCancel(t *testing.T) {
	started := make(chan struct{})
	var once sync.Once
	provider := &scriptProvider{fn: func(ctx context.Context, _ v1.Request) (v1.Stream, error) {
		once.Do(func() { close(started) })
		return &blockingStream{ctx: ctx}, nil
	}}
	rec := &recorder{}
	a := mustNewAgent(t, v1.AgentSpec{Model: "m", Provider: provider})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := a.Run(ctx, v1.RunRequest{Input: []v1.Message{userMessage("hi")}, Emit: rec.emit})
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		requireErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	if rec.count(v1.EventError) != 0 {
		t.Fatalf("cancellation should not emit EventError, got %d", rec.count(v1.EventError))
	}
}

func TestRunProviderMidStreamError(t *testing.T) {
	boom := errors.New("boom")
	provider := &scriptProvider{fn: func(context.Context, v1.Request) (v1.Stream, error) {
		return &errorStream{chunks: []v1.Chunk{{Delta: "partial"}}, err: boom}, nil
	}}
	rec := &recorder{}
	a := mustNewAgent(t, v1.AgentSpec{Model: "m", Provider: provider})

	result, err := a.Run(context.Background(), v1.RunRequest{Input: []v1.Message{userMessage("hi")}, Emit: rec.emit})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error = %v, want stream failure", err)
	}
	if result.Iterations != 1 {
		t.Fatalf("Iterations = %d, want 1", result.Iterations)
	}
	if len(result.Messages) != 1 {
		t.Fatalf("Messages = %v, want only the user input", result.Messages)
	}
	if rec.count(v1.EventTextDelta) != 1 {
		t.Fatalf("text deltas = %d, want 1 (partial output before the error)", rec.count(v1.EventTextDelta))
	}
	if rec.count(v1.EventError) != 1 {
		t.Fatalf("error events = %d, want 1", rec.count(v1.EventError))
	}
	if rec.count(v1.EventMessage) != 0 {
		t.Fatalf("partial assistant message must not be emitted, got %d", rec.count(v1.EventMessage))
	}
}

// ---------------------------------------------------------------------------
// usage / finish reason / empty completion
// ---------------------------------------------------------------------------

func TestRunUsageAccumulation(t *testing.T) {
	provider := &scriptProvider{scripts: [][]v1.Chunk{
		append(toolCallScript("c1", "echo", `{}`)[:1],
			v1.Chunk{Usage: &v1.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}},
			finishChunk("tool_calls")),
		{
			{Delta: "done", Usage: &v1.Usage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30, CachedTokens: 5}},
			finishChunk("stop"),
		},
	}}
	echo := &fakeTool{name: "echo"}
	rec := &recorder{}
	a := mustNewAgent(t, v1.AgentSpec{Model: "m", Provider: provider, Tools: []v1.Tool{echo}})

	result, err := a.Run(context.Background(), v1.RunRequest{Input: []v1.Message{userMessage("hi")}, Emit: rec.emit})
	requireNoError(t, err)
	if result.Usage != (v1.Usage{PromptTokens: 11, CompletionTokens: 22, TotalTokens: 33, CachedTokens: 5}) {
		t.Fatalf("Usage = %+v", result.Usage)
	}
	if rec.count(v1.EventUsage) != 2 {
		t.Fatalf("usage events = %d, want 2", rec.count(v1.EventUsage))
	}
}

func TestRunFinishReasonLength(t *testing.T) {
	provider := &scriptProvider{scripts: [][]v1.Chunk{{
		{Delta: "truncated"},
		finishChunk("length"),
	}}}
	a := mustNewAgent(t, v1.AgentSpec{Model: "m", Provider: provider})
	result, err := a.Run(context.Background(), v1.RunRequest{Input: []v1.Message{userMessage("hi")}})
	requireNoError(t, err)
	if result.FinishReason != "length" {
		t.Fatalf("FinishReason = %q, want length", result.FinishReason)
	}
	if result.Messages[1].Text() != "truncated" {
		t.Fatalf("assistant text = %q", result.Messages[1].Text())
	}
}

func TestRunEmptyCompletion(t *testing.T) {
	provider := &scriptProvider{scripts: [][]v1.Chunk{{
		finishChunk("stop"),
	}}}
	rec := &recorder{}
	a := mustNewAgent(t, v1.AgentSpec{Model: "m", Provider: provider})
	result, err := a.Run(context.Background(), v1.RunRequest{Input: []v1.Message{userMessage("hi")}, Emit: rec.emit})
	requireNoError(t, err)
	if len(result.Messages) != 1 || result.Messages[0].Role != v1.RoleUser {
		t.Fatalf("Messages = %+v, want only the user input", result.Messages)
	}
	if rec.count(v1.EventMessage) != 0 {
		t.Fatalf("empty completion emitted EventMessage")
	}
	if result.FinishReason != "stop" || result.Iterations != 1 {
		t.Fatalf("FinishReason=%q Iterations=%d", result.FinishReason, result.Iterations)
	}
}

// ---------------------------------------------------------------------------
// history / system prompt / model override / nil options
// ---------------------------------------------------------------------------

func TestRunHistoryPersistenceWithSessionMemory(t *testing.T) {
	provider := &scriptProvider{scripts: [][]v1.Chunk{
		toolCallScript("c1", "echo", `{}`),
		textScript("done"),
	}}
	echo := &fakeTool{name: "echo"}
	memory := session.NewMemory()
	a := mustNewAgent(t, v1.AgentSpec{Model: "m", Provider: provider, History: memory, Tools: []v1.Tool{echo}})

	result, err := a.Run(context.Background(), v1.RunRequest{
		SessionID: "s1",
		Input:     []v1.Message{userMessage("hello")},
	})
	requireNoError(t, err)
	if len(result.Messages) != 4 {
		t.Fatalf("Messages = %d, want 4", len(result.Messages))
	}
	if memory.Len("s1") != 4 {
		t.Fatalf("history length = %d, want 4", memory.Len("s1"))
	}
	stored, err := memory.Load(context.Background(), "s1")
	requireNoError(t, err)
	roles := []v1.Role{v1.RoleUser, v1.RoleAssistant, v1.RoleTool, v1.RoleAssistant}
	if len(stored) != len(roles) {
		t.Fatalf("stored = %d messages", len(stored))
	}
	for i, role := range roles {
		if stored[i].Role != role {
			t.Fatalf("stored[%d].Role = %q, want %q", i, stored[i].Role, role)
		}
	}

	// A second run on the same session loads the prior conversation.
	provider.scripts = [][]v1.Chunk{textScript("again")}
	result2, err := a.Run(context.Background(), v1.RunRequest{
		SessionID: "s1",
		Input:     []v1.Message{userMessage("second")},
	})
	requireNoError(t, err)
	if len(result2.Messages) != 2 {
		t.Fatalf("second run Messages = %d, want 2", len(result2.Messages))
	}
	first := provider.request(2) // calls 0/1 were the first run's two rounds
	if len(first.Messages) != 5 {
		t.Fatalf("second run first request has %d messages, want 5 (4 history + input)", len(first.Messages))
	}
	if first.Messages[0].Text() != "hello" || first.Messages[4].Text() != "second" {
		t.Fatalf("history not loaded in order: %+v", first.Messages)
	}
	if memory.Len("s1") != 6 {
		t.Fatalf("history length = %d, want 6", memory.Len("s1"))
	}
}

func TestRunNilHistoryAndEmptySession(t *testing.T) {
	provider := &scriptProvider{scripts: [][]v1.Chunk{toolCallScript("c1", "echo", `{}`), textScript("done")}}
	echo := &fakeTool{name: "echo"}
	a := mustNewAgent(t, v1.AgentSpec{Model: "m", Provider: provider, Tools: []v1.Tool{echo}})

	result, err := a.Run(context.Background(), v1.RunRequest{SessionID: "ignored", Input: []v1.Message{userMessage("hi")}})
	requireNoError(t, err)
	if len(result.Messages) != 4 {
		t.Fatalf("Messages = %d, want 4 even without History", len(result.Messages))
	}

	// Empty SessionID means "fresh history": nothing is persisted.
	memory := session.NewMemory()
	a = mustNewAgent(t, v1.AgentSpec{Model: "m", Provider: &scriptProvider{scripts: [][]v1.Chunk{textScript("x")}}, History: memory})
	_, err = a.Run(context.Background(), v1.RunRequest{Input: []v1.Message{userMessage("fresh")}})
	requireNoError(t, err)
	if memory.Len("") != 0 {
		t.Fatalf("empty session ID persisted %d messages", memory.Len(""))
	}
}

func TestRunLoadsPreloadedHistory(t *testing.T) {
	history := newCaptureHistory(map[string][]v1.Message{
		"s1": {v1.NewMessage(v1.RoleUser, "old user"), v1.NewMessage(v1.RoleAssistant, "old answer")},
	})
	provider := &scriptProvider{scripts: [][]v1.Chunk{textScript("new answer")}}
	a := mustNewAgent(t, v1.AgentSpec{Model: "m", Provider: provider, History: history})
	result, err := a.Run(context.Background(), v1.RunRequest{
		SessionID: "s1",
		Input:     []v1.Message{userMessage("new question")},
	})
	requireNoError(t, err)
	if len(result.Messages) != 2 {
		t.Fatalf("Messages = %d, want only new messages (2)", len(result.Messages))
	}
	request := provider.request(0)
	if len(request.Messages) != 3 || request.Messages[0].Text() != "old user" || request.Messages[2].Text() != "new question" {
		t.Fatalf("request messages = %+v", request.Messages)
	}
	if stored := history.stored("s1"); len(stored) != 4 {
		t.Fatalf("stored messages = %d, want 4", len(stored))
	}
}

func TestRunSystemPromptNotPersisted(t *testing.T) {
	provider := &scriptProvider{scripts: [][]v1.Chunk{textScript("answer")}}
	memory := session.NewMemory()
	a := mustNewAgent(t, v1.AgentSpec{Model: "m", Provider: provider, History: memory, SystemPrompt: "be terse"})
	result, err := a.Run(context.Background(), v1.RunRequest{SessionID: "s1", Input: []v1.Message{userMessage("hi")}})
	requireNoError(t, err)
	request := provider.request(0)
	if len(request.Messages) != 2 || request.Messages[0].Role != v1.RoleSystem || request.Messages[0].Text() != "be terse" {
		t.Fatalf("system prompt not prepended: %+v", request.Messages)
	}
	if memory.Len("s1") != 2 {
		t.Fatalf("history length = %d, want 2 (system prompt must not be persisted)", memory.Len("s1"))
	}
	if len(result.Messages) != 2 || result.Messages[0].Role != v1.RoleUser {
		t.Fatalf("run messages include the system prompt: %+v", result.Messages)
	}
}

func TestRunModelOverride(t *testing.T) {
	provider := &scriptProvider{scripts: [][]v1.Chunk{textScript("a"), textScript("b")}}
	a := mustNewAgent(t, v1.AgentSpec{Model: "default-model", Provider: provider})
	_, err := a.Run(context.Background(), v1.RunRequest{Model: "override-model", Input: []v1.Message{userMessage("hi")}})
	requireNoError(t, err)
	_, err = a.Run(context.Background(), v1.RunRequest{Input: []v1.Message{userMessage("hi again")}})
	requireNoError(t, err)
	if provider.request(0).Model != "override-model" {
		t.Fatalf("request 0 model = %q", provider.request(0).Model)
	}
	if provider.request(1).Model != "default-model" {
		t.Fatalf("request 1 model = %q", provider.request(1).Model)
	}
}

func TestRunSamplingAndToolSpecs(t *testing.T) {
	temperature := 0.3
	topP := 0.9
	provider := &scriptProvider{scripts: [][]v1.Chunk{textScript("ok")}}
	echo := &fakeTool{name: "echo", desc: "echoes", schema: map[string]any{"type": "object"}}
	a := mustNewAgent(t, v1.AgentSpec{
		Model: "m", Provider: provider, Tools: []v1.Tool{echo},
		Temperature: &temperature, TopP: &topP, MaxTokens: 256,
	})
	_, err := a.Run(context.Background(), v1.RunRequest{Input: []v1.Message{userMessage("hi")}})
	requireNoError(t, err)
	request := provider.request(0)
	if request.Temperature == nil || *request.Temperature != temperature {
		t.Fatalf("Temperature = %v", request.Temperature)
	}
	if request.TopP == nil || *request.TopP != topP {
		t.Fatalf("TopP = %v", request.TopP)
	}
	if request.MaxTokens != 256 {
		t.Fatalf("MaxTokens = %d", request.MaxTokens)
	}
	if len(request.Tools) != 1 || request.Tools[0].Name != "echo" || request.Tools[0].Description != "echoes" {
		t.Fatalf("Tools = %+v", request.Tools)
	}
	if request.Tools[0].Parameters["type"] != "object" {
		t.Fatalf("tool schema = %+v", request.Tools[0].Parameters)
	}
}

// ---------------------------------------------------------------------------
// error backfill
// ---------------------------------------------------------------------------

func TestRunUnknownToolBackfilled(t *testing.T) {
	provider := &scriptProvider{scripts: [][]v1.Chunk{
		toolCallScript("c1", "missing-tool", `{}`),
		textScript("corrected"),
	}}
	rec := &recorder{}
	a := mustNewAgent(t, v1.AgentSpec{Model: "m", Provider: provider})

	result, err := a.Run(context.Background(), v1.RunRequest{Input: []v1.Message{userMessage("x")}, Emit: rec.emit})
	requireNoError(t, err)
	if len(result.Messages) != 4 {
		t.Fatalf("Messages = %d, want 4", len(result.Messages))
	}
	toolMessage := result.Messages[2]
	if toolMessage.Role != v1.RoleTool || !strings.Contains(toolMessage.Text(), "tool not found") {
		t.Fatalf("unknown tool message = %+v", toolMessage)
	}
	toolMessages := rec.ofType(v1.EventToolResult)
	if len(toolMessages) != 1 || !toolMessages[0].Result.IsError {
		t.Fatalf("tool result events = %+v", toolMessages)
	}
	if result.Messages[3].Text() != "corrected" {
		t.Fatalf("loop did not continue after unknown tool")
	}
}

func TestRunToolErrorBackfilled(t *testing.T) {
	provider := &scriptProvider{scripts: [][]v1.Chunk{
		toolCallScript("c1", "flaky", `{}`),
		textScript("recovered"),
	}}
	flaky := &fakeTool{name: "flaky", fn: func(context.Context, v1.ToolCall) (v1.ToolResult, error) {
		return v1.ToolResult{}, errors.New("disk on fire")
	}}
	rec := &recorder{}
	a := mustNewAgent(t, v1.AgentSpec{Model: "m", Provider: provider, Tools: []v1.Tool{flaky}})
	result, err := a.Run(context.Background(), v1.RunRequest{Input: []v1.Message{userMessage("x")}, Emit: rec.emit})
	requireNoError(t, err)
	if !strings.Contains(result.Messages[2].Text(), "disk on fire") {
		t.Fatalf("tool error text = %q", result.Messages[2].Text())
	}
	events := rec.ofType(v1.EventToolResult)
	if len(events) != 1 || events[0].Result.IsError != true {
		t.Fatalf("tool result events = %+v", events)
	}
	if result.Messages[3].Text() != "recovered" {
		t.Fatalf("loop did not continue after tool error")
	}
}

// ---------------------------------------------------------------------------
// nil options / concurrency
// ---------------------------------------------------------------------------

func TestRunNilEmitAndNilGate(t *testing.T) {
	provider := &scriptProvider{scripts: [][]v1.Chunk{
		toolCallScript("c1", "echo", `{}`),
		textScript("done"),
	}}
	echo := &fakeTool{name: "echo"}
	a := mustNewAgent(t, v1.AgentSpec{Model: "m", Provider: provider, Tools: []v1.Tool{echo}})
	result, err := a.Run(context.Background(), v1.RunRequest{Input: []v1.Message{userMessage("hi")}}) // nil Emit, nil Gate
	requireNoError(t, err)
	if result.Iterations != 2 || echo.callCount() != 1 {
		t.Fatalf("iterations=%d echo=%d", result.Iterations, echo.callCount())
	}
}

func TestRunConcurrentSessions(t *testing.T) {
	var calls atomic.Int32
	provider := &scriptProvider{fn: func(_ context.Context, req v1.Request) (v1.Stream, error) {
		calls.Add(1)
		text := "answer:" + req.Messages[len(req.Messages)-1].Text()
		return &chunkStream{chunks: []v1.Chunk{{Delta: text}, finishChunk("stop")}}, nil
	}}
	memory := session.NewMemory()
	a := mustNewAgent(t, v1.AgentSpec{Model: "m", Provider: provider, History: memory})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sessionID := "sess-" + string(rune('a'+i))
			if _, err := a.Run(context.Background(), v1.RunRequest{
				SessionID: sessionID,
				Input:     []v1.Message{userMessage("q")},
			}); err != nil {
				t.Errorf("run %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if calls.Load() != 8 {
		t.Fatalf("provider calls = %d, want 8", calls.Load())
	}
	for i := 0; i < 8; i++ {
		sessionID := "sess-" + string(rune('a'+i))
		if memory.Len(sessionID) != 2 {
			t.Fatalf("session %s history length = %d, want 2", sessionID, memory.Len(sessionID))
		}
	}
}

// ---------------------------------------------------------------------------
// small assertion helpers
// ---------------------------------------------------------------------------

func eventTypes(events []v1.Event) []v1.EventType {
	types := make([]v1.EventType, len(events))
	for i, event := range events {
		types[i] = event.Type
	}
	return types
}

func equalEventTypes(got, want []v1.EventType) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
