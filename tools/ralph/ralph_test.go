package ralph_test

// Tests for the ralph loop: scripted subagents drive completion, round limits,
// completion-prompt templates, failures and cancellation.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
	"github.com/zeokeel/zeokeel-rig/tools/ralph"
)

type fakeSubagent struct {
	mu         sync.Mutex
	waitResult v1.SubagentResult
	waitErr    error
	responses  []string
	sendErr    error
	sends      []string
	stopCalls  int
}

func (s *fakeSubagent) Name() string { return "ralph-child" }

func (s *fakeSubagent) Wait(ctx context.Context) (v1.SubagentResult, error) {
	if err := ctx.Err(); err != nil {
		return v1.SubagentResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.waitResult, s.waitErr
}

func (s *fakeSubagent) Send(ctx context.Context, message string) (v1.SubagentResult, error) {
	if err := ctx.Err(); err != nil {
		return v1.SubagentResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sends = append(s.sends, message)
	if s.sendErr != nil {
		return v1.SubagentResult{}, s.sendErr
	}
	if len(s.responses) == 0 {
		return v1.SubagentResult{Output: "still busy"}, nil
	}
	output := s.responses[0]
	s.responses = s.responses[1:]
	return v1.SubagentResult{Output: output}, nil
}

func (s *fakeSubagent) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopCalls++
	return nil
}

func (s *fakeSubagent) sent() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sends...)
}

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
		sub = &fakeSubagent{waitResult: v1.SubagentResult{Output: "first output"}}
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

