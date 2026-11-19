package sqlite_test

// The last few percent: the branches a host reaches on a bad day rather than in
// the happy path.

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	rig "github.com/zeokeel/zeokeel-rig"
	sqlitestore "github.com/zeokeel/zeokeel-rig/session/sqlite"
	"github.com/zeokeel/zeokeel-rig/session/store"
)

// TestAppendCreatesTheSession covers a run that starts before the UI created the
// conversation row.
func TestAppendCreatesTheSession(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	meta, err := db.Append(context.Background(), "made-up-id", userMessage("从零开始"))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if meta.ID != "made-up-id" || meta.Messages != 1 {
		t.Fatalf("meta = %+v", meta)
	}
	if meta.Title != "从零开始" {
		t.Fatalf("title = %q", meta.Title)
	}
}

// TestTitleIsCutOnRunes keeps a long opening message from becoming the sidebar.
func TestTitleIsCutOnRunes(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	long := strings.Repeat("发", sqlitestore.MaxTitleRunes+40)
	meta, err := db.Append(context.Background(), "long", userMessage(long))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if got := len([]rune(meta.Title)); got != sqlitestore.MaxTitleRunes {
		t.Fatalf("title runes = %d, want %d", got, sqlitestore.MaxTitleRunes)
	}
}

// TestAssistantOnlyFirstBatchKeepsTheTitleEmpty covers a turn that opens with
// the model rather than the user.
func TestAssistantOnlyFirstBatchKeepsTheTitleEmpty(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	meta, err := db.Append(context.Background(), "assistant-first", assistantMessage("我先说"))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if meta.Title != "" {
		t.Fatalf("title = %q, want empty", meta.Title)
	}
}

// TestCreateKeepsExplicitTimestamps covers the importer's use: a migrated
// session must keep the dates it had.
func TestCreateKeepsExplicitTimestamps(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	created := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	meta, err := db.Create(context.Background(), sqlitestore.Meta{ID: "kept", CreatedAt: created, UpdatedAt: created})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !meta.CreatedAt.Equal(created) {
		t.Fatalf("created = %v, want %v", meta.CreatedAt, created)
	}
}

// TestEventSearchBounds covers the until/text filters the trajectory uses.
func TestEventSearchBounds(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	log := db.SessionLog()
	if err := log.AppendEvent(ctx, rigEvent("s", "tool.call", `{"path":"a"}`)); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := log.AppendEvent(ctx, rigEvent("s", "tool.call", `{"path":"b"}`)); err != nil {
		t.Fatalf("append: %v", err)
	}
	hits, err := db.SearchEvents(ctx, sqlitestore.EventQuery{Text: `"a"`})
	if err != nil || len(hits) != 1 {
		t.Fatalf("text filter = %d (err %v)", len(hits), err)
	}
	if none, err := db.SearchEvents(ctx, sqlitestore.EventQuery{Until: time.Unix(1, 0)}); err != nil || len(none) != 0 {
		t.Fatalf("until filter = %d (err %v)", len(none), err)
	}
}

// TestCostTotalsUntil covers the second half of the date window.
func TestCostTotalsUntil(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	if err := db.SaveCost(ctx, "s", sqlitestore.CostEntry{Model: "m", CostUSD: 1}); err != nil {
		t.Fatalf("SaveCost: %v", err)
	}
	if none, err := db.CostTotals(ctx, sqlitestore.CostFilter{Until: time.Unix(1, 0)}); err != nil || none.Requests != 0 {
		t.Fatalf("until filter = %+v (err %v)", none, err)
	}
}

// TestImportMissingDirectoryReportsAnError keeps a typo in the path visible.
func TestImportMissingDirectoryReportsAnError(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	if _, err := sqlitestore.ImportDir(context.Background(), db, sqlitestore.ImportOptions{Dir: filepath.Join(t.TempDir(), "nope")}); err == nil {
		t.Fatal("import of a missing directory: want an error, got none")
	}
	if _, err := sqlitestore.ImportDir(context.Background(), db, sqlitestore.ImportOptions{}); err == nil {
		t.Fatal("import without a directory: want an error, got none")
	}
}

// TestSnippetKeepsContext covers a long message: the snippet shows the match and
// ellipses on both sides.
func TestSnippetKeepsContext(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	long := strings.Repeat("前缀", 40) + "关键字" + strings.Repeat("后缀", 40)
	if _, err := db.Append(ctx, "s", userMessage(long)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	hits, err := db.SearchMessages(ctx, sqlitestore.SearchQuery{Text: "关键字"})
	if err != nil || len(hits) != 1 {
		t.Fatalf("hits = %d (err %v)", len(hits), err)
	}
	if !strings.Contains(hits[0].Snippet, "关键字") {
		t.Fatalf("snippet = %q", hits[0].Snippet)
	}
	if !strings.HasPrefix(hits[0].Snippet, "…") || !strings.HasSuffix(hits[0].Snippet, "…") {
		t.Fatalf("snippet not trimmed at both ends: %q", hits[0].Snippet)
	}
}

// TestSearchNarrowsToOneSession covers the per-conversation search box.
func TestSearchNarrowsToOneSession(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	for _, id := range []string{"a", "b"} {
		if _, err := db.Append(ctx, id, userMessage("同样的词")); err != nil {
			t.Fatalf("append %s: %v", id, err)
		}
	}
	hits, err := db.SearchMessages(ctx, sqlitestore.SearchQuery{Text: "同样的词", SessionID: "a"})
	if err != nil || len(hits) != 1 || hits[0].SessionID != "a" {
		t.Fatalf("scoped hits = %+v (err %v)", hits, err)
	}
}

// TestContractAliasesMatch covers the promise that a host can switch
// implementations without changing its error checks.
func TestContractAliasesMatch(t *testing.T) {
	if !errorsIs(sqlitestore.ErrNotFound, store.ErrNotFound) {
		t.Fatal("ErrNotFound is not the store's")
	}
	if !errorsIs(sqlitestore.ErrClosed, store.ErrClosed) {
		t.Fatal("ErrClosed is not the store's")
	}
	var _ store.Store = (*sqlitestore.Store)(nil)
}

// rigEvent builds an event with a payload.
func rigEvent(session, kind, payload string) rig.SessionEvent {
	return rig.SessionEvent{SessionID: session, Type: kind, Payload: json.RawMessage(payload)}
}

func errorsIs(err, target error) bool { return errors.Is(err, target) }
