package kit_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/kit"
)

// ---------------------------------------------------------------------------
// fakes: provider / stream
// ---------------------------------------------------------------------------

// chunkStream replays a fixed chunk list and then returns io.EOF.
type chunkStream struct {
	chunks []kit.Chunk
	next   int
}

// Recv implements kit.Stream.
func (s *chunkStream) Recv() (kit.Chunk, error) {
	if s.next >= len(s.chunks) {
		return kit.Chunk{}, io.EOF
	}
	chunk := s.chunks[s.next]
	s.next++
	return chunk, nil
}

// scriptProvider is a kit.Provider that replays one chunk script per Stream call
// and records the requests. It proves a custom provider only needs kit types.
type scriptProvider struct {
	mu      sync.Mutex
	scripts [][]kit.Chunk
	calls   []kit.Request
}

// Name implements kit.Provider.
func (p *scriptProvider) Name() string { return "script" }

// Stream implements kit.Provider.
func (p *scriptProvider) Stream(ctx context.Context, req kit.Request) (kit.Stream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, req)
	if len(p.scripts) == 0 {
		return nil, errors.New("scriptProvider: no script left")
	}
	script := p.scripts[0]
	p.scripts = p.scripts[1:]
	return &chunkStream{chunks: script}, nil
}

func (p *scriptProvider) request(index int) kit.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[index]
}

func (p *scriptProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

// textScript builds one model response made of text deltas and a stop finish.
func textScript(parts ...string) []kit.Chunk {
	chunks := make([]kit.Chunk, 0, len(parts)+1)
	for _, part := range parts {
		chunks = append(chunks, kit.Chunk{Delta: part})
	}
	return append(chunks, kit.Chunk{FinishReason: "stop"})
}

// toolCallScript builds one model response requesting the given tool calls in
// one round.
func toolCallScript(calls ...kit.ToolCall) []kit.Chunk {
	deltas := make([]kit.ToolCallDelta, len(calls))
	for i, call := range calls {
		deltas[i] = kit.ToolCallDelta{Index: i, ID: call.ID, Name: call.Name, Arguments: call.Arguments}
	}
	return []kit.Chunk{{ToolCalls: deltas}, {FinishReason: "tool_calls"}}
}

// ---------------------------------------------------------------------------
// fakes: tool / gate / history
// ---------------------------------------------------------------------------

// fakeTool is a kit.Tool that records its calls.
type fakeTool struct {
	name string
	fn   func(ctx context.Context, call kit.ToolCall) (kit.ToolResult, error)

	mu    sync.Mutex
	calls []kit.ToolCall
}

func (t *fakeTool) Name() string           { return t.name }
func (t *fakeTool) Description() string    { return "fake " + t.name }
func (t *fakeTool) Schema() map[string]any { return map[string]any{"type": "object"} }

func (t *fakeTool) Invoke(ctx context.Context, call kit.ToolCall) (kit.ToolResult, error) {
	t.mu.Lock()
	t.calls = append(t.calls, call)
	fn := t.fn
	t.mu.Unlock()
	if fn != nil {
		return fn(ctx, call)
	}
	return kit.ToolResult{Content: "ok"}, nil
}

func (t *fakeTool) callCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.calls)
}

// recordingGate is a kit.Gate that records approved tool names.
type recordingGate struct {
	decision kit.Decision // "" means allow

	mu    sync.Mutex
	calls []string
}

func (g *recordingGate) Approve(ctx context.Context, tool kit.Tool, call kit.ToolCall) (kit.Decision, error) {
	name := call.Name
	if tool != nil {
		name = tool.Name()
	}
	g.mu.Lock()
	g.calls = append(g.calls, name)
	decision := g.decision
	g.mu.Unlock()
	if decision == "" {
		return kit.DecisionAllow, nil
	}
	return decision, nil
}

func (g *recordingGate) approved() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.calls...)
}

