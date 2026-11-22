package team

import (
	"context"
	"sync"
)

// Persistence seam: the coordinator saves a deep snapshot after every published
// change, and Restore reads it back. MemoryStore is a ready-made in-process
// implementation; a durable host implements Store over its own storage.

// MemoryStore is an in-memory Store. It is safe for concurrent use and keeps
// deep copies, so callers can neither mutate the coordinator through Load nor
// mutate the stored state through Save.
type MemoryStore struct {
	mu    sync.Mutex
	state TeamState
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{}
}

// Load implements Store.
func (s *MemoryStore) Load(ctx context.Context) (TeamState, error) {
	if err := contextErr(ctx); err != nil {
		return TeamState{}, err
	}
	if s == nil {
		return TeamState{}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneState(s.state), nil
}

// Save implements Store.
func (s *MemoryStore) Save(ctx context.Context, state TeamState) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	if s == nil {
		return nil
	}
	s.mu.Lock()
	s.state = cloneState(state)
	s.mu.Unlock()
	return nil
}

// cloneState deep-copies a TeamState.
func cloneState(state TeamState) TeamState {
	return TeamState{
		Revision: state.Revision,
		Members:  cloneTeammates(state.Members),
		Activity: cloneActivities(state.Activity),
	}
}

// cloneTeammates deep-copies a teammate list, including every activity row.
func cloneTeammates(members []Teammate) []Teammate {
	if len(members) == 0 {
		return nil
	}
	cloned := make([]Teammate, len(members))
	for index, member := range members {
		member.Activity = cloneActivities(member.Activity)
		cloned[index] = member
	}
	return cloned
}

// cloneActivities deep-copies an activity list.
func cloneActivities(rows []Activity) []Activity {
	if len(rows) == 0 {
		return nil
	}
	return append([]Activity(nil), rows...)
}
