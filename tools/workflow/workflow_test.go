package workflow_test

// Tests for the workflow tool: sequential steps, {{prev}}/{{steps.name}}
// templates, parallel groups, and failure propagation. A fake runner scripts the
// step outputs; the end-to-end test uses agent.NewRunner with a static provider.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/agent"
	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
	"github.com/zeokeel/zeokeel-rig/tools/workflow"
)

// fakeRunner starts scripted subagents and tracks their concurrency.
type fakeRunner struct {
	mu           sync.Mutex
	specs        []v1.SubagentSpec
	outputs      map[string]string
	defaultOut   string
	failName     string
	startErrName string
	delay        time.Duration

	active atomic.Int32
	peak   atomic.Int32
}

func (r *fakeRunner) Start(_ context.Context, spec v1.SubagentSpec) (v1.Subagent, error) {
	r.mu.Lock()
	r.specs = append(r.specs, spec)
	failStart := r.startErrName == spec.Name
	r.mu.Unlock()
	if failStart {
		return nil, fmt.Errorf("cannot start %s", spec.Name)
	}
	current := r.active.Add(1)
	for {
		peak := r.peak.Load()
		if current <= peak || r.peak.CompareAndSwap(peak, current) {
			break
		}
	}
	return &fakeSubagent{runner: r, name: spec.Name}, nil
}

func (r *fakeRunner) recordedSpecs() []v1.SubagentSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]v1.SubagentSpec(nil), r.specs...)
}

func (r *fakeRunner) outputFor(name string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failName == name {
		return "", fmt.Errorf("step exploded: %s", name)
	}
	if output, ok := r.outputs[name]; ok {
		return output, nil
	}
	return r.defaultOut, nil
}

// fakeSubagent is one scripted child.
type fakeSubagent struct {
	runner *fakeRunner
	name   string
}

func (s *fakeSubagent) Name() string { return s.name }

func (s *fakeSubagent) Wait(ctx context.Context) (v1.SubagentResult, error) {
	if s.runner.delay > 0 {
		select {
		case <-time.After(s.runner.delay):
		case <-ctx.Done():
			return v1.SubagentResult{Name: s.name}, ctx.Err()
		}
	}
	output, err := s.runner.outputFor(s.name)
	if err != nil {
		return v1.SubagentResult{Name: s.name}, err
	}
	return v1.SubagentResult{Name: s.name, Output: output}, nil
}

func (s *fakeSubagent) Send(ctx context.Context, _ string) (v1.SubagentResult, error) {
	return s.Wait(ctx)
}

func (s *fakeSubagent) Stop() error {
	s.runner.active.Add(-1)
	return nil
}

func invokeWorkflow(t *testing.T, tool v1.Tool, body map[string]any) v1.ToolResult {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal arguments: %v", err)
	}
	result, err := tool.Invoke(context.Background(), v1.ToolCall{ID: "call-1", Name: tool.Name(), Arguments: string(payload)})
	if err != nil {
		t.Fatalf("workflow.Invoke returned Go error: %v", err)
	}
	return result
}

func requireContains(t *testing.T, text string, fragments ...string) {
	t.Helper()
	for _, fragment := range fragments {
		if !strings.Contains(text, fragment) {
			t.Fatalf("text does not contain %q:\n%s", fragment, text)
		}
	}
}

func TestSequentialStepsAndTemplates(t *testing.T) {
	runner := &fakeRunner{outputs: map[string]string{"a": "out-A", "b": "out-B"}}
	tool := workflow.New(runner)
	if tool.Name() != workflow.ToolName {
		t.Fatalf("Name = %q, want %q", tool.Name(), workflow.ToolName)
	}
	result := invokeWorkflow(t, tool, map[string]any{
		"steps": []map[string]any{
			{"name": "a", "prompt": "first step"},
			{"name": "b", "prompt": "use {{prev}} then {{steps.a}}"},
		},
	})
	if result.IsError {
		t.Fatalf("workflow = %q", result.Content)
	}
	requireContains(t, result.Content, "workflow: 2 step(s) completed", "== a ==", "out-A", "== b ==", "out-B")

	specs := runner.recordedSpecs()
	if len(specs) != 2 || specs[0].Name != "a" || specs[1].Name != "b" {
		t.Fatalf("specs = %+v, want steps in order", specs)
	}
	if specs[0].Prompt != "first step" {
		t.Fatalf("first prompt = %q", specs[0].Prompt)
	}
	if want := "use out-A then out-A"; specs[1].Prompt != want {
		t.Fatalf("second prompt = %q, want %q (both templates expanded)", specs[1].Prompt, want)
	}
}

