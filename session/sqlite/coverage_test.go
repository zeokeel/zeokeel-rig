package sqlite_test

// The remaining edges: what a host meets when it closes a store, when a write
// races another writer, and when it asks the store for things the happy path
// never touches.

import (
	"context"
	"errors"
	"testing"
	"time"

	rig "github.com/zeokeel/zeokeel-rig"
	sqlitestore "github.com/zeokeel/zeokeel-rig/session/sqlite"
	"github.com/zeokeel/zeokeel-rig/session/store"
)

// TestViewsReportClosed keeps a late UI refresh from panicking after shutdown.
func TestViewsReportClosed(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ctx := context.Background()
	if _, err := db.History().Load(ctx, "s"); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("History.Load after close = %v", err)
	}
	if err := db.History().Append(ctx, "s", userMessage("x")); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("History.Append after close = %v", err)
	}
	if _, err := db.Checkpoints().Load(ctx, "s"); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("Checkpoints.Load after close = %v", err)
	}
	if err := db.SessionLog().AppendEvent(ctx, rig.SessionEvent{SessionID: "s", Type: "t"}); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("AppendEvent after close = %v", err)
	}
	if _, err := db.SearchMessages(ctx, sqlitestore.SearchQuery{Text: "x"}); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("SearchMessages after close = %v", err)
	}
	if err := db.SaveCost(ctx, "s", sqlitestore.CostEntry{}); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("SaveCost after close = %v", err)
	}
}

// TestTwoStoresOnOneFile covers the desktop case: a window and a background run
// open the same database. The second writer waits instead of failing.
func TestTwoStoresOnOneFile(t *testing.T) {
	_, path := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	first, err := sqlitestore.Open(sqlitestore.Options{Path: path})
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	defer func() { _ = first.Close() }()
	second, err := sqlitestore.Open(sqlitestore.Options{Path: path})
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer func() { _ = second.Close() }()
	created, err := first.Create(ctx, sqlitestore.Meta{Title: "两个进程"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := second.Append(ctx, created.ID, userMessage("从第二个连接写")); err != nil {
		t.Fatalf("Append from second store: %v", err)
	}
	if _, messages, err := first.Load(ctx, created.ID); err != nil || len(messages) != 1 {
		t.Fatalf("first store sees %d messages (err %v)", len(messages), err)
	}
}

// TestEventRejectsIncompleteRecords catches the two mistakes a host makes when
// it builds an event by hand.
func TestEventRejectsIncompleteRecords(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	log := db.SessionLog()
	if err := log.AppendEvent(context.Background(), rig.SessionEvent{Type: "x"}); err == nil {
		t.Fatal("event without a session id: want an error, got none")
	}
	if err := log.AppendEvent(context.Background(), rig.SessionEvent{SessionID: "s"}); err == nil {
		t.Fatal("event without a type: want an error, got none")
	}
}

// TestSearchEmptyQueryIsEmpty keeps a blank search box from listing everything.
func TestSearchEmptyQueryIsEmpty(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	if hits, err := db.SearchMessages(context.Background(), sqlitestore.SearchQuery{Text: "   "}); err != nil || len(hits) != 0 {
		t.Fatalf("blank search = %d hits (err %v)", len(hits), err)
	}
}

// TestDSNAndDBExposeTheHandle covers the escape hatch a host uses for a query
// the store does not offer.
func TestDSNAndDBExposeTheHandle(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	if db.DSN() == "" {
		t.Fatal("DSN is empty")
	}
	if db.DB() == nil {
		t.Fatal("DB is nil")
	}
	var one int
	if err := db.DB().QueryRow(`SELECT 1`).Scan(&one); err != nil || one != 1 {
		t.Fatalf("query through DB = %d (err %v)", one, err)
	}
}

// TestSchemaListsAppliedExtensions covers the health check a host runs once at
// startup.
func TestSchemaListsAppliedExtensions(t *testing.T) {
	store, _ := openTemp(t, sqlitestore.Options{Extensions: []sqlitestore.Extension{{
		Name:       "example/notes",
		Statements: []string{`CREATE TABLE IF NOT EXISTS example_notes (id INTEGER PRIMARY KEY)`},
	}}})
	schema := schemaOf(t, store)
	if len(schema.Extensions) != 1 || schema.Extensions[0] != "example/notes" {
		t.Fatalf("extensions = %v", schema.Extensions)
	}
}

// TestOpenMakesTheFile is the one side effect of Open the host must be able to
// rely on: the directory exists, so the file is created even before a write.
func TestOpenMakesTheFile(t *testing.T) {
	store, path := openTemp(t, sqlitestore.Options{})
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if path == "" {
		t.Fatal("path is empty")
	}
	if _, err := sqlitestore.Open(sqlitestore.Options{Path: path, BusyTimeout: 2 * time.Second}); err != nil {
		t.Fatalf("reopen with a busy timeout: %v", err)
	}
}
