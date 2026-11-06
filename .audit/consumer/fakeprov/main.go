// Command fakeprov is the single most important integration test: a third
// party implements v1.Provider, v1.Tool and v1.Gate from scratch, configures an
// agent with session.NewMemory and runs a complete model<->tool loop fully
// offline (no credentials, no network). It also verifies the defaults an SDK
// consumer relies on: nil Gate = allow all, MaxIterations 0 = 20, errors.Is on
// the v1 sentinels, and History persistence.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/zeokeel/zeokeel-rig/agent"
	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
	"github.com/zeokeel/zeokeel-rig/gate"
	"github.com/zeokeel/zeokeel-rig/session"
)

// --- custom Provider: OpenAI-style stream, served from a script ------------

type fakeProvider struct {
	mu    sync.Mutex
	round int
}

func (p *fakeProvider) Name() string { return "fake" }

func (p *fakeProvider) Stream(_ context.Context, req v1.Request) (v1.Stream, error) {
	if req.Model != "fake-model" {
		return nil, fmt.Errorf("fake: unexpected model %q", req.Model)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.round++
	switch p.round {
	case 1:
		if len(req.Tools) == 0 {
			return nil, errors.New("fake: agent sent no tools")
		}
		return &fakeStream{chunks: []v1.Chunk{
			{ToolCalls: []v1.ToolCallDelta{{Index: 0, ID: "call_1", Name: "echo"}}},
			{ToolCalls: []v1.ToolCallDelta{{Index: 0, Arguments: `{"text":"hi`}}},
			{ToolCalls: []v1.ToolCallDelta{{Index: 0, Arguments: ` there"}`}}},
			{FinishReason: "tool_calls"},
		}}, nil
	case 2:
		return &fakeStream{chunks: []v1.Chunk{
			{Delta: "Echo says: "},
			{Delta: "hi there"},
			{Usage: &v1.Usage{PromptTokens: 11, CompletionTokens: 3, TotalTokens: 14}},
			{FinishReason: "stop"},
		}}, nil
	default:
		return nil, fmt.Errorf("fake: unexpected round %d", p.round)
	}
}

type fakeStream struct{ chunks []v1.Chunk }

func (s *fakeStream) Recv() (v1.Chunk, error) {
	if len(s.chunks) == 0 {
		return v1.Chunk{}, io.EOF
	}
	c := s.chunks[0]
	s.chunks = s.chunks[1:]
	return c, nil
}

// --- custom Tool ----------------------------------------------------------

type echoTool struct{ calls int }

func (t *echoTool) Name() string        { return "echo" }
func (t *echoTool) Description() string { return "Echo the text back." }
func (t *echoTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"text": map[string]any{"type": "string"},
	}, "required": []any{"text"}}
}
func (t *echoTool) Invoke(_ context.Context, call v1.ToolCall) (v1.ToolResult, error) {
	t.calls++
	return v1.ToolResult{Content: "echoed " + call.Arguments}, nil
}

// --- custom Gate ----------------------------------------------------------

type recordingGate struct{ calls int }

func (g *recordingGate) Approve(_ context.Context, tool v1.Tool, call v1.ToolCall) (v1.Decision, error) {
	g.calls++
	if call.Name != "echo" {
		return v1.DecisionDeny, nil
	}
	return v1.DecisionAllow, nil
}

func main() {
	ctx := context.Background()

	// 1. Fake provider removes any credential/network requirement.
	prov := &fakeProvider{}
	tool := &echoTool{}
	g := &recordingGate{}
	hist := session.NewMemory()

	// 2. Assemble an agent with a hand-written spec: no facade, but the
	//    AgentSpec type carries everything an agent needs.
	a, err := agent.New(v1.AgentSpec{
		Provider:     prov,
		Model:        "fake-model",
		SystemPrompt: "be brief",
		Tools:        []v1.Tool{tool},
		Gate:         g,
		History:      hist,
	})
	must(err)

	var events []v1.EventType
	res, err := a.Run(ctx, v1.RunRequest{
		SessionID: "sess-1",
		Input:     []v1.Message{v1.NewMessage(v1.RoleUser, "say hi")},
		Emit:      func(ev v1.Event) { events = append(events, ev.Type) },
	})
	must(err)

	// 3. A consumer can assert the whole loop happened.
	check("tool invoked once", tool.calls == 1)
	check("gate consulted once", g.calls == 1)
	check("2 model rounds", res.Iterations == 2)
	check("finish reason stop", res.FinishReason == "stop")
	check("usage summed", res.Usage.TotalTokens == 14)
	check("4 messages appended", len(res.Messages) == 4)
	check("tool_result event emitted", has(events, v1.EventToolResult))
	check("text_delta event emitted", has(events, v1.EventTextDelta))

	stored, err := hist.Load(ctx, "sess-1")
	must(err)
	check("history persisted 4 messages", len(stored) == 4)
	check("system prompt not persisted", stored[0].Role != v1.RoleSystem)

	// 4. Defaults: nil Gate allows all and nil History disables persistence
	//    (no SessionID needed). MaxIterations 0 = 20 via DefaultMaxIterations.
	tool2 := &echoTool{}
	a2, err := agent.New(v1.AgentSpec{Provider: &fakeProvider{}, Model: "fake-model", Tools: []v1.Tool{tool2}})
	must(err)
	res2, err := a2.Run(ctx, v1.RunRequest{Input: []v1.Message{v1.NewMessage(v1.RoleUser, "hi")}})
	must(err)
	check("nil Gate allows the tool", tool2.calls == 1)
	check("nil History still returns messages", len(res2.Messages) == 4)
	check("DefaultMaxIterations constant is 20", agent.DefaultMaxIterations == 20)

	// 5. Error taxonomy: errors.Is works on the v1 sentinels.
	_, err = agent.New(v1.AgentSpec{Model: "m"})
	check("ErrProviderRequired via errors.Is", errors.Is(err, v1.ErrProviderRequired))
	_, err = agent.New(v1.AgentSpec{Provider: &fakeProvider{}})
	check("ErrModelRequired via errors.Is", errors.Is(err, v1.ErrModelRequired))

	// 6. Built-in gates are ordinary values, no registration needed.
	a3, err := agent.New(v1.AgentSpec{Provider: &fakeProvider{}, Model: "fake-model",
		Tools: []v1.Tool{&echoTool{}}, Gate: gate.DenyAll()})
	must(err)
	res3, err := a3.Run(ctx, v1.RunRequest{Input: []v1.Message{v1.NewMessage(v1.RoleUser, "hi")}})
	must(err)
	check("DenyAll feeds a denial back to the model", res3.Iterations == 2)

	fmt.Println("FAKEPROV_OK: offline full agent.Run integration passed")
}

func has(events []v1.EventType, want v1.EventType) bool {
	for _, e := range events {
		if e == want {
			return true
		}
	}
	return false
}

func check(name string, ok bool) {
	if ok {
		fmt.Println("ok  ", name)
		return
	}
	fmt.Println("FAIL", name)
	os.Exit(1)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}
