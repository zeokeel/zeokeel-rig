package agent_test

// Context-overflow recovery: one compaction + retry of the same turn, the raw
// history archived before it is replaced, the current user message preserved, and
// unrelated provider errors left alone.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
)

// recoveryHistory is an in-memory HistoryReplacer.
type recoveryHistory struct {
	mu       sync.Mutex
	messages []rig.Message
	replaced int
}

func (h *recoveryHistory) Load(context.Context, string) ([]rig.Message, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]rig.Message(nil), h.messages...), nil
}

func (h *recoveryHistory) Append(_ context.Context, _ string, messages ...rig.Message) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.messages = append(h.messages, messages...)
	return nil
}

func (h *recoveryHistory) Replace(_ context.Context, _ string, messages []rig.Message) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.messages = append([]rig.Message(nil), messages...)
	h.replaced++
	return nil
}

// keepLastCompactor keeps the most recent message and records the budget.
type keepLastCompactor struct {
	mu     sync.Mutex
	calls  int
	budget int
}

func (c *keepLastCompactor) Compact(_ context.Context, _ string, messages []rig.Message, budget int) ([]rig.Message, error) {
	c.mu.Lock()
	c.calls++
	c.budget = budget
	c.mu.Unlock()
	if len(messages) == 0 {
		return nil, nil
	}
	return messages[len(messages)-1:], nil
}

func overflowError() error {
	return errors.New("openai: context_length_exceeded: maximum context length is 128000 tokens")
}

func TestRunRecoversFromContextOverflow(t *testing.T) {
	callCount := 0
	provider := &scriptProvider{fn: func(context.Context, rig.Request) (rig.Stream, error) {
		callCount++
		if callCount == 1 {
			return nil, overflowError()
		}
		return &chunkStream{chunks: textScript("recovered")}, nil
	}}
	history := &recoveryHistory{}
	seed := []rig.Message{
		userMessage("old question"),
		rig.NewMessage(rig.RoleAssistant, "old answer"),
		userMessage("the current question"),
	}
	if err := history.Append(context.Background(), "s1", seed...); err != nil {
		t.Fatalf("seed: %v", err)
	}
	compactor := &keepLastCompactor{}
	var archived int
	var reported struct {
		before, after int
		err           error
	}
	a := mustNewAgent(t, rig.AgentSpec{
		Model:         "m",
		Provider:      provider,
		History:       history,
		Compactor:     compactor,
		ContextBudget: 1000,
		ContextRecovery: &rig.ContextRecovery{
			BeforeReplace: func(_ context.Context, _ string, messages, compacted []rig.Message) error {
				archived = len(messages)
				if len(compacted) == 0 {
					return errors.New("compacted history is empty")
				}
				return nil
			},
			OnRecovery: func(_ context.Context, _ string, before, after int, err error) {
				reported.before, reported.after, reported.err = before, after, err
			},
		},
	})

	result, err := a.Run(context.Background(), rig.RunRequest{SessionID: "s1"})
	requireNoError(t, err)
	if callCount != 2 {
		t.Fatalf("provider calls = %d, want 2 (overflow + retry)", callCount)
	}
	if compactor.calls != 1 {
		t.Fatalf("compactor calls = %d, want 1", compactor.calls)
	}
	if archived != len(seed) {
		t.Fatalf("archive hook saw %d messages, want the raw %d", archived, len(seed))
	}
	if history.replaced != 1 {
		t.Fatalf("history replaced %d times, want 1", history.replaced)
	}
	stored, _ := history.Load(context.Background(), "s1")
	kept := false
	for _, message := range stored {
		if message.Role == rig.RoleUser && message.Text() == "the current question" {
			kept = true
		}
	}
	if !kept {
		t.Fatalf("retried history lost the current user message: %+v", stored)
	}
	if reported.err != nil || reported.before <= reported.after {
		t.Fatalf("recovery report = %+v, want a successful shrink", reported)
	}
	if !strings.Contains(lastText(result), "recovered") {
		t.Fatalf("result text = %q", lastText(result))
	}
}

func TestRunDoesNotRecoverUnrelatedErrors(t *testing.T) {
	provider := &scriptProvider{fn: func(context.Context, rig.Request) (rig.Stream, error) {
		return nil, errors.New("openai: 429 rate limit exceeded")
	}}
	history := &recoveryHistory{}
	seed := []rig.Message{userMessage("q")}
	if err := history.Append(context.Background(), "s1", seed...); err != nil {
		t.Fatalf("seed: %v", err)
	}
	compactor := &keepLastCompactor{}
	recovered := false
	a := mustNewAgent(t, rig.AgentSpec{
		Model: "m", Provider: provider, History: history, Compactor: compactor, ContextBudget: 1000,
		ContextRecovery: &rig.ContextRecovery{OnRecovery: func(context.Context, string, int, int, error) {
			recovered = true
		}},
	})
	if _, err := a.Run(context.Background(), rig.RunRequest{SessionID: "s1"}); err == nil {
		t.Fatal("rate limit error was swallowed")
	}
	if recovered || compactor.calls != 0 || history.replaced != 0 {
		t.Fatalf("unrelated error triggered recovery (reported=%v compact=%d replaced=%d)", recovered, compactor.calls, history.replaced)
	}
}

// TestRecoveryAbortsWhenArchiveFails pins fail-closed: an archive error keeps the
// raw history and the original error.
func TestRecoveryAbortsWhenArchiveFails(t *testing.T) {
	provider := &scriptProvider{fn: func(context.Context, rig.Request) (rig.Stream, error) {
		return nil, overflowError()
	}}
	history := &recoveryHistory{}
	if err := history.Append(context.Background(), "s1", userMessage("q")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	compactor := &keepLastCompactor{}
	a := mustNewAgent(t, rig.AgentSpec{
		Model: "m", Provider: provider, History: history, Compactor: compactor, ContextBudget: 1000,
		ContextRecovery: &rig.ContextRecovery{BeforeReplace: func(context.Context, string, []rig.Message, []rig.Message) error {
			return errors.New("archive disk full")
		}},
	})
	if _, err := a.Run(context.Background(), rig.RunRequest{SessionID: "s1"}); err == nil {
		t.Fatal("recovery swallowed the archive failure")
	}
	// The compaction itself may run (the archive hook needs its result), but the
	// history must stay untouched and no retry may happen.
	if history.replaced != 0 {
		t.Fatalf("failed archive still replaced the history (%d)", history.replaced)
	}
	if _, err := history.Load(context.Background(), "s1"); err != nil || len(mustLoad(t, history)) != 1 {
		t.Fatalf("failed archive changed the history: %+v", mustLoad(t, history))
	}
}

// lastText returns the last assistant text of a run result.
func lastText(result rig.RunResult) string {
	for index := len(result.Messages) - 1; index >= 0; index-- {
		if result.Messages[index].Role == rig.RoleAssistant {
			return result.Messages[index].Text()
		}
	}
	return ""
}

// mustLoad loads the recovery history or fails the test.
func mustLoad(t *testing.T, history *recoveryHistory) []rig.Message {
	t.Helper()
	messages, err := history.Load(context.Background(), "s1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return messages
}
