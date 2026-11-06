package agent_test

// Tests for agent.NewRunner / Subagent. A scripted provider stands in for the
// model: each Stream call replays a fixed chunk script, so multi-run subagent
// behavior (first run, Send, Stop) is deterministic and offline. The fakes are
// the shared ones from agent_test.go.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/agent"
)

// concurrencyProbe records the maximum number of simultaneously active streams.
type concurrencyProbe struct {
	active atomic.Int32
	max    atomic.Int32
}

func (p *concurrencyProbe) begin() {
	current := p.active.Add(1)
	for {
		peak := p.max.Load()
		if current <= peak || p.max.CompareAndSwap(peak, current) {
			return
		}
	}
}

func (p *concurrencyProbe) done() { p.active.Add(-1) }

// probeStream counts one active provider stream until it is drained.
type probeStream struct {
	inner rig.Stream
	probe *concurrencyProbe
	once  sync.Once
}

func (s *probeStream) Recv() (rig.Chunk, error) {
	chunk, err := s.inner.Recv()
	if err != nil {
		s.once.Do(s.probe.done)
	}
	return chunk, err
}

func specNames(specs []rig.ToolSpec) []string {
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, spec.Name)
	}
	return names
}

func newTestRunner(t *testing.T, provider rig.Provider, opts agent.RunnerOptions, tools ...rig.Tool) rig.SubagentRunner {
	t.Helper()
	if opts.MaxDepth == 0 && opts.MaxConcurrent == 0 {
		opts = agent.DefaultRunnerOptions()
	}
	parent := rig.AgentSpec{
		Model:        "parent-model",
		SystemPrompt: "parent system prompt",
		Tools:        tools,
		Provider:     provider,
	}
	runner, err := agent.NewRunner(parent, opts)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return runner
}

func waitSubagent(t *testing.T, sub rig.Subagent) rig.SubagentResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := sub.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	return result
}

func usageScript(text string, usage *rig.Usage) []rig.Chunk {
	return []rig.Chunk{
		{Delta: text},
		{Usage: usage, FinishReason: "stop"},
	}
}