// recordingHistory is a kit.History that counts loads and stores messages.
type recordingHistory struct {
	mu       sync.Mutex
	sessions map[string][]kit.Message
	loads    int
}

func newRecordingHistory() *recordingHistory {
	return &recordingHistory{sessions: make(map[string][]kit.Message)}
}

func (h *recordingHistory) Load(ctx context.Context, sessionID string) ([]kit.Message, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.loads++
	return append([]kit.Message(nil), h.sessions[sessionID]...), nil
}

func (h *recordingHistory) Append(ctx context.Context, sessionID string, messages ...kit.Message) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sessions[sessionID] = append(h.sessions[sessionID], messages...)
	return nil
}

func (h *recordingHistory) messages(sessionID string) []kit.Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]kit.Message(nil), h.sessions[sessionID]...)
}

func (h *recordingHistory) loadCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.loads
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func mustNew(tb testing.TB, cfg kit.Config) *kit.Agent {
	tb.Helper()
	a, err := kit.New(cfg)
	if err != nil {
		tb.Fatalf("kit.New: %v", err)
	}
	return a
}

func toolNames(specs []kit.ToolSpec) map[string]bool {
	names := make(map[string]bool, len(specs))
	for _, spec := range specs {
		names[spec.Name] = true
	}
	return names
}

func findToolMessage(messages []kit.Message, name string) (kit.Message, bool) {
	for _, message := range messages {
		if message.Role == kit.RoleTool && message.Name == name {
			return message, true
		}
	}
	return kit.Message{}, false
}

func eventTypes(events []kit.Event) []kit.EventType {
	types := make([]kit.EventType, len(events))
	for i, event := range events {
		types[i] = event.Type
	}
	return types
}

// ---------------------------------------------------------------------------
// version / construction
// ---------------------------------------------------------------------------

func TestVersionMatchesModule(t *testing.T) {
	if kit.Version == "" {
		t.Fatal("kit.Version is empty")
	}
	if kit.Version != rig.Version {
		t.Fatalf("kit.Version = %q, want rig.Version %q", kit.Version, rig.Version)
	}
	if parts := strings.Split(kit.Version, "."); len(parts) != 3 {
		t.Fatalf("kit.Version = %q, want numeric major.minor.patch", kit.Version)
	}
}

func TestNewValidation(t *testing.T) {
	t.Run("model required", func(t *testing.T) {
		_, err := kit.New(kit.Config{})
		if !errors.Is(err, rig.ErrModelRequired) {
			t.Fatalf("kit.New error = %v, want rig.ErrModelRequired", err)
		}
	})
	t.Run("blank model", func(t *testing.T) {
		_, err := kit.New(kit.Config{Model: "   "})
		if !errors.Is(err, rig.ErrModelRequired) {
			t.Fatalf("kit.New error = %v, want rig.ErrModelRequired", err)
		}
	})
	t.Run("invalid base URL", func(t *testing.T) {
		_, err := kit.New(kit.Config{Model: "m", BaseURL: "ftp://example.com/v1"})
		if err == nil {
			t.Fatal("kit.New accepted a non-HTTP base URL")
		}
	})
	t.Run("missing workspace root", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "does-not-exist")
		_, err := kit.New(kit.Config{Model: "m", Root: root})
		if err == nil || !strings.Contains(err.Error(), "workspace root") {
			t.Fatalf("kit.New error = %v, want a workspace-root error", err)
		}
	})
	t.Run("workspace root is a file", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(root, []byte("x"), 0o600); err != nil {
			t.Fatalf("write root file: %v", err)
		}
		_, err := kit.New(kit.Config{Model: "m", Root: root})
		if err == nil || !strings.Contains(err.Error(), "not a directory") {
			t.Fatalf("kit.New error = %v, want a not-a-directory error", err)
		}
	})
	t.Run("duplicate custom tools", func(t *testing.T) {
		_, err := kit.New(kit.Config{
			Model:    "m",
			Provider: &scriptProvider{},
			Tools:    []kit.Tool{&fakeTool{name: "echo"}, &fakeTool{name: "echo"}},
		})
		if err == nil {
			t.Fatal("kit.New accepted duplicate tool names")
		}
	})
	t.Run("no API key needed for construction", func(t *testing.T) {
		t.Setenv("OPENAI_API_KEY", "")
		a, err := kit.New(kit.Config{Model: "gpt-4o-mini", Root: t.TempDir()})
		if err != nil {
			t.Fatalf("kit.New without an API key: %v", err)
		}
		if a == nil {
			t.Fatal("kit.New returned a nil agent")
		}
	})
}

