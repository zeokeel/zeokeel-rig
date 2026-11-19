package sqlite_test

// The three rig contracts the agent loop persists through are views on the same
// database: message history, interruption checkpoints and the session event log.
// A host hands them to agent.AgentSpec without knowing a database is there.

import (
	"context"
	"encoding/json"
	"testing"

	rig "github.com/zeokeel/zeokeel-rig"
	sqlitestore "github.com/zeokeel/zeokeel-rig/session/sqlite"
)

// TestHistoryLoadIsNilForUnknownSession pins the rig.History rule the agent
// relies on: a missing session is an empty history, not an error.
func TestHistoryLoadIsNilForUnknownSession(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	history := db.History()
	msgs, err := history.Load(context.Background(), "nobody")
	if err != nil {
		t.Fatalf("Load unknown: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("Load unknown returned %d messages", len(msgs))
	}
}

// TestHistoryAppendThenLoad covers the write-read round trip the agent loop runs
// every turn.
func TestHistoryAppendThenLoad(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	history := db.History()
	if err := history.Append(ctx, "s1", userMessage("你好"), assistantMessage("在")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	msgs, err := history.Load(ctx, "s1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(msgs) != 2 || msgs[0].Role != rig.RoleUser || msgs[1].Role != rig.RoleAssistant {
		t.Fatalf("Load = %+v", msgs)
	}
}

// TestHistoryReplaceIsAtomicFromTheReadersSide covers compaction: the host
// replaces the window and a reader sees the new one, never a mixture.
func TestHistoryReplaceIsAtomicFromTheReadersSide(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	history := db.History()
	if err := history.Append(ctx, "s1", userMessage("一"), assistantMessage("二"), userMessage("三")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	replacer, ok := history.(rig.HistoryReplacer)
	if !ok {
		t.Fatal("history does not implement rig.HistoryReplacer")
	}
	if err := replacer.Replace(ctx, "s1", []rig.Message{assistantMessage("摘要")}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	msgs, err := history.Load(ctx, "s1")
	if err != nil {
		t.Fatalf("Load after replace: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Role != rig.RoleAssistant {
		t.Fatalf("Load after replace = %+v", msgs)
	}
}

// TestCheckpointRoundTrip covers resuming an interrupted turn.
func TestCheckpointRoundTrip(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	cp := db.Checkpoints()
	if saved, err := cp.Load(ctx, "s1"); err != nil || saved != nil {
		t.Fatalf("Load before save = %v, %v", saved, err)
	}
	checkpoint := &rig.Checkpoint{Messages: []rig.Message{userMessage("续跑")}}
	if err := cp.Save(ctx, "s1", checkpoint); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := cp.Load(ctx, "s1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded == nil || len(loaded.Messages) != 1 {
		t.Fatalf("Load = %+v", loaded)
	}
	if err := cp.Clear(ctx, "s1"); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if again, err := cp.Load(ctx, "s1"); err != nil || again != nil {
		t.Fatalf("Load after clear = %v, %v", again, err)
	}
	// Clearing twice is not an error.
	if err := cp.Clear(ctx, "s1"); err != nil {
		t.Fatalf("Clear twice: %v", err)
	}
}

// TestSessionLogAppendsAndReadsBySeq covers the trajectory view.
func TestSessionLogAppendsAndReadsBySeq(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	log := db.SessionLog()
	for _, kind := range []string{"turn.start", "tool.call", "turn.end"} {
		if err := log.AppendEvent(ctx, rig.SessionEvent{
			SessionID: "s1",
			Type:      kind,
			Payload:   json.RawMessage(`{"ok":true}`),
		}); err != nil {
			t.Fatalf("AppendEvent %s: %v", kind, err)
		}
	}
	events, err := log.Events(ctx, "s1", 0, 0)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("Events = %d, want 3", len(events))
	}
	if events[0].Seq >= events[1].Seq || events[1].Seq >= events[2].Seq {
		t.Fatalf("seq not increasing: %+v", events)
	}
	if events[0].Type != "turn.start" {
		t.Fatalf("first event = %+v", events[0])
	}
	// A window is inclusive on both ends; to <= 0 means "to the end".
	window, err := log.Events(ctx, "s1", events[1].Seq, events[1].Seq)
	if err != nil || len(window) != 1 || window[0].Type != "tool.call" {
		t.Fatalf("Events window = %+v (err %v)", window, err)
	}
	last, err := log.Last(ctx, "s1", 1)
	if err != nil || len(last) != 1 || last[0].Type != "turn.end" {
		t.Fatalf("Last = %+v (err %v)", last, err)
	}
	found, err := log.Search(ctx, "tool.call", 10)
	if err != nil || len(found) != 1 {
		t.Fatalf("Search = %+v (err %v)", found, err)
	}
	if none, err := log.Search(ctx, "not-there", 10); err != nil || len(none) != 0 {
		t.Fatalf("Search miss = %+v (err %v)", none, err)
	}
}

// TestAppendEventAssignsSeq keeps a caller from having to know the sequence.
func TestAppendEventAssignsSeq(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	log := db.SessionLog()
	if err := log.AppendEvent(ctx, rig.SessionEvent{SessionID: "s1", Type: "a"}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	events, err := log.Events(ctx, "s1", 0, 0)
	if err != nil || len(events) != 1 {
		t.Fatalf("Events = %+v (err %v)", events, err)
	}
	if events[0].Seq != 1 {
		t.Fatalf("first seq = %d, want 1", events[0].Seq)
	}
	if events[0].Time.IsZero() {
		t.Fatal("event time was not filled in")
	}
}

// TestSessionLogIsPerSession keeps two conversations' trajectories apart.
func TestSessionLogIsPerSession(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	log := db.SessionLog()
	if err := log.AppendEvent(ctx, rig.SessionEvent{SessionID: "a", Type: "x"}); err != nil {
		t.Fatalf("append a: %v", err)
	}
	if err := log.AppendEvent(ctx, rig.SessionEvent{SessionID: "b", Type: "y"}); err != nil {
		t.Fatalf("append b: %v", err)
	}
	a, err := log.Events(ctx, "a", 0, 0)
	if err != nil || len(a) != 1 || a[0].Type != "x" {
		t.Fatalf("events of a = %+v (err %v)", a, err)
	}
}

// TestDeletedSessionLeavesNoCheckpoint keeps a deleted conversation from being
// resumable.
func TestDeletedSessionLeavesNoCheckpoint(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	if _, err := db.Create(ctx, sqlitestore.Meta{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	metas, err := db.List(ctx, sqlitestore.Filter{})
	if err != nil || len(metas) != 1 {
		t.Fatalf("List = %+v (err %v)", metas, err)
	}
	id := metas[0].ID
	if err := db.Checkpoints().Save(ctx, id, &rig.Checkpoint{}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := db.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if cp, err := db.Checkpoints().Load(ctx, id); err != nil {
		t.Fatalf("Load checkpoint: %v", err)
	} else if cp != nil {
		t.Fatalf("checkpoint survived delete: %+v", cp)
	}
}
