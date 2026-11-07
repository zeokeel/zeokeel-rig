package rig_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

// stubCheckpointStore 只是用来把 CheckpointStore 的方法集合钉在编译期，防止
// 后续改动无意中破坏宿主实现。
type stubCheckpointStore struct{}

var _ rig.CheckpointStore = stubCheckpointStore{}

func (stubCheckpointStore) Load(context.Context, string) (*rig.Checkpoint, error) {
	return nil, nil
}

func (stubCheckpointStore) Save(context.Context, string, *rig.Checkpoint) error { return nil }

func (stubCheckpointStore) Clear(context.Context, string) error { return nil }

// TestCheckpointPhase pins the phase vocabulary and the interrupted predicate.
func TestCheckpointPhase(t *testing.T) {
	tests := []struct {
		phase       rig.CheckpointPhase
		valid       bool
		interrupted bool
	}{
		{rig.CheckpointPhaseCompleted, true, false},
		{rig.CheckpointPhaseRequested, true, true},
		{rig.CheckpointPhaseToolPending, true, true},
		{"", false, false},
		{"unknown", false, false},
		{"Completed", false, false},
	}
	for _, test := range tests {
		if got := test.phase.Valid(); got != test.valid {
			t.Errorf("%q.Valid() = %v, want %v", test.phase, got, test.valid)
		}
		if got := test.phase.Interrupted(); got != test.interrupted {
			t.Errorf("%q.Interrupted() = %v, want %v", test.phase, got, test.interrupted)
		}
	}
}

// TestCheckpointValidate covers the required-field and range checks, including
// the sentinel errors host stores can match with errors.Is.
func TestCheckpointValidate(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	valid := []*rig.Checkpoint{
		{
			SessionID: "s-1",
			TurnID:    "turn-1",
			Phase:     rig.CheckpointPhaseCompleted,
			Messages:  []rig.Message{rig.NewMessage(rig.RoleUser, "hi")},
			CreatedAt: now,
			UpdatedAt: now,
		},
		{
			SessionID:     "s-2",
			TurnID:        "turn-2",
			Phase:         rig.CheckpointPhaseRequested,
			Messages:      []rig.Message{rig.NewMessage(rig.RoleUser, "hi")},
			PriorMessages: 1,
		},
		{
			SessionID: "s-3",
			TurnID:    "turn-3",
			Phase:     rig.CheckpointPhaseToolPending,
		},
	}
	for _, checkpoint := range valid {
		if err := checkpoint.Validate(); err != nil {
			t.Errorf("Validate(%+v) = %v, want nil", checkpoint, err)
		}
	}

	tests := []struct {
		name       string
		checkpoint *rig.Checkpoint
		want       error
	}{
		{name: "nil", checkpoint: nil, want: rig.ErrCheckpointInvalid},
		{name: "missing session", checkpoint: &rig.Checkpoint{Phase: rig.CheckpointPhaseCompleted}, want: rig.ErrCheckpointSessionRequired},
		{name: "blank session", checkpoint: &rig.Checkpoint{SessionID: "   ", Phase: rig.CheckpointPhaseCompleted}, want: rig.ErrCheckpointSessionRequired},
		{name: "invalid phase", checkpoint: &rig.Checkpoint{SessionID: "s", Phase: "weird"}, want: rig.ErrCheckpointPhaseInvalid},
		{name: "interrupted without turn", checkpoint: &rig.Checkpoint{SessionID: "s", Phase: rig.CheckpointPhaseRequested}, want: rig.ErrCheckpointTurnRequired},
		{name: "tool pending without turn", checkpoint: &rig.Checkpoint{SessionID: "s", Phase: rig.CheckpointPhaseToolPending}, want: rig.ErrCheckpointTurnRequired},
		{name: "negative prior", checkpoint: &rig.Checkpoint{SessionID: "s", Phase: rig.CheckpointPhaseCompleted, PriorMessages: -1}, want: rig.ErrCheckpointInvalid},
		{
			name: "prior beyond messages",
			checkpoint: &rig.Checkpoint{
				SessionID:     "s",
				Phase:         rig.CheckpointPhaseCompleted,
				Messages:      []rig.Message{rig.NewMessage(rig.RoleUser, "hi")},
				PriorMessages: 2,
			},
			want: rig.ErrCheckpointInvalid,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.checkpoint.Validate()
			if !errors.Is(err, test.want) {
				t.Fatalf("Validate = %v, want errors.Is(..., %v)", err, test.want)
			}
		})
	}
}