// TestAgentImplementsRigAgent pins the escape-hatch property: *kit.Agent is a
// rig.Agent, so it can be passed to any API that expects the interface.
func TestAgentImplementsRigAgent(t *testing.T) {
	a := mustNew(t, kit.Config{Model: "m", Provider: &scriptProvider{}, Tools: []kit.Tool{}})
	// The assignment is the assertion: it only compiles when *kit.Agent
	// implements rig.Agent, which is the escape-hatch contract.
	var contract rig.Agent = a
	_ = contract
}

// ---------------------------------------------------------------------------
// default wiring / RunText
// ---------------------------------------------------------------------------

// TestDefaultAssemblyRunsOffline builds the default provider/tools/gate/history
// wiring and runs one text-only turn against a fake provider (no network).
func TestDefaultAssemblyRunsOffline(t *testing.T) {
	provider := &scriptProvider{scripts: [][]kit.Chunk{textScript("hello")}}
	a := mustNew(t, kit.Config{Model: "test-model", Provider: provider, Root: t.TempDir()})

	var events []kit.Event
	text, result, err := a.RunText(context.Background(), "s1", "hi", func(event kit.Event) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatalf("RunText: %v", err)
	}
	if text != "hello" {
		t.Fatalf("RunText text = %q, want %q", text, "hello")
	}
	if result.FinishReason != "stop" || result.Iterations != 1 {
		t.Fatalf("result = %+v, want one stop round", result)
	}

	request := provider.request(0)
	if request.Model != "test-model" {
		t.Fatalf("request model = %q", request.Model)
	}
	if len(request.Tools) == 0 {
		t.Fatal("default assembly sent no tool specs")
	}
	names := toolNames(request.Tools)
	for _, want := range []string{"read_file", "list_dir", "glob", "grep", "bash"} {
		if !names[want] {
			t.Fatalf("default tool set is missing %q", want)
		}
	}

	wantEvents := []kit.EventType{
		kit.EventTurnStart,
		kit.EventTextDelta,
		kit.EventMessage,
		kit.EventTurnEnd,
	}
	got := eventTypes(events)
	if len(got) != len(wantEvents) {
		t.Fatalf("event types = %v, want %v", got, wantEvents)
	}
	for i := range wantEvents {
		if got[i] != wantEvents[i] {
			t.Fatalf("event types = %v, want %v", got, wantEvents)
		}
	}
}