func TestTasksAlias(t *testing.T) {
	runner := &fakeRunner{defaultOut: "task output"}
	tool := workflow.New(runner)
	result := invokeWorkflow(t, tool, map[string]any{
		"tasks": []map[string]any{{"name": "only", "prompt": "do it"}},
	})
	if result.IsError || !strings.Contains(result.Content, "task output") {
		t.Fatalf("tasks alias result = %q (IsError=%v)", result.Content, result.IsError)
	}
	specs := runner.recordedSpecs()
	if len(specs) != 1 || specs[0].Name != "only" || specs[0].Prompt != "do it" {
		t.Fatalf("specs = %+v", specs)
	}
}

func TestFailurePropagation(t *testing.T) {
	runner := &fakeRunner{
		outputs:  map[string]string{"a": "out-A"},
		failName: "b",
	}
	result := invokeWorkflow(t, workflow.New(runner), map[string]any{
		"steps": []map[string]any{
			{"name": "a", "prompt": "first"},
			{"name": "b", "prompt": "second"},
			{"name": "c", "prompt": "third"},
		},
	})
	if !result.IsError {
		t.Fatalf("workflow = %q, want IsError", result.Content)
	}
	requireContains(t, result.Content, "out-A", `FAILED at step "b"`, "step exploded: b")
	if strings.Contains(result.Content, "third") {
		t.Fatalf("workflow continued past the failure: %q", result.Content)
	}
	if got := len(runner.recordedSpecs()); got != 2 {
		t.Fatalf("steps started = %d, want 2 (stop after failure)", got)
	}
}

func TestParallelGroupRunsConcurrently(t *testing.T) {
	runner := &fakeRunner{
		defaultOut: "parallel output",
		delay:      20 * time.Millisecond,
	}
	result := invokeWorkflow(t, workflow.New(runner), map[string]any{
		"max_parallel": 3,
		"steps": []map[string]any{
			{"name": "a", "prompt": "one", "parallel": true},
			{"name": "b", "prompt": "two", "parallel": true},
			{"name": "c", "prompt": "three", "parallel": true},
		},
	})
	if result.IsError {
		t.Fatalf("parallel workflow = %q", result.Content)
	}
	if peak := runner.peak.Load(); peak < 2 {
		t.Fatalf("peak concurrent steps = %d, want >= 2 for parallel:true", peak)
	}
	// The summary keeps step order regardless of completion order.
	text := result.Content
	if !(strings.Index(text, "== a ==") < strings.Index(text, "== b ==") &&
		strings.Index(text, "== b ==") < strings.Index(text, "== c ==")) {
		t.Fatalf("summary order = %q, want a, b, c", text)
	}
}

func TestParallelCapIsRespected(t *testing.T) {
	runner := &fakeRunner{
		defaultOut: "ok",
		delay:      20 * time.Millisecond,
	}
	result := invokeWorkflow(t, workflow.New(runner), map[string]any{
		"max_parallel": 1,
		"steps": []map[string]any{
			{"name": "a", "prompt": "one", "parallel": true},
			{"name": "b", "prompt": "two", "parallel": true},
			{"name": "c", "prompt": "three", "parallel": true},
		},
	})
	if result.IsError {
		t.Fatalf("workflow = %q", result.Content)
	}
	if peak := runner.peak.Load(); peak != 1 {
		t.Fatalf("peak concurrent steps = %d, want 1 with max_parallel=1", peak)
	}
}

func TestParallelFailurePropagation(t *testing.T) {
	runner := &fakeRunner{
		outputs:  map[string]string{"a": "out-A"},
		failName: "b",
	}
	result := invokeWorkflow(t, workflow.New(runner), map[string]any{
		"steps": []map[string]any{
			{"name": "a", "prompt": "one", "parallel": true},
			{"name": "b", "prompt": "two", "parallel": true},
		},
	})
	if !result.IsError {
		t.Fatalf("workflow = %q, want IsError", result.Content)
	}
	requireContains(t, result.Content, `FAILED at step "b"`, "step exploded: b")
}

