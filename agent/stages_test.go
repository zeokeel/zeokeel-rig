package agent_test

// Stage hooks are the per-run extension points above TurnHook: prompt, request,
// response, turn boundaries and compaction input. These tests pin that each one
// runs at the documented point and that a mutation is what the next layer sees.

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/agent"
)

type promptStageFunc func(ctx context.Context, draft *rig.PromptDraft) error

func (f promptStageFunc) Prompt(ctx context.Context, draft *rig.PromptDraft) error {
	return f(ctx, draft)
}

type requestStageFunc func(ctx context.Context, draft *rig.RequestDraft) error

func (f requestStageFunc) Request(ctx context.Context, draft *rig.RequestDraft) error {
	return f(ctx, draft)
}

type responseStageFunc func(ctx context.Context, draft *rig.ResponseDraft) error

func (f responseStageFunc) Response(ctx context.Context, draft *rig.ResponseDraft) error {
	return f(ctx, draft)
}

type compactionStageFunc func(ctx context.Context, draft *rig.CompactionDraft) error

func (f compactionStageFunc) Compact(ctx context.Context, draft *rig.CompactionDraft) error {
	return f(ctx, draft)
}

type turnStageRecorder struct {
	name   string
	order  int
	starts []int
	ends   []int
}

func (s *turnStageRecorder) Order() int { return s.order }

func (s *turnStageRecorder) TurnStart(_ context.Context, info *rig.TurnInfo) error {
	s.starts = append(s.starts, info.Iteration)
	return nil
}

func (s *turnStageRecorder) TurnEnd(_ context.Context, info *rig.TurnInfo) error {
	s.ends = append(s.ends, info.Iteration)
	return nil
}

func textProvider(text string) *scriptProvider {
	return &scriptProvider{scripts: [][]rig.Chunk{{{Delta: text, FinishReason: "stop"}}}}
}

func newStagedAgent(t *testing.T, spec rig.AgentSpec) rig.Agent {
	t.Helper()
	a, err := agent.New(spec)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	return a
}

func TestPromptStageRewritesSystemPrompt(t *testing.T) {
	provider := textProvider("ok")
	a := newStagedAgent(t, rig.AgentSpec{
		Provider:     provider,
		Model:        "m",
		SystemPrompt: "base",
		PromptStages: []rig.PromptStage{promptStageFunc(func(_ context.Context, draft *rig.PromptDraft) error {
			if draft.SystemPrompt != "base" {
				t.Fatalf("draft prompt = %q, want base", draft.SystemPrompt)
			}
			draft.SystemPrompt += "\nextra"
			return nil
		})},
	})
	if _, err := a.Run(context.Background(), rig.RunRequest{Input: []rig.Message{rig.NewMessage(rig.RoleUser, "hi")}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	messages := provider.request(0).Messages
	if len(messages) == 0 || messages[0].Role != rig.RoleSystem || messages[0].Text() != "base\nextra" {
		t.Fatalf("request system message = %+v", messages)
	}
}

func TestRequestStageRewritesModelAndMessages(t *testing.T) {
	provider := textProvider("ok")
	a := newStagedAgent(t, rig.AgentSpec{
		Provider: provider,
		Model:    "default",
		RequestStages: []rig.RequestStage{requestStageFunc(func(_ context.Context, draft *rig.RequestDraft) error {
			draft.Model = "override"
			draft.Messages = append(draft.Messages, rig.NewMessage(rig.RoleSystem, "injected"))
			return nil
		})},
	})
	if _, err := a.Run(context.Background(), rig.RunRequest{Input: []rig.Message{rig.NewMessage(rig.RoleUser, "hi")}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	request := provider.request(0)
	if request.Model != "override" {
		t.Fatalf("request model = %q, want override", request.Model)
	}
	if last := request.Messages[len(request.Messages)-1]; last.Text() != "injected" {
		t.Fatalf("last request message = %+v", last)
	}
}

func TestResponseStageRewritesAssistantText(t *testing.T) {
	provider := textProvider("raw")
	a := newStagedAgent(t, rig.AgentSpec{
		Provider: provider,
		Model:    "m",
		ResponseStages: []rig.ResponseStage{responseStageFunc(func(_ context.Context, draft *rig.ResponseDraft) error {
			draft.Message.Content = []rig.ContentPart{rig.TextPart("rewritten")}
			return nil
		})},
	})
	result, err := a.Run(context.Background(), rig.RunRequest{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Messages) != 1 || result.Messages[0].Text() != "rewritten" {
		t.Fatalf("result = %+v", result.Messages)
	}
}

func TestTurnStagesRunInOrderAndPerIteration(t *testing.T) {
	first := &turnStageRecorder{name: "first", order: 20}
	second := &turnStageRecorder{name: "second", order: 10}
	a := newStagedAgent(t, rig.AgentSpec{
		Provider:   textProvider("done"),
		Model:      "m",
		TurnStages: []rig.TurnStage{first, second},
	})
	if _, err := a.Run(context.Background(), rig.RunRequest{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !slices.Equal(first.starts, []int{1}) || !slices.Equal(first.ends, []int{1}) {
		t.Fatalf("first stage turns = %v/%v", first.starts, first.ends)
	}
	if !slices.Equal(second.starts, []int{1}) || !slices.Equal(second.ends, []int{1}) {
		t.Fatalf("second stage turns = %v/%v", second.starts, second.ends)
	}
}

type stageRecordingCompactor struct{ inputs [][]rig.Message }

func (c *stageRecordingCompactor) Compact(_ context.Context, _ string, messages []rig.Message, _ int) ([]rig.Message, error) {
	c.inputs = append(c.inputs, append([]rig.Message(nil), messages...))
	return messages[:1], nil
}

func TestCompactionStageRewritesInput(t *testing.T) {
	provider := textProvider("ok")
	compactor := &stageRecordingCompactor{}
	a := newStagedAgent(t, rig.AgentSpec{
		Provider:      provider,
		Model:         "m",
		ContextBudget: 1,
		Compactor:     compactor,
		CompactionStages: []rig.CompactionStage{compactionStageFunc(func(_ context.Context, draft *rig.CompactionDraft) error {
			draft.Messages = append(draft.Messages, rig.NewMessage(rig.RoleUser, "from-stage"))
			return nil
		})},
	})
	if _, err := a.Run(context.Background(), rig.RunRequest{
		Input: []rig.Message{rig.NewMessage(rig.RoleUser, strings.Repeat("long input ", 100))},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(compactor.inputs) != 1 || compactor.inputs[0][len(compactor.inputs[0])-1].Text() != "from-stage" {
		t.Fatalf("compactor input = %+v", compactor.inputs)
	}
}

func TestStageErrorAbortsBeforeProvider(t *testing.T) {
	provider := textProvider("unused")
	a := newStagedAgent(t, rig.AgentSpec{
		Provider: provider,
		Model:    "m",
		PromptStages: []rig.PromptStage{promptStageFunc(func(context.Context, *rig.PromptDraft) error {
			return errors.New("refuse")
		})},
	})
	if _, err := a.Run(context.Background(), rig.RunRequest{}); err == nil {
		t.Fatal("Run succeeded despite a prompt stage error")
	}
	if provider.callCount() != 0 {
		t.Fatalf("provider calls = %d, want 0", provider.callCount())
	}
}