func TestRunTextAggregatesDeltas(t *testing.T) {
	provider := &scriptProvider{scripts: [][]kit.Chunk{
		textScript("Hel", "lo", " world"),
		textScript("again"),
	}}
	a := mustNew(t, kit.Config{Model: "m", Provider: provider, Tools: []kit.Tool{}})

	var emitted strings.Builder
	text, result, err := a.RunText(context.Background(), "s1", "hi", func(event kit.Event) {
		if event.Type == kit.EventTextDelta {
			emitted.WriteString(event.Text)
		}
	})
	if err != nil {
		t.Fatalf("RunText: %v", err)
	}
	if text != "Hello world" {
		t.Fatalf("RunText text = %q, want %q", text, "Hello world")
	}
	if emitted.String() != text {
		t.Fatalf("emitted deltas = %q, returned text = %q", emitted.String(), text)
	}
	if result.Messages[len(result.Messages)-1].Text() != "Hello world" {
		t.Fatalf("assistant message = %q", result.Messages[len(result.Messages)-1].Text())
	}

	request := provider.request(0)
	if request.Model != "m" {
		t.Fatalf("request model = %q, want m", request.Model)
	}
	if len(request.Tools) != 0 {
		t.Fatalf("explicit empty Tools override sent %d tool specs", len(request.Tools))
	}
	if len(request.Messages) != 1 || request.Messages[0].Role != kit.RoleUser || request.Messages[0].Text() != "hi" {
		t.Fatalf("request messages = %+v", request.Messages)
	}

	// emit may be nil.
	text, _, err = a.RunText(context.Background(), "s2", "again", nil)
	if err != nil {
		t.Fatalf("RunText with nil emit: %v", err)
	}
	if text != "again" {
		t.Fatalf("RunText(nil emit) text = %q, want again", text)
	}
}

func TestDefaultHistoryPersistsAcrossRuns(t *testing.T) {
	provider := &scriptProvider{scripts: [][]kit.Chunk{
		textScript("one"),
		textScript("two"),
		textScript("three"),
	}}
	a := mustNew(t, kit.Config{Model: "m", Provider: provider, Tools: []kit.Tool{}})
	ctx := context.Background()

	if _, _, err := a.RunText(ctx, "chat", "first", nil); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if _, _, err := a.RunText(ctx, "chat", "second", nil); err != nil {
		t.Fatalf("second run: %v", err)
	}

	second := provider.request(1)
	if len(second.Messages) != 3 {
		t.Fatalf("second request has %d messages, want history+input (3)", len(second.Messages))
	}
	if second.Messages[0].Text() != "first" || second.Messages[1].Text() != "one" || second.Messages[2].Text() != "second" {
		t.Fatalf("second request messages = %+v", second.Messages)
	}
	if second.Messages[0].Role != kit.RoleUser || second.Messages[1].Role != kit.RoleAssistant {
		t.Fatalf("second request roles = %+v, %+v", second.Messages[0].Role, second.Messages[1].Role)
	}

	// A different session starts empty.
	if _, _, err := a.RunText(ctx, "other", "third", nil); err != nil {
		t.Fatalf("third run: %v", err)
	}
	third := provider.request(2)
	if len(third.Messages) != 1 || third.Messages[0].Text() != "third" {
		t.Fatalf("other-session messages = %+v", third.Messages)
	}
}

// ---------------------------------------------------------------------------
// default gate
// ---------------------------------------------------------------------------

// TestDefaultGateAllowsReadFileDeniesBash exercises the safe default gate
// through a real run: read_file is approved and executed, bash is denied before
// it can run and the denial is reported to the model as a tool error.
func TestDefaultGateAllowsReadFileDeniesBash(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("kit-default\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	provider := &scriptProvider{scripts: [][]kit.Chunk{
		toolCallScript(
			kit.ToolCall{ID: "call_read", Name: "read_file", Arguments: `{"path":"hello.txt"}`},
			kit.ToolCall{ID: "call_bash", Name: "bash", Arguments: `{"command":"echo should-not-run"}`},
		),
		textScript("done"),
	}}
	a := mustNew(t, kit.Config{Model: "m", Provider: provider, Root: root})

	text, result, err := a.RunText(context.Background(), "s1", "read hello.txt", nil)
	if err != nil {
		t.Fatalf("RunText: %v", err)
	}
	if text != "done" {
		t.Fatalf("RunText text = %q, want done", text)
	}
	if provider.callCount() != 2 {
		t.Fatalf("provider calls = %d, want 2 (tool round + answer)", provider.callCount())
	}

	second := provider.request(1).Messages
	readMessage, ok := findToolMessage(second, "read_file")
	if !ok {
		t.Fatalf("no read_file tool message in %+v", second)
	}
	if !strings.Contains(readMessage.Text(), "kit-default") || readMessage.ToolCallID != "call_read" {
		t.Fatalf("read_file result = %+v, want the file content", readMessage)
	}
	bashMessage, ok := findToolMessage(second, "bash")
	if !ok {
		t.Fatalf("no bash tool message in %+v", second)
	}
	if !strings.Contains(bashMessage.Text(), "denied by approval gate") {
		t.Fatalf("bash result = %q, want an approval-gate denial", bashMessage.Text())
	}
	if got := result.Messages[len(result.Messages)-1].Text(); got != "done" {
		t.Fatalf("final assistant message = %q, want done", got)
	}
}

