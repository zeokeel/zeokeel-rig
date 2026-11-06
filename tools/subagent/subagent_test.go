package subagent_test

// Tests for the subagent tool: argument mapping, output/error semantics, Stop
// release, and one end-to-end run through the real agent.NewRunner stack with a
// scripted provider.

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
	"github.com/zeokeel/zeokeel-rig/tools/subagent"
)

// fakeSubagent is a scripted v1.Subagent.
type fakeSubagent struct {
	name       string
	output     string
	structured map[string]any
	waitErr    error
	waitFn     func(ctx context.Context) (v1.SubagentResult, error)
	stopCalls  atomic.Int32
}

func (s *fakeSubagent) Name() string { return s.name }

func (s *fakeSubagent) Wait(ctx context.Context) (v1.SubagentResult, error) {
	if s.waitFn != nil {
		return s.waitFn(ctx)
	}
	if s.waitErr != nil {
		return v1.SubagentResult{Name: s.name}, s.waitErr
	}
	return v1.SubagentResult{Name: s.name, Output: s.output, Structured: s.structured}, nil
}

func (s *fakeSubagent) Send(ctx context.Context, _ string) (v1.SubagentResult, error) {
	return s.Wait(ctx)
}

func (s *fakeSubagent) Stop() error {
	s.stopCalls.Add(1)
	return nil
}

// fakeRunner records specs and returns one scripted subagent.
type fakeRunner struct {
	mu       sync.Mutex
	specs    []v1.SubagentSpec
	startErr error
	sub      *fakeSubagent
}

func (r *fakeRunner) Start(_ context.Context, spec v1.SubagentSpec) (v1.Subagent, error) {
	r.mu.Lock()
	r.specs = append(r.specs, spec)
	startErr := r.startErr
	sub := r.sub
	if sub == nil {
		sub = &fakeSubagent{name: spec.Name, output: "child output"}
		r.sub = sub
	}
	r.mu.Unlock()
	if startErr != nil {
		return nil, startErr
	}
	return sub, nil
}

func (r *fakeRunner) recorded() []v1.SubagentSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]v1.SubagentSpec(nil), r.specs...)
}

func invokeTool(t *testing.T, tool v1.Tool, arguments string) v1.ToolResult {
	t.Helper()
	result, err := tool.Invoke(context.Background(), v1.ToolCall{ID: "call-1", Name: tool.Name(), Arguments: arguments})
	if err != nil {
		t.Fatalf("%s.Invoke returned Go error: %v", tool.Name(), err)
	}
	return result
}

func TestInvokeMapsArgumentsAndReturnsOutput(t *testing.T) {
	runner := &fakeRunner{}
	tool := subagent.New(runner)
	if tool.Name() != subagent.ToolName {
		t.Fatalf("Name = %q, want %q", tool.Name(), subagent.ToolName)
	}
	result := invokeTool(t, tool, `{"prompt":"do the work","name":"researcher","model":"m2","max_iterations":3}`)
	if result.IsError || result.Content != "child output" {
		t.Fatalf("result = %q (IsError=%v), want child output", result.Content, result.IsError)
	}
	specs := runner.recorded()
	if len(specs) != 1 {
		t.Fatalf("runner.Start calls = %d, want 1", len(specs))
	}
	spec := specs[0]
	if spec.Prompt != "do the work" || spec.Name != "researcher" || spec.Model != "m2" || spec.MaxIterations != 3 {
		t.Fatalf("spec = %+v, want the tool arguments", spec)
	}
	if got := runner.sub.stopCalls.Load(); got != 1 {
		t.Fatalf("Stop calls = %d, want 1 (the tool must release the slot)", got)
	}
}

func TestMissingPromptAndValidation(t *testing.T) {
	tool := subagent.New(&fakeRunner{})
	if result := invokeTool(t, tool, `{}`); !result.IsError || !strings.Contains(result.Content, "prompt") {
		t.Fatalf("missing prompt = %q (IsError=%v)", result.Content, result.IsError)
	}
	if result := invokeTool(t, tool, `{"prompt":"x","max_iterations":-1}`); !result.IsError {
		t.Fatal("negative max_iterations accepted")
	}
}

