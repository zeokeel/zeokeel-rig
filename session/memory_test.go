package session_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
	"github.com/zeokeel/zeokeel-rig/session"
)

// Memory must satisfy the frozen v1.History contract.
var _ v1.History = (*session.Memory)(nil)

func TestMemoryLoadUnknownSession(t *testing.T) {
	memory := session.NewMemory()
	messages, err := memory.Load(context.Background(), "nope")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if messages != nil {
		t.Fatalf("unknown session returned %#v, want nil", messages)
	}
	if memory.Len("nope") != 0 {
		t.Fatalf("Len = %d, want 0", memory.Len("nope"))
	}
}

func TestMemoryAppendLoadRoundTrip(t *testing.T) {
	memory := session.NewMemory()
	ctx := context.Background()
	want := []v1.Message{
		v1.NewMessage(v1.RoleSystem, "sys"),
		v1.NewMessage(v1.RoleUser, "hello"),
		{Role: v1.RoleAssistant, Content: []v1.ContentPart{v1.TextPart("hi")}, ToolCalls: []v1.ToolCall{{ID: "c1", Name: "echo", Arguments: `{}`}}},
		{Role: v1.RoleTool, Content: []v1.ContentPart{v1.TextPart("echoed")}, ToolCallID: "c1", Name: "echo"},
	}
	if err := memory.Append(ctx, "s1", want[0], want[1]); err != nil {
		t.Fatalf("Append first call: %v", err)
	}
	if err := memory.Append(ctx, "s1", want[2], want[3]); err != nil {
		t.Fatalf("Append second call: %v", err)
	}
	got, err := memory.Load(ctx, "s1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("load = %d messages, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Role != want[i].Role || got[i].Text() != want[i].Text() || got[i].ToolCallID != want[i].ToolCallID {
			t.Fatalf("message[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	if memory.Len("s1") != len(want) {
		t.Fatalf("Len = %d, want %d", memory.Len("s1"), len(want))
	}
	if memory.Len("other") != 0 {
		t.Fatalf("sessions are not isolated: other has %d messages", memory.Len("other"))
	}
}

func TestMemoryLoadReturnsCopies(t *testing.T) {
	memory := session.NewMemory()
	ctx := context.Background()
	if err := memory.Append(ctx, "s1", v1.Message{
		Role:      v1.RoleAssistant,
		Content:   []v1.ContentPart{v1.TextPart("original")},
		ToolCalls: []v1.ToolCall{{ID: "c1", Name: "echo", Arguments: `{}`}},
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	first, err := memory.Load(ctx, "s1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("load = %d messages", len(first))
	}
	// Mutating the returned slice and its nested slices must not touch stored
	// history.
	first[0].Content[0].Text = "mutated"
	first[0].ToolCalls[0].Name = "mutated"
	first = append(first, v1.NewMessage(v1.RoleUser, "extra"))

	second, err := memory.Load(ctx, "s1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(second) != 1 {
		t.Fatalf("stored history grew to %d messages through returned value", len(second))
	}
	if second[0].Content[0].Text != "original" {
		t.Fatalf("stored content mutated: %q", second[0].Content[0].Text)
	}
	if second[0].ToolCalls[0].Name != "echo" {
		t.Fatalf("stored tool call mutated: %q", second[0].ToolCalls[0].Name)
	}
}

func TestMemoryAppendCopiesInput(t *testing.T) {
	memory := session.NewMemory()
	ctx := context.Background()
	message := v1.Message{
		Role:      v1.RoleAssistant,
		Content:   []v1.ContentPart{v1.TextPart("original")},
		ToolCalls: []v1.ToolCall{{ID: "c1", Name: "echo", Arguments: `{}`}},
	}
	if err := memory.Append(ctx, "s1", message); err != nil {
		t.Fatalf("Append: %v", err)
	}
	message.Content[0].Text = "changed after append"
	message.ToolCalls[0].Arguments = `{"changed":true}`

	stored, err := memory.Load(ctx, "s1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if stored[0].Content[0].Text != "original" || stored[0].ToolCalls[0].Arguments != `{}` {
		t.Fatalf("stored message aliased the caller's slices: %+v", stored[0])
	}
}

func TestMemoryConcurrentAccess(t *testing.T) {
	memory := session.NewMemory()
	ctx := context.Background()
	const writers = 8
	const perWriter = 50

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				message := v1.NewMessage(v1.RoleUser, "message")
				if err := memory.Append(ctx, "shared", message); err != nil {
					t.Errorf("Append: %v", err)
					return
				}
				if _, err := memory.Load(ctx, "shared"); err != nil {
					t.Errorf("Load: %v", err)
					return
				}
				if n := memory.Len("shared"); n < 0 {
					t.Errorf("impossible length %d", n)
				}
			}
		}(w)
	}
	wg.Wait()
	if got := memory.Len("shared"); got != writers*perWriter {
		t.Fatalf("Len = %d, want %d", got, writers*perWriter)
	}
}

// TestMemoryAppendBatchIsAtomic verifies that concurrent readers never observe
// half of one Append call's batch.
func TestMemoryAppendBatchIsAtomic(t *testing.T) {
	memory := session.NewMemory()
	ctx := context.Background()
	const batches = 200

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < batches; i++ {
			if err := memory.Append(ctx, "s1",
				v1.NewMessage(v1.RoleUser, "u"),
				v1.NewMessage(v1.RoleAssistant, "a")); err != nil {
				t.Errorf("Append: %v", err)
				return
			}
		}
	}()

	var (
		reads  int
		last   int
		closed bool
	)
	for !closed {
		select {
		case <-done:
			closed = true
		default:
		}
		messages, err := memory.Load(ctx, "s1")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		reads++
		last = len(messages)
		if last%2 != 0 {
			t.Fatalf("reader observed a partial batch: %d messages", last)
		}
	}
	if reads == 0 {
		t.Fatal("no concurrent reads happened")
	}
	if final := memory.Len("s1"); final != batches*2 {
		t.Fatalf("Len = %d, want %d", final, batches*2)
	}
}