func TestRunnerStartWaitOutputUsageAndToolFiltering(t *testing.T) {
	usage := &rig.Usage{PromptTokens: 3, CompletionTokens: 4, TotalTokens: 7}
	provider := &scriptProvider{scripts: [][]rig.Chunk{usageScript("hello child", usage)}}
	runner := newTestRunner(t, provider,
		agent.RunnerOptions{MaxDepth: 1, MaxConcurrent: 2, SystemPromptAppend: "you are a focused subagent"},
		&fakeTool{name: "read_file"}, &fakeTool{name: "subagent"}, &fakeTool{name: "workflow"}, &fakeTool{name: "team"},
	)

	sub, err := runner.Start(context.Background(), rig.SubagentSpec{Name: "researcher", Prompt: "do the research"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sub.Stop() }()
	if got := sub.Name(); got != "researcher" {
		t.Fatalf("Name = %q, want researcher", got)
	}
	result := waitSubagent(t, sub)
	if result.Name != "researcher" || result.Output != "hello child" {
		t.Fatalf("result = %+v, want name+output", result)
	}
	if result.Usage != (rig.Usage{PromptTokens: 3, CompletionTokens: 4, TotalTokens: 7}) {
		t.Fatalf("usage = %+v, want the model usage", result.Usage)
	}
	if result.Iterations != 1 {
		t.Fatalf("iterations = %d, want 1", result.Iterations)
	}

	request := provider.request(0)
	if request.Model != "parent-model" {
		t.Fatalf("child model = %q, want the parent model", request.Model)
	}
	if got := specNames(request.Tools); len(got) != 1 || got[0] != "read_file" {
		t.Fatalf("child tools = %v, want [read_file] (orchestration tools filtered)", got)
	}
	if len(request.Messages) == 0 || request.Messages[0].Role != rig.RoleSystem {
		t.Fatalf("request messages = %+v, want a system prompt first", request.Messages)
	}
	systemPrompt := request.Messages[0].Text()
	if !strings.Contains(systemPrompt, "parent system prompt") || !strings.Contains(systemPrompt, "focused subagent") {
		t.Fatalf("system prompt = %q, want parent prompt plus append", systemPrompt)
	}
	last := request.Messages[len(request.Messages)-1]
	if last.Role != rig.RoleUser || last.Text() != "do the research" {
		t.Fatalf("first input = %+v, want the subagent prompt", last)
	}
}

func TestRunnerChildOverrides(t *testing.T) {
	provider := &scriptProvider{scripts: [][]rig.Chunk{toolCallScript("call-1", "read_file")}}
	runner := newTestRunner(t, provider, agent.RunnerOptions{MaxDepth: 1, MaxConcurrent: 1, ExcludedTools: []string{}})

	sub, err := runner.Start(context.Background(), rig.SubagentSpec{
		Name:          "overridden",
		Prompt:        "call a tool",
		Model:         "child-model",
		SystemPrompt:  "child system prompt",
		MaxIterations: 1,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sub.Stop() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = sub.Wait(ctx)
	if !errors.Is(err, rig.ErrMaxIterations) {
		t.Fatalf("Wait error = %v, want ErrMaxIterations (MaxIterations override)", err)
	}
	request := provider.request(0)
	if request.Model != "child-model" {
		t.Fatalf("model = %q, want child-model", request.Model)
	}
	if len(request.Messages) == 0 || !strings.Contains(request.Messages[0].Text(), "child system prompt") {
		t.Fatalf("system prompt = %+v, want the spec override", request.Messages)
	}
}

func TestRunnerSendAfterWait(t *testing.T) {
	provider := &scriptProvider{scripts: [][]rig.Chunk{
		textScript("first answer"),
		textScript("second answer"),
	}}
	runner := newTestRunner(t, provider, agent.RunnerOptions{MaxDepth: 1, MaxConcurrent: 1})
	sub, err := runner.Start(context.Background(), rig.SubagentSpec{Prompt: "first prompt"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sub.Stop() }()

	first := waitSubagent(t, sub)
	if first.Output != "first answer" {
		t.Fatalf("first output = %q", first.Output)
	}
	// Wait is stable after the run finished.
	again := waitSubagent(t, sub)
	if again.Output != "first answer" {
		t.Fatalf("second Wait output = %q, want the same first result", again.Output)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	second, err := sub.Send(ctx, "second prompt")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if second.Output != "second answer" {
		t.Fatalf("send output = %q", second.Output)
	}
	if provider.callCount() != 2 {
		t.Fatalf("provider calls = %d, want 2", provider.callCount())
	}
	secondRequest := provider.request(1)
	last := secondRequest.Messages[len(secondRequest.Messages)-1]
	if last.Role != rig.RoleUser || last.Text() != "second prompt" {
		t.Fatalf("send input = %+v, want the follow-up message", last)
	}
}

func TestRunnerStopCancelsInFlightRun(t *testing.T) {
	started := make(chan struct{}, 1)
	provider := &scriptProvider{fn: func(ctx context.Context, _ rig.Request) (rig.Stream, error) {
		started <- struct{}{}
		return &blockingStream{ctx: ctx}, nil
	}}
	runner := newTestRunner(t, provider, agent.RunnerOptions{MaxDepth: 1, MaxConcurrent: 1})
	sub, err := runner.Start(context.Background(), rig.SubagentSpec{Prompt: "block forever"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("provider was never called")
	}
	if err := sub.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := sub.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := sub.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait after Stop = %v, want context.Canceled", err)
	}
	if _, err := sub.Send(context.Background(), "after stop"); err == nil ||
		!strings.Contains(err.Error(), "stopped") {
		t.Fatalf("Send after Stop = %v, want a stopped error", err)
	}
}

func TestRunnerConcurrencyLimit(t *testing.T) {
	provider := &scriptProvider{repeatLast: true, scripts: [][]rig.Chunk{textScript("ok")}}
	runner := newTestRunner(t, provider, agent.RunnerOptions{MaxDepth: 1, MaxConcurrent: 1})

	first, err := runner.Start(context.Background(), rig.SubagentSpec{Prompt: "one"})
	if err != nil {
		t.Fatalf("first Start: %v", err)
	}
	waitSubagent(t, first) // finished, but not stopped: the slot is still held

	// A second live subagent cannot start while the first holds the only slot.
	blockedCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := runner.Start(blockedCtx, rig.SubagentSpec{Prompt: "two"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second Start error = %v, want the concurrency slot to block", err)
	}

	// Stopping the first releases the slot.
	if err := first.Stop(); err != nil {
		t.Fatalf("Stop first: %v", err)
	}
	second, err := runner.Start(context.Background(), rig.SubagentSpec{Prompt: "two"})
	if err != nil {
		t.Fatalf("Start after Stop: %v", err)
	}
	defer func() { _ = second.Stop() }()
	waitSubagent(t, second)
}

func TestRunnerDepthLimit(t *testing.T) {
	provider := &scriptProvider{repeatLast: true, scripts: [][]rig.Chunk{textScript("ok")}}
	parent := rig.AgentSpec{Model: "m", Provider: provider}

	// Negative MaxDepth disables subagents explicitly.
	disabled, err := agent.NewRunner(parent, agent.RunnerOptions{MaxDepth: -1})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	if _, err := disabled.Start(context.Background(), rig.SubagentSpec{Prompt: "x"}); err == nil ||
		!strings.Contains(err.Error(), "depth limit") {
		t.Fatalf("disabled Start error = %v, want a depth limit error", err)
	}

	// The zero value means the default depth (one level works).
	defaults, err := agent.NewRunner(parent, agent.RunnerOptions{})
	if err != nil {
		t.Fatalf("NewRunner defaults: %v", err)
	}
	sub, err := defaults.Start(context.Background(), rig.SubagentSpec{Prompt: "x"})
	if err != nil {
		t.Fatalf("default-depth Start: %v", err)
	}
	_ = sub.Stop()
}

func TestRunnerSerializesConcurrentSends(t *testing.T) {
	probe := &concurrencyProbe{}
	provider := &scriptProvider{fn: func(context.Context, rig.Request) (rig.Stream, error) {
		probe.begin()
		return &probeStream{inner: &chunkStream{chunks: textScript("send answer")}, probe: probe}, nil
	}}
	runner := newTestRunner(t, provider, agent.RunnerOptions{MaxDepth: 1, MaxConcurrent: 1})
	sub, err := runner.Start(context.Background(), rig.SubagentSpec{Prompt: "first"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sub.Stop() }()
	waitSubagent(t, sub)

	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := sub.Send(context.Background(), "follow up")
			if err != nil || result.Output != "send answer" {
				t.Errorf("concurrent Send: output=%q err=%v", result.Output, err)
			}
		}()
	}
	wg.Wait()
	if peak := probe.max.Load(); peak != 1 {
		t.Fatalf("peak concurrent provider streams = %d, want 1 (runs are serialized)", peak)
	}
}

func TestRunnerValidation(t *testing.T) {
	provider := &scriptProvider{repeatLast: true, scripts: [][]rig.Chunk{textScript("ok")}}
	parent := rig.AgentSpec{Model: "m", Provider: provider}

	runner, err := agent.NewRunner(parent, agent.RunnerOptions{MaxDepth: 1})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	if _, err := runner.Start(context.Background(), rig.SubagentSpec{Prompt: "   "}); err == nil ||
		!strings.Contains(err.Error(), "Prompt") {
		t.Fatalf("empty prompt error = %v", err)
	}
	if _, err := runner.Start(context.Background(), rig.SubagentSpec{Prompt: "x", MaxIterations: -1}); err == nil {
		t.Fatal("negative MaxIterations accepted")
	}
	if _, err := agent.NewRunner(rig.AgentSpec{}, agent.RunnerOptions{}); !errors.Is(err, rig.ErrProviderRequired) {
		t.Fatalf("nil provider error = %v, want ErrProviderRequired", err)
	}
	if _, err := agent.NewRunner(rig.AgentSpec{Provider: provider}, agent.RunnerOptions{}); !errors.Is(err, rig.ErrModelRequired) {
		t.Fatalf("empty model error = %v, want ErrModelRequired", err)
	}
	if _, err := agent.NewRunner(parent, agent.RunnerOptions{MaxConcurrent: -1}); err == nil {
		t.Fatal("negative MaxConcurrent accepted")
	}

	// A missing name gets a generated stable prefix.
	sub, err := runner.Start(context.Background(), rig.SubagentSpec{Prompt: "x"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sub.Stop() }()
	if !strings.HasPrefix(sub.Name(), "subagent-") {
		t.Fatalf("generated name = %q, want subagent-N", sub.Name())
	}
}