func TestNilRunnerIsConfigurationError(t *testing.T) {
	_, err := subagent.New(nil).Invoke(context.Background(), v1.ToolCall{Arguments: `{"prompt":"x"}`})
	if err == nil || !strings.Contains(err.Error(), "SubagentRunner") {
		t.Fatalf("nil runner error = %v, want a configuration error", err)
	}
}

func TestStartAndWaitErrorsPassThrough(t *testing.T) {
	startFailure := &fakeRunner{startErr: errors.New("depth limit reached")}
	if _, err := subagent.New(startFailure).Invoke(context.Background(), v1.ToolCall{
		Arguments: `{"prompt":"x"}`,
	}); err == nil || !strings.Contains(err.Error(), "depth limit reached") {
		t.Fatalf("start error = %v, want passthrough", err)
	}

	sub := &fakeSubagent{name: "s", waitErr: errors.New("model exploded")}
	runner := &fakeRunner{sub: sub}
	if _, err := subagent.New(runner).Invoke(context.Background(), v1.ToolCall{
		Arguments: `{"prompt":"x"}`,
	}); err == nil || !strings.Contains(err.Error(), "model exploded") {
		t.Fatalf("wait error = %v, want passthrough", err)
	}
	if got := sub.stopCalls.Load(); got != 1 {
		t.Fatalf("Stop calls after wait error = %d, want 1", got)
	}
}

func TestWaitCancellationPassesThrough(t *testing.T) {
	sub := &fakeSubagent{name: "s", waitFn: func(ctx context.Context) (v1.SubagentResult, error) {
		<-ctx.Done()
		return v1.SubagentResult{}, ctx.Err()
	}}
	runner := &fakeRunner{sub: sub}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := subagent.New(runner).Invoke(ctx, v1.ToolCall{Arguments: `{"prompt":"x"}`})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait error = %v, want context.Canceled", err)
	}
}

func TestSchemaAndEmptyOutput(t *testing.T) {
	runner := &fakeRunner{sub: &fakeSubagent{name: "s"}} // no output
	tool := subagent.New(runner)
	if tool.Description() == "" {
		t.Fatal("subagent has an empty description")
	}
	schema := tool.Schema()
	required, _ := schema["required"].([]string)
	if len(required) != 1 || required[0] != "prompt" {
		t.Fatalf("schema required = %v, want [prompt]", required)
	}
	result := invokeTool(t, tool, `{"prompt":"x"}`)
	if result.IsError || !strings.Contains(result.Content, "without text output") {
		t.Fatalf("empty output result = %q (IsError=%v)", result.Content, result.IsError)
	}
}

// --- end-to-end: real runner + scripted provider ---------------------------

type staticProvider struct{ text string }

func (p staticProvider) Name() string { return "static" }

func (p staticProvider) Stream(context.Context, v1.Request) (v1.Stream, error) {
	return &staticStream{text: p.text}, nil
}

type staticStream struct {
	text string
	sent bool
}

func (s *staticStream) Recv() (v1.Chunk, error) {
	if s.sent {
		return v1.Chunk{}, io.EOF
	}
	s.sent = true
	return v1.Chunk{Delta: s.text, FinishReason: "stop"}, nil
}

func TestRealRunnerEndToEnd(t *testing.T) {
	provider := staticProvider{text: "real child answer"}
	runner, err := agent.NewRunner(v1.AgentSpec{Model: "parent-model", Provider: provider}, agent.DefaultRunnerOptions())
	if err != nil {
		t.Fatalf("agent.NewRunner: %v", err)
	}
	tool := subagent.New(runner)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := tool.Invoke(ctx, v1.ToolCall{Arguments: `{"prompt":"solve this","name":"child"}`})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.IsError || result.Content != "real child answer" {
		t.Fatalf("result = %q (IsError=%v), want the child's final text", result.Content, result.IsError)
	}
}