func TestMemoryContextCancellation(t *testing.T) {
	memory := session.NewMemory()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := memory.Load(ctx, "s1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Load error = %v, want context.Canceled", err)
	}
	if err := memory.Append(ctx, "s1", v1.NewMessage(v1.RoleUser, "x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("Append error = %v, want context.Canceled", err)
	}
	if memory.Len("s1") != 0 {
		t.Fatalf("cancelled Append stored messages")
	}
}

func TestMemoryEmptySessionID(t *testing.T) {
	memory := session.NewMemory()
	ctx := context.Background()
	if err := memory.Append(ctx, "", v1.NewMessage(v1.RoleUser, "x")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	messages, err := memory.Load(ctx, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if memory.Len("") != 1 || len(messages) != 1 {
		t.Fatalf("empty session ID not stored: len=%d load=%d", memory.Len(""), len(messages))
	}
}

func TestMemoryAppendNoMessages(t *testing.T) {
	memory := session.NewMemory()
	if err := memory.Append(context.Background(), "s1"); err != nil {
		t.Fatalf("Append with no messages: %v", err)
	}
	if memory.Len("s1") != 0 {
		t.Fatalf("Len = %d, want 0", memory.Len("s1"))
	}
}

func TestMemoryNilContextIsTolerated(t *testing.T) {
	// v1.History always passes a real context; the implementation still guards
	// nil so a sloppy host cannot panic it.
	memory := session.NewMemory()
	if _, err := memory.Load(nil, "s1"); err != nil { //nolint:staticcheck // intentional nil context
		t.Fatalf("Load(nil): %v", err)
	}
	if err := memory.Append(nil, "s1", v1.NewMessage(v1.RoleUser, "x")); err != nil { //nolint:staticcheck // intentional nil context
		t.Fatalf("Append(nil): %v", err)
	}
	if memory.Len("s1") != 1 {
		t.Fatalf("Len = %d, want 1", memory.Len("s1"))
	}
}