func TestValidationAndConfigurationErrors(t *testing.T) {
	// A nil runner is a host configuration error, not a model tool result.
	if _, err := workflow.New(nil).Invoke(context.Background(), v1.ToolCall{Arguments: `{"steps":[{"prompt":"x"}]}`}); err == nil ||
		!strings.Contains(err.Error(), "SubagentRunner") {
		t.Fatalf("nil runner error = %v, want configuration error", err)
	}
	runner := &fakeRunner{defaultOut: "ok"}
	tool := workflow.New(runner)
	if result := invokeWorkflow(t, tool, map[string]any{}); !result.IsError ||
		!strings.Contains(result.Content, "steps is required") {
		t.Fatalf("empty steps = %q (IsError=%v)", result.Content, result.IsError)
	}
	if result := invokeWorkflow(t, tool, map[string]any{
		"steps": []map[string]any{{"name": "a", "prompt": "x"}, {"name": "a", "prompt": "y"}},
	}); !result.IsError || !strings.Contains(result.Content, "duplicate") {
		t.Fatalf("duplicate names = %q", result.Content)
	}
	if result := invokeWorkflow(t, tool, map[string]any{
		"steps": []map[string]any{{"name": "a", "prompt": "  "}},
	}); !result.IsError || !strings.Contains(result.Content, "empty prompt") {
		t.Fatalf("empty prompt = %q", result.Content)
	}
	// A start failure becomes a model-facing failure summary.
	startFailure := &fakeRunner{startErrName: "a", defaultOut: "ok"}
	result := invokeWorkflow(t, workflow.New(startFailure), map[string]any{
		"steps": []map[string]any{{"name": "a", "prompt": "x"}},
	})
	if !result.IsError || !strings.Contains(result.Content, `FAILED at step "a"`) {
		t.Fatalf("start failure = %q (IsError=%v)", result.Content, result.IsError)
	}
	// Malformed JSON is a Go error.
	if _, err := tool.Invoke(context.Background(), v1.ToolCall{Arguments: "{"}); err == nil {
		t.Fatal("malformed arguments returned nil error")
	}
}

func TestSchema(t *testing.T) {
	tool := workflow.New(&fakeRunner{})
	if tool.Description() == "" {
		t.Fatal("workflow has an empty description")
	}
	schema := tool.Schema()
	properties, _ := schema["properties"].(map[string]any)
	for _, name := range []string{"steps", "tasks", "max_parallel"} {
		if _, ok := properties[name]; !ok {
			t.Fatalf("schema is missing %q: %v", name, properties)
		}
	}
}

// --- end-to-end with the real runner ---------------------------------------

type countingProvider struct {
	text  string
	calls atomic.Int32
}

func (p *countingProvider) Name() string { return "counting" }

func (p *countingProvider) Stream(context.Context, v1.Request) (v1.Stream, error) {
	p.calls.Add(1)
	return &oneChunkStream{text: p.text}, nil
}

type oneChunkStream struct {
	text string
	sent bool
}

func (s *oneChunkStream) Recv() (v1.Chunk, error) {
	if s.sent {
		return v1.Chunk{}, io.EOF
	}
	s.sent = true
	return v1.Chunk{Delta: s.text, FinishReason: "stop"}, nil
}

func TestRealRunnerEndToEnd(t *testing.T) {
	provider := &countingProvider{text: "child answer"}
	runner, err := agent.NewRunner(v1.AgentSpec{Model: "m", Provider: provider}, agent.DefaultRunnerOptions())
	if err != nil {
		t.Fatalf("agent.NewRunner: %v", err)
	}
	tool := workflow.New(runner)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	payload := `{"steps":[{"name":"a","prompt":"one"},{"name":"b","prompt":"two: {{prev}}"}]}`
	result, err := tool.Invoke(ctx, v1.ToolCall{Arguments: payload})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.IsError {
		t.Fatalf("workflow = %q", result.Content)
	}
	requireContains(t, result.Content, "== a ==", "== b ==")
	if got := provider.calls.Load(); got != 2 {
		t.Fatalf("provider calls = %d, want 2 (one per step)", got)
	}
}