// TestCheckpointSplit pins the prior/turn boundary used by integration code.
func TestCheckpointSplit(t *testing.T) {
	checkpoint := &rig.Checkpoint{
		Messages: []rig.Message{
			rig.NewMessage(rig.RoleUser, "prior"),
			rig.NewMessage(rig.RoleAssistant, "prior reply"),
			rig.NewMessage(rig.RoleUser, "this turn"),
		},
		PriorMessages: 2,
	}
	prior, turn := checkpoint.Split()
	if len(prior) != 2 || len(turn) != 1 {
		t.Fatalf("Split = (%d, %d) messages, want (2, 1)", len(prior), len(turn))
	}
	if prior[0].Text() != "prior" || turn[0].Text() != "this turn" {
		t.Fatalf("Split content = (%q, %q)", prior[0].Text(), turn[0].Text())
	}

	// Out-of-range boundaries are clamped, not panics.
	checkpoint.PriorMessages = -5
	if prior, turn := checkpoint.Split(); len(prior) != 0 || len(turn) != 3 {
		t.Fatalf("negative Split = (%d, %d), want (0, 3)", len(prior), len(turn))
	}
	checkpoint.PriorMessages = 99
	if prior, turn := checkpoint.Split(); len(prior) != 3 || len(turn) != 0 {
		t.Fatalf("oversized Split = (%d, %d), want (3, 0)", len(prior), len(turn))
	}

	var nilCheckpoint *rig.Checkpoint
	if prior, turn := nilCheckpoint.Split(); prior != nil || turn != nil {
		t.Fatalf("nil Split = (%v, %v), want (nil, nil)", prior, turn)
	}
}

// TestCheckpointClone pins the deep-copy contract: mutating the clone (or the
// original) must not alias message content, tool calls or metadata.
func TestCheckpointClone(t *testing.T) {
	original := &rig.Checkpoint{
		SessionID:     "s-clone",
		TurnID:        "turn-clone",
		Phase:         rig.CheckpointPhaseToolPending,
		PriorMessages: 1,
		Messages: []rig.Message{
			rig.NewMessage(rig.RoleUser, "hi"),
			{
				Role: rig.RoleAssistant,
				ToolCalls: []rig.ToolCall{
					{ID: "call-1", Name: "bash", Arguments: `{}`},
				},
			},
		},
		Metadata: map[string]string{"trace": "abc"},
	}
	cloned := original.Clone()
	if cloned == nil || !reflect.DeepEqual(cloned, original) {
		t.Fatalf("Clone = %+v, want an equal copy of %+v", cloned, original)
	}
	cloned.Messages[0].Content[0].Text = "mutated"
	cloned.Messages[1].ToolCalls[0].Name = "mutated"
	cloned.Metadata["trace"] = "mutated"
	if original.Messages[0].Text() != "hi" {
		t.Fatalf("clone aliases message content: %q", original.Messages[0].Text())
	}
	if original.Messages[1].ToolCalls[0].Name != "bash" {
		t.Fatalf("clone aliases tool calls: %q", original.Messages[1].ToolCalls[0].Name)
	}
	if original.Metadata["trace"] != "abc" {
		t.Fatalf("clone aliases metadata: %q", original.Metadata["trace"])
	}

	var nilCheckpoint *rig.Checkpoint
	if cloned := nilCheckpoint.Clone(); cloned != nil {
		t.Fatalf("nil Clone = %+v, want nil", cloned)
	}
}

// TestCheckpointJSONRoundTrip pins that the contract is directly serializable
// with encoding/json (stores can persist Messages as-is).
func TestCheckpointJSONRoundTrip(t *testing.T) {
	created := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	updated := created.Add(2 * time.Minute)
	original := &rig.Checkpoint{
		SessionID:     "s-json",
		TurnID:        "turn-json",
		Phase:         rig.CheckpointPhaseToolPending,
		Messages:      []rig.Message{rig.NewMessage(rig.RoleUser, "hi")},
		PriorMessages: 0,
		CreatedAt:     created,
		UpdatedAt:     updated,
		Metadata:      map[string]string{"trace": "abc"},
	}
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded rig.Checkpoint
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(&decoded, original) {
		t.Fatalf("round trip = %+v, want %+v", &decoded, original)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatalf("decoded checkpoint Validate: %v", err)
	}
}