func invoke(t *testing.T, runner v1.SubagentRunner, arguments string) v1.ToolResult {
	t.Helper()
	result, err := ralph.New(runner).Invoke(context.Background(), v1.ToolCall{ID: "call-1", Name: ralph.ToolName, Arguments: arguments})
	if err != nil {
		t.Fatalf("ralph.Invoke returned Go error: %v", err)
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

func TestTwoRoundsUntilDone(t *testing.T) {
	sub := &fakeSubagent{
		waitResult: v1.SubagentResult{Output: "still working"},
		responses:  []string{"all done"},
	}
	runner := &fakeRunner{sub: sub}
	tool := ralph.New(runner)
	if tool.Name() != ralph.ToolName {
		t.Fatalf("Name = %q, want %q", tool.Name(), ralph.ToolName)
	}

	result := invoke(t, runner, `{"prompt":"fix the bug","name":"worker"}`)
	if result.IsError {
		t.Fatalf("ralph = %q", result.Content)
	}
	requireContains(t, result.Content,
		"completed after 2 round(s)",
		"== round 1 ==", "still working",
		"== round 2 ==", "all done",
		"final output:", "all done")
	specs := runner.recorded()
	if len(specs) != 1 || specs[0].Name != "worker" || specs[0].Prompt != "fix the bug" {
		t.Fatalf("specs = %+v, want the task spec", specs)
	}
	sends := sub.sent()
	if len(sends) != 1 {
		t.Fatalf("Send calls = %d, want 1", len(sends))
	}
	requireContains(t, sends[0], "still working", ralph.DefaultCompletionPrompt, "Previous round output:")
	if got := sub.stopCalls; got != 1 {
		t.Fatalf("Stop calls = %d, want 1", got)
	}
}

func TestMaxRoundsWithoutCompletion(t *testing.T) {
	sub := &fakeSubagent{
		waitResult: v1.SubagentResult{Output: "round one"},
		responses:  []string{"round two", "round three"},
	}
	result := invoke(t, &fakeRunner{sub: sub}, `{"prompt":"never finish","max_rounds":2}`)
	if !result.IsError {
		t.Fatalf("ralph = %q, want IsError after max rounds", result.Content)
	}
	requireContains(t, result.Content, "reached max_rounds (2)", "== round 1 ==", "== round 2 ==")
	if sends := sub.sent(); len(sends) != 1 {
		t.Fatalf("Send calls = %d, want 1 (two rounds total)", len(sends))
	}
}

func TestCompletionPromptTemplate(t *testing.T) {
	sub := &fakeSubagent{
		waitResult: v1.SubagentResult{Output: "first output"},
		responses:  []string{"DONE"},
	}
	arguments := `{"prompt":"go","completion_prompt":"Round {{round}}: {{output}} — reply DONE"}`
	result := invoke(t, &fakeRunner{sub: sub}, arguments)
	if result.IsError {
		t.Fatalf("ralph = %q", result.Content)
	}
	sends := sub.sent()
	if len(sends) != 1 {
		t.Fatalf("Send calls = %d, want 1", len(sends))
	}
	if want := "Round 2: first output — reply DONE"; sends[0] != want {
		t.Fatalf("Send prompt = %q, want %q", sends[0], want)
	}
}

func TestSubagentFailures(t *testing.T) {
	// A first-round failure is reported with the round number.
	waitFailure := &fakeSubagent{waitErr: errors.New("model exploded")}
	result := invoke(t, &fakeRunner{sub: waitFailure}, `{"prompt":"x"}`)
	if !result.IsError || !strings.Contains(result.Content, "round 1 failed") {
		t.Fatalf("wait failure = %q (IsError=%v)", result.Content, result.IsError)
	}

	// A later-round failure keeps the completed rounds in the summary.
	sendFailure := &fakeSubagent{
		waitResult: v1.SubagentResult{Output: "round one"},
		sendErr:    errors.New("send exploded"),
	}
	result = invoke(t, &fakeRunner{sub: sendFailure}, `{"prompt":"x"}`)
	if !result.IsError {
		t.Fatalf("send failure = %q, want IsError", result.Content)
	}
	requireContains(t, result.Content, "round 2 failed", "send exploded", "round one")
	if got := sendFailure.stopCalls; got != 1 {
		t.Fatalf("Stop calls after failure = %d, want 1", got)
	}
}

func TestCancellationIsAGoError(t *testing.T) {
	sub := &fakeSubagent{waitResult: v1.SubagentResult{Output: "x"}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ralph.New(&fakeRunner{sub: sub}).Invoke(ctx, v1.ToolCall{Arguments: `{"prompt":"x"}`})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled ralph error = %v, want context.Canceled", err)
	}
}

func TestCompletionMarkers(t *testing.T) {
	cases := []struct {
		name     string
		first    string
		complete bool
	}{
		{"plain DONE", "DONE", true},
		{"DONE with punctuation", "task: DONE!", true},
		{"lowercase", "done", true},
		{"chinese marker", "已完成，完成", true},
		{"undone is not done", "this is undone", false},
		{"not yet", "not yet, keep going", false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			sub := &fakeSubagent{waitResult: v1.SubagentResult{Output: testCase.first}}
			result := invoke(t, &fakeRunner{sub: sub}, `{"prompt":"go","max_rounds":1}`)
			if result.IsError == testCase.complete {
				t.Fatalf("output %q: content = %q (IsError=%v), want complete=%v",
					testCase.first, result.Content, result.IsError, testCase.complete)
			}
		})
	}
}

func TestValidationAndConfiguration(t *testing.T) {
	// A nil runner is a host configuration error.
	_, err := ralph.New(nil).Invoke(context.Background(), v1.ToolCall{Arguments: `{"prompt":"x"}`})
	if err == nil || !strings.Contains(err.Error(), "SubagentRunner") {
		t.Fatalf("nil runner error = %v, want configuration error", err)
	}
	// Missing prompt is a model-facing error.
	result := invoke(t, &fakeRunner{sub: &fakeSubagent{waitResult: v1.SubagentResult{Output: "DONE"}}}, `{}`)
	if !result.IsError || !strings.Contains(result.Content, "prompt is required") {
		t.Fatalf("missing prompt = %q (IsError=%v)", result.Content, result.IsError)
	}
	// Start errors pass through as Go errors.
	_, err = ralph.New(&fakeRunner{startErr: errors.New("depth limit reached")}).Invoke(
		context.Background(), v1.ToolCall{Arguments: `{"prompt":"x"}`})
	if err == nil || !strings.Contains(err.Error(), "depth limit reached") {
		t.Fatalf("start error = %v, want passthrough", err)
	}
	if ralph.New(&fakeRunner{}).Description() == "" {
		t.Fatal("ralph has an empty description")
	}
	schema := ralph.New(&fakeRunner{}).Schema()
	required, _ := schema["required"].([]string)
	if len(required) != 1 || required[0] != "prompt" {
		t.Fatalf("schema required = %v, want [prompt]", required)
	}
}
