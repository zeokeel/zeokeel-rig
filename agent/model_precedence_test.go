package agent_test

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/agent"
)

// modelRecorderProvider is a rig.Provider that records the model name of every
// request and answers with one text chunk.
type modelRecorderProvider struct {
	mu     sync.Mutex
	models []string
}

func (p *modelRecorderProvider) Name() string { return "model-recorder" }

func (p *modelRecorderProvider) Stream(ctx context.Context, req rig.Request) (rig.Stream, error) {
	p.mu.Lock()
	p.models = append(p.models, req.Model)
	p.mu.Unlock()
	return &modelRecorderStream{}, nil
}

func (p *modelRecorderProvider) recorded() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.models)
}

// modelRecorderStream returns one stop chunk and then io.EOF.
type modelRecorderStream struct{ done bool }

func (s *modelRecorderStream) Recv() (rig.Chunk, error) {
	if s.done {
		return rig.Chunk{}, io.EOF
	}
	s.done = true
	return rig.Chunk{Delta: "ok", FinishReason: "stop"}, nil
}

// TestModelPrecedence pins the documented rule: RunRequest.Model wins over
// AgentSpec.Model, both are whitespace-trimmed, and an empty/blank request model
// falls back to the agent's configured model. Because an Agent always sends its
// configured model, a provider's DefaultModel is never consulted on this path.
func TestModelPrecedence(t *testing.T) {
	tests := []struct {
		name         string
		specModel    string
		requestModel string
		want         string
	}{
		{name: "spec model used when request model is empty", specModel: "spec-model", requestModel: "", want: "spec-model"},
		{name: "request model wins", specModel: "spec-model", requestModel: "request-model", want: "request-model"},
		{name: "blank request model falls back to spec model", specModel: "spec-model", requestModel: "   ", want: "spec-model"},
		{name: "spec model is trimmed", specModel: "  spec-model  ", requestModel: "", want: "spec-model"},
		{name: "request model is trimmed", specModel: "spec-model", requestModel: "  request-model  ", want: "request-model"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := &modelRecorderProvider{}
			a, err := agent.New(rig.AgentSpec{Model: test.specModel, Provider: provider})
			if err != nil {
				t.Fatalf("agent.New: %v", err)
			}
			_, err = a.Run(context.Background(), rig.RunRequest{
				Model: test.requestModel,
				Input: []rig.Message{userMessage("hi")},
			})
			requireNoError(t, err)

			models := provider.recorded()
			if len(models) != 1 {
				t.Fatalf("provider calls = %d, want 1", len(models))
			}
			if models[0] != test.want {
				t.Fatalf("model sent to Provider.Stream = %q, want %q", models[0], test.want)
			}
			if strings.TrimSpace(models[0]) == "" {
				t.Fatal("agent sent an empty model to Provider.Stream")
			}
		})
	}
}

// TestAgentNeverSendsEmptyModel covers the other half of the contract: an agent
// cannot exist without a default model, so no empty request model can produce a
// model-less provider request (the provider's own default model never applies on
// the Agent path).
func TestAgentNeverSendsEmptyModel(t *testing.T) {
	provider := &modelRecorderProvider{}

	_, err := agent.New(rig.AgentSpec{Provider: provider})
	if !errors.Is(err, rig.ErrModelRequired) {
		t.Fatalf("agent.New without a model error = %v, want rig.ErrModelRequired", err)
	}

	a := mustNewAgent(t, rig.AgentSpec{Model: "configured-model", Provider: provider})
	for _, requestModel := range []string{"", " ", "\t\n"} {
		_, runErr := a.Run(context.Background(), rig.RunRequest{
			Model: requestModel,
			Input: []rig.Message{userMessage("hi")},
		})
		requireNoError(t, runErr)
	}

	models := provider.recorded()
	if len(models) != 3 {
		t.Fatalf("provider calls = %d, want 3", len(models))
	}
	for i, model := range models {
		if model != "configured-model" {
			t.Fatalf("call %d model = %q, want configured-model", i, model)
		}
	}
}
