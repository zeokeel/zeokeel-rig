package session

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
)

func TestMemoryLogAppendAndSessions(t *testing.T) {
	log := NewMemoryLog()
	first := log.Append(v1.SessionEvent{SessionID: "s1", Type: "user_message", Payload: json.RawMessage(`{"text":"hi"}`)})
	if first.Seq != 1 {
		t.Fatalf("first seq = %d, want 1", first.Seq)
	}
	second := log.Append(v1.SessionEvent{SessionID: "s2", Type: "tool_call"})
	if second.Seq != 2 {
		t.Fatalf("second seq = %d, want 2", second.Seq)
	}
	explicit := log.Append(v1.SessionEvent{Seq: 10, SessionID: "s1", Type: "tool_result"})
	if explicit.Seq != 10 {
		t.Fatalf("explicit seq = %d, want 10", explicit.Seq)
	}
	next := log.Append(v1.SessionEvent{SessionID: "s1", Type: "message"})
	if next.Seq != 11 {
		t.Fatalf("seq after explicit = %d, want 11", next.Seq)
	}
	if log.Len() != 4 {
		t.Fatalf("Len = %d, want 4", log.Len())
	}
	ids, err := log.Sessions(context.Background())
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if len(ids) != 2 || ids[0] != "s1" || ids[1] != "s2" {
		t.Fatalf("Sessions = %v, want [s1 s2]", ids)
	}

	// Append copies the payload, so the caller may reuse its slice.
	payload := json.RawMessage(`{"x":1}`)
	log.Append(v1.SessionEvent{SessionID: "s1", Type: "copy", Payload: payload})
	payload[0] = '!'
	events, err := log.Events(context.Background(), "s1", 0, 0)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	for _, event := range events {
		if event.Type != "copy" {
			continue
		}
		if string(event.Payload) != `{"x":1}` {
			t.Fatalf("stored payload mutated: %s", event.Payload)
		}
	}
}

func TestMemoryLogEvents(t *testing.T) {
	log := populatedLog(t)
	events, err := log.Events(context.Background(), "s1", 0, 0)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 4 {
		t.Fatalf("s1 events = %d, want 4", len(events))
	}
	fromTo, err := log.Events(context.Background(), "s1", 2, 3)
	if err != nil {
		t.Fatalf("Events(range): %v", err)
	}
	if len(fromTo) != 2 || fromTo[0].Seq != 2 || fromTo[1].Seq != 3 {
		t.Fatalf("Events(2..3) = %+v", fromTo)
	}
	all, err := log.Events(context.Background(), "", 0, 0)
	if err != nil {
		t.Fatalf("Events(all): %v", err)
	}
	if len(all) != 6 {
		t.Fatalf("all events = %d, want 6", len(all))
	}
	unknown, err := log.Events(context.Background(), "ghost", 0, 0)
	if err != nil || len(unknown) != 0 {
		t.Fatalf("Events(unknown) = %v, %v", unknown, err)
	}
	// Results are copies.
	all[0].Payload[0] = '!'
	again, _ := log.Events(context.Background(), "s1", 1, 1)
	if len(again) != 1 || string(again[0].Payload) == "" || again[0].Payload[0] == '!' {
		t.Fatalf("stored event mutated through result copy: %s", again[0].Payload)
	}
}

func TestMemoryLogSearch(t *testing.T) {
	log := populatedLog(t)
	cases := []struct {
		query string
		want  int
	}{
		{query: "boom", want: 1},
		{query: "TOOL_CALL", want: 2},
		{query: "read_file", want: 1},
		{query: "s2", want: 2},
		{query: "nothing-matches", want: 0},
		{query: "", want: 6},
	}
	for _, test := range cases {
		events, err := log.Search(context.Background(), test.query, 0)
		if err != nil {
			t.Fatalf("Search(%q): %v", test.query, err)
		}
		if len(events) != test.want {
			t.Fatalf("Search(%q) = %d events, want %d", test.query, len(events), test.want)
		}
	}
	limited, err := log.Search(context.Background(), "", 2)
	if err != nil || len(limited) != 2 {
		t.Fatalf("Search(limit 2) = %d, %v", len(limited), err)
	}
}

func TestMemoryLogLast(t *testing.T) {
	log := populatedLog(t)
	if _, err := log.Last(context.Background(), "", 5); err == nil {
		t.Fatal("Last(blank session) succeeded")
	}
	last, err := log.Last(context.Background(), "s1", 2)
	if err != nil {
		t.Fatalf("Last: %v", err)
	}
	if len(last) != 2 || last[0].Seq != 3 || last[1].Seq != 4 {
		t.Fatalf("Last(s1, 2) = %+v, want seq 3,4", last)
	}
	all, err := log.Last(context.Background(), "s1", 0)
	if err != nil || len(all) != 4 {
		t.Fatalf("Last(s1, 0) = %d, %v", len(all), err)
	}
}

func TestMemoryLogContextCancellation(t *testing.T) {
	log := populatedLog(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := log.Events(ctx, "s1", 0, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("Events(cancelled) = %v", err)
	}
	if _, err := log.Search(ctx, "", 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("Search(cancelled) = %v", err)
	}
	if _, err := log.Last(ctx, "s1", 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("Last(cancelled) = %v", err)
	}
	if _, err := log.Sessions(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Sessions(cancelled) = %v", err)
	}
}

func TestMemoryLogConcurrent(t *testing.T) {
	log := NewMemoryLog()
	var group sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			for round := 0; round < 20; round++ {
				log.Append(v1.SessionEvent{SessionID: "s1", Type: "tool_call"})
				_, _ = log.Search(context.Background(), "tool", 10)
				_, _ = log.Events(context.Background(), "s1", 0, 0)
				_, _ = log.Last(context.Background(), "s1", 5)
				_, _ = log.Sessions(context.Background())
			}
		}(worker)
	}
	group.Wait()
	if got := log.Len(); got != 160 {
		t.Fatalf("Len = %d, want 160", got)
	}
}

// populatedLog has four s1 events (messages, a tool call and an error) and two
// s2 events.
func populatedLog(t *testing.T) *MemoryLog {
	t.Helper()
	log := NewMemoryLog()
	stamp := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	log.Append(v1.SessionEvent{SessionID: "s1", Type: "user_message", Time: stamp, Payload: json.RawMessage(`{"text":"hello"}`)})
	log.Append(v1.SessionEvent{SessionID: "s1", Type: "tool_call", Time: stamp, Payload: json.RawMessage(`{"tool":"read_file","path":"a.go"}`)})
	log.Append(v1.SessionEvent{SessionID: "s1", Type: "tool_result", Payload: json.RawMessage(`{"ok":true}`)})
	log.Append(v1.SessionEvent{SessionID: "s1", Type: "error", Payload: json.RawMessage(`{"message":"boom"}`)})
	log.Append(v1.SessionEvent{SessionID: "s2", Type: "user_message", Payload: json.RawMessage(`{"text":"other"}`)})
	log.Append(v1.SessionEvent{SessionID: "s2", Type: "tool_call", Payload: json.RawMessage(`{"tool":"bash","command":"ls"}`)})
	return log
}
