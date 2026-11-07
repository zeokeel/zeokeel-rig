// Package session provides ZeoKeel-Rig's in-memory session state.
//
// Memory is the reference rig.History implementation: a concurrency-safe,
// per-session message store for tests, headless runs and single-process hosts
// that do not need durable storage. Durable implementations (SQLite/JSONL) live
// outside the SDK and implement the same rig.History contract.
package session

import (
	"context"
	"slices"
	"sync"

	"github.com/zeokeel/zeokeel-rig"
)

// Memory is an in-memory rig.History. The zero value is not usable; construct it
// with NewMemory. All methods are safe for concurrent use.
type Memory struct {
	mu       sync.RWMutex
	sessions map[string][]rig.Message
}

// NewMemory returns an empty in-memory history.
func NewMemory() *Memory {
	return &Memory{sessions: make(map[string][]rig.Message)}
}

// Load returns a copy of the stored messages for sessionID. An unknown session
// returns (nil, nil), matching rig.History's contract; the returned slice and its
// messages' content/tool-call slices are copies, so mutating them does not
// affect stored history.
func (m *Memory) Load(ctx context.Context, sessionID string) ([]rig.Message, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	stored := m.sessions[sessionID]
	if len(stored) == 0 {
		return nil, nil
	}
	return cloneMessages(stored), nil
}

// Append adds messages to sessionID atomically: concurrent readers observe
// either none or all messages of one call. The messages are copied before being
// stored, so later mutation of the caller's slice cannot change history.
func (m *Memory) Append(ctx context.Context, sessionID string, messages ...rig.Message) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	if len(messages) == 0 {
		return nil
	}
	copied := cloneMessages(messages)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[sessionID] = append(m.sessions[sessionID], copied...)
	return nil
}

// Len reports how many messages are stored for sessionID. A zero result for an
// unknown session is indistinguishable from an empty session; it exists for
// tests and diagnostics, not for protocol decisions.
func (m *Memory) Len(sessionID string) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions[sessionID])
}

// contextErr reports a cancelled or expired context without depending on the
// caller passing a non-nil one (a nil context panics on Err, so guard it: the
// rig.History contract passes a real context, but defensive code here keeps the
// zero-value-ish call from panicking in tests).
func contextErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

// cloneMessages deep-copies the slice and the per-message Content/ToolCalls
// slices (strings inside ContentPart/ToolCall are immutable values).
func cloneMessages(messages []rig.Message) []rig.Message {
	out := make([]rig.Message, len(messages))
	for i, msg := range messages {
		out[i] = msg
		if msg.Content != nil {
			out[i].Content = slices.Clone(msg.Content)
		}
		if msg.ToolCalls != nil {
			out[i].ToolCalls = slices.Clone(msg.ToolCalls)
		}
	}
	return out
}
