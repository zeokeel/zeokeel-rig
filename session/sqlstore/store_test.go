package sqlstore_test

// The store is the durable half of a conversation: session rows and metadata,
// message history, the event log, checkpoints and cross-session search. These
// tests run on a real SQLite database (modernc driver) because the contract is
// the SQL, not an interface.

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	rig "github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/session/sqlstore"

	_ "modernc.org/sqlite"
)

func openTestStore(t *testing.T) *sqlstore.Store {
	t.Helper()
	db, err := sql.Open("sqlite", "file::memory:?cache=shared&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	store, err := sqlstore.New(context.Background(), db, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return store
}

func TestSessionLifecycle(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if err := store.CreateSession(ctx, "s1", "First"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := store.CreateSession(ctx, "s1", "First"); err != nil {
		t.Fatalf("CreateSession is not idempotent: %v", err)
	}
	if err := store.RenameSession(ctx, "s1", "Renamed"); err != nil {
		t.Fatalf("RenameSession: %v", err)
	}
	if err := store.RenameSession(ctx, "missing", "x"); err == nil {
		t.Fatal("RenameSession on an unknown session succeeded")
	}
	if err := store.SetSessionWorkspace(ctx, "s1", "ws-1"); err != nil {
		t.Fatalf("SetSessionWorkspace: %v", err)
	}
	meta, found, err := store.GetSessionMeta(ctx, "s1")
	if err != nil || !found {
		t.Fatalf("GetSessionMeta = %+v/%v/%v", meta, found, err)
	}
	if meta.Title != "Renamed" || meta.WorkspaceID != "ws-1" {
		t.Fatalf("meta = %+v", meta)
	}
	sessions, err := store.Sessions(ctx)
	if err != nil || len(sessions) != 1 || sessions[0].ID != "s1" {
		t.Fatalf("Sessions = %+v/%v", sessions, err)
	}
	if err := store.DeleteSession(ctx, "s1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, found, _ := store.GetSessionMeta(ctx, "s1"); found {
		t.Fatal("deleted session still listed")
	}
	// Unknown deletes are no-ops.
	if err := store.DeleteSession(ctx, "s1"); err != nil {
		t.Fatalf("DeleteSession(unknown): %v", err)
	}
}

func TestHistoryAppendLoadReplace(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	history := store.History()
	user := rig.NewMessage(rig.RoleUser, "hello")
	answer := rig.NewMessage(rig.RoleAssistant, "hi")
	if err := history.Append(ctx, "s1", user, answer); err != nil {
		t.Fatalf("Append: %v", err)
	}
	loaded, err := history.Load(ctx, "s1")
	if err != nil || len(loaded) != 2 || loaded[0].Text() != "hello" {
		t.Fatalf("Load = %+v/%v", loaded, err)
	}
	replacer, ok := history.(rig.HistoryReplacer)
	if !ok {
		t.Fatal("history is not replaceable")
	}
	if err := replacer.Replace(ctx, "s1", []rig.Message{answer}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	loaded, _ = history.Load(ctx, "s1")
	if len(loaded) != 1 || loaded[0].Text() != "hi" {
		t.Fatalf("replaced history = %+v", loaded)
	}
	// Unknown sessions load as empty, not as an error.
	if unknown, err := history.Load(ctx, "nope"); err != nil || len(unknown) != 0 {
		t.Fatalf("unknown Load = %+v/%v", unknown, err)
	}
}

func TestCheckpointsAndEvents(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	checkpoints := store.CheckpointStore()
	checkpoint := &rig.Checkpoint{SessionID: "s1", TurnID: "t1", Phase: rig.CheckpointPhaseRequested, CreatedAt: time.Now()}
	if err := checkpoints.Save(ctx, "s1", checkpoint); err != nil {
		t.Fatalf("Save checkpoint: %v", err)
	}
	loaded, err := checkpoints.Load(ctx, "s1")
	if err != nil || loaded == nil || loaded.TurnID != "t1" {
		t.Fatalf("Load checkpoint = %+v/%v", loaded, err)
	}
	if err := checkpoints.Clear(ctx, "s1"); err != nil {
		t.Fatalf("Clear checkpoint: %v", err)
	}
	if loaded, _ := checkpoints.Load(ctx, "s1"); loaded != nil {
		t.Fatalf("checkpoint survived clear: %+v", loaded)
	}

	for index := 0; index < 3; index++ {
		if err := store.AppendEvent(ctx, rig.SessionEvent{
			SessionID: "s1", Type: "run_start", Payload: []byte(`{"n":1}`),
		}); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}
	events, err := store.Events(ctx, "s1", 0, 0)
	if err != nil || len(events) != 3 {
		t.Fatalf("Events = %d/%v", len(events), err)
	}
	last, err := store.Last(ctx, "s1", 2)
	if err != nil || len(last) != 2 {
		t.Fatalf("Last = %d/%v", len(last), err)
	}
}

func TestSearchMessagesDecodesJSON(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	message := rig.NewMessage(rig.RoleUser, `find "quoted" text`)
	message.ToolCalls = []rig.ToolCall{{ID: "c1", Name: "write_file", Arguments: `{"path":"docs/a.md"}`}}
	if err := store.History().Append(ctx, "s1", message); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := store.CreateSession(ctx, "s1", "Search session"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := store.RenameSession(ctx, "s1", "Search session"); err != nil {
		t.Fatalf("RenameSession: %v", err)
	}
	hits, err := store.SearchMessages(ctx, `"quoted"`, 10)
	if err != nil || len(hits) != 1 {
		t.Fatalf("SearchMessages = %+v/%v", hits, err)
	}
	if hits[0].SessionTitle != "Search session" || hits[0].Ordinal != 1 || !strings.Contains(hits[0].Snippet, "quoted") {
		t.Fatalf("hit = %+v", hits[0])
	}
	// Tool-call argument values are searchable without JSON escaping.
	if hits, err := store.SearchMessages(ctx, "docs/a.md", 10); err != nil || len(hits) != 1 {
		t.Fatalf("argument search = %+v/%v", hits, err)
	}
	// LIKE wildcards are literal.
	if hits, err := store.SearchMessages(ctx, "%", 10); err != nil || len(hits) != 0 {
		t.Fatalf("wildcard search = %+v/%v", hits, err)
	}
	if hits, err := store.SearchMessages(ctx, "   ", 10); err != nil || len(hits) != 0 {
		t.Fatalf("blank search = %+v/%v", hits, err)
	}
}