// ---------------------------------------------------------------------------
// overrides
// ---------------------------------------------------------------------------

func TestOverridesHonored(t *testing.T) {
	t.Run("tools gate and history", func(t *testing.T) {
		echo := &fakeTool{name: "echo", fn: func(ctx context.Context, call kit.ToolCall) (kit.ToolResult, error) {
			return kit.ToolResult{Content: "echo:" + call.Arguments}, nil
		}}
		approvalGate := &recordingGate{}
		history := newRecordingHistory()
		provider := &scriptProvider{scripts: [][]kit.Chunk{
			toolCallScript(kit.ToolCall{ID: "c1", Name: "echo", Arguments: `{"text":"hi"}`}),
			textScript("done"),
		}}
		a := mustNew(t, kit.Config{
			Model:    "m",
			Provider: provider,
			Tools:    []kit.Tool{echo},
			Gate:     approvalGate,
			History:  history,
			Root:     t.TempDir(),
		})

		text, _, err := a.RunText(context.Background(), "s1", "go", nil)
		if err != nil {
			t.Fatalf("RunText: %v", err)
		}
		if text != "done" {
			t.Fatalf("RunText text = %q, want done", text)
		}
		if echo.callCount() != 1 {
			t.Fatalf("echo calls = %d, want 1", echo.callCount())
		}
		if got := approvalGate.approved(); len(got) != 1 || got[0] != "echo" {
			t.Fatalf("gate calls = %v, want [echo]", got)
		}
		if history.loadCount() == 0 {
			t.Fatal("override history was never loaded")
		}
		if got := history.messages("s1"); len(got) != 4 {
			t.Fatalf("history messages = %d, want 4 (input, assistant tool call, tool result, answer)", len(got))
		}

		request := provider.request(0)
		if len(request.Tools) != 1 || request.Tools[0].Name != "echo" {
			t.Fatalf("request tools = %+v, want only the override", request.Tools)
		}
	})

	t.Run("gate override denies", func(t *testing.T) {
		echo := &fakeTool{name: "echo"}
		denyGate := &recordingGate{decision: kit.DecisionDeny}
		provider := &scriptProvider{scripts: [][]kit.Chunk{
			toolCallScript(kit.ToolCall{ID: "c1", Name: "echo", Arguments: `{}`}),
			textScript("after denial"),
		}}
		a := mustNew(t, kit.Config{
			Model:    "m",
			Provider: provider,
			Tools:    []kit.Tool{echo},
			Gate:     denyGate,
			Root:     t.TempDir(),
		})

		text, _, err := a.RunText(context.Background(), "s1", "go", nil)
		if err != nil {
			t.Fatalf("RunText: %v", err)
		}
		if text != "after denial" {
			t.Fatalf("RunText text = %q, want after denial", text)
		}
		if echo.callCount() != 0 {
			t.Fatalf("denied tool was invoked %d times", echo.callCount())
		}
		message, ok := findToolMessage(provider.request(1).Messages, "echo")
		if !ok || !strings.Contains(message.Text(), "denied by approval gate") {
			t.Fatalf("denied tool message = %+v", message)
		}
	})
}
