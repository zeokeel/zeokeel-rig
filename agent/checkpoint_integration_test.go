package agent_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/agent"
)

type integrationCheckpointStore struct {
	mu      sync.Mutex
	current *rig.Checkpoint
	saves   []*rig.Checkpoint
	saveErr error
}

func (s *integrationCheckpointStore) Load(context.Context, string) (*rig.Checkpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current.Clone(), nil
}

func (s *integrationCheckpointStore) Save(_ context.Context, _ string, checkpoint *rig.Checkpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveErr != nil {
		return s.saveErr
	}
	s.current = checkpoint.Clone()
	s.saves = append(s.saves, checkpoint.Clone())
	return nil
}

func (s *integrationCheckpointStore) Clear(context.Context, string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current = nil
	return nil
}

func (s *integrationCheckpointStore) phases() []rig.CheckpointPhase {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]rig.CheckpointPhase, len(s.saves))
	for i, checkpoint := range s.saves {
		out[i] = checkpoint.Phase
	}
	return out
}

func TestAgentWritesCheckpoints(t *testing.T) {
	store := &integrationCheckpointStore{}
	provider := &scriptProvider{scripts: [][]rig.Chunk{textScript("done")}}
	a := mustNewAgent(t, rig.AgentSpec{
		Model:           "m",
		Provider:        provider,
		CheckpointStore: store,
	})
	_, err := a.Run(context.Background(), rig.RunRequest{SessionID: "s", Input: []rig.Message{rig.NewMessage(rig.RoleUser, "hi")}})
	requireNoError(t, err)
	phases := store.phases()
	if len(phases) != 2 || phases[0] != rig.CheckpointPhaseRequested || phases[1] != rig.CheckpointPhaseCompleted {
		t.Fatalf("checkpoint phases = %v", phases)
	}
	current := store.current
	if current == nil || current.TurnID == "" || current.PriorMessages != 0 {
		t.Fatalf("current checkpoint = %+v", current)
	}
	if current.Messages[len(current.Messages)-1].Text() != "done" {
		t.Fatalf("completed checkpoint messages = %+v", current.Messages)
	}
}

func TestAgentRecoversInterruptedToolCall(t *testing.T) {
	store := &integrationCheckpointStore{current: &rig.Checkpoint{
		SessionID: "s",
		TurnID:    "turn-interrupted",
		Phase:     rig.CheckpointPhaseToolPending,
		Messages: []rig.Message{
			rig.NewMessage(rig.RoleUser, "run it"),
			{Role: rig.RoleAssistant, ToolCalls: []rig.ToolCall{{ID: "call_1", Name: "bash", Arguments: `{}`}}},
		},
		PriorMessages: 0,
	}}
	provider := &scriptProvider{scripts: [][]rig.Chunk{textScript("recovered")}}
	a := mustNewAgent(t, rig.AgentSpec{Model: "m", Provider: provider, CheckpointStore: store})
	_, err := a.Run(context.Background(), rig.RunRequest{SessionID: "s", Input: []rig.Message{rig.NewMessage(rig.RoleUser, "continue")}})
	requireNoError(t, err)
	if len(provider.calls) != 1 {
		t.Fatalf("provider calls = %d", len(provider.calls))
	}
	messages := provider.calls[0].Messages
	var sawToolResult, sawMarker bool
	for _, message := range messages {
		if message.Role == rig.RoleTool && message.ToolCallID == "call_1" && message.Text() == agent.InterruptedToolResult {
			sawToolResult = true
		}
		if message.Role == rig.RoleAssistant && message.Text() == agent.InterruptedTurnMarker {
			sawMarker = true
		}
	}
	if !sawToolResult || !sawMarker {
		t.Fatalf("provider messages missing recovery tail: %+v", messages)
	}
	if store.current == nil || store.current.Phase != rig.CheckpointPhaseCompleted {
		t.Fatalf("repaired checkpoint = %+v", store.current)
	}
}

func TestAgentCheckpointSaveFailureFailsClosed(t *testing.T) {
	store := &integrationCheckpointStore{saveErr: errors.New("disk full")}
	provider := &scriptProvider{scripts: [][]rig.Chunk{textScript("never")}}
	a := mustNewAgent(t, rig.AgentSpec{Model: "m", Provider: provider, CheckpointStore: store})
	_, err := a.Run(context.Background(), rig.RunRequest{SessionID: "s", Input: []rig.Message{rig.NewMessage(rig.RoleUser, "hi")}})
	if err == nil {
		t.Fatal("Run succeeded despite checkpoint save failure")
	}
	if len(provider.calls) != 0 {
		t.Fatalf("provider was called before checkpoint failure: %d", len(provider.calls))
	}
}
