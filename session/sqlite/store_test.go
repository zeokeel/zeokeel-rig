package sqlite_test

// Conformance for the session store contract: the same behaviour a host gets
// from the directory store (session/store), implemented over one database.

import (
	"context"
	"errors"
	"testing"
	"time"

	rig "github.com/zeokeel/zeokeel-rig"
	sqlitestore "github.com/zeokeel/zeokeel-rig/session/sqlite"
	"github.com/zeokeel/zeokeel-rig/session/store"
)

func userMessage(text string) rig.Message {
	return rig.Message{Role: rig.RoleUser, Content: []rig.ContentPart{rig.TextPart(text)}}
}

func assistantMessage(text string) rig.Message {
	return rig.Message{Role: rig.RoleAssistant, Content: []rig.ContentPart{rig.TextPart(text)}}
}

// TestCreateGeneratesAnIDAndTimestamps covers the one call every host makes
// first: no ID given, no title given, and the returned Meta is what the sidebar
// lists.
func TestCreateGeneratesAnIDAndTimestamps(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	meta, err := db.Create(context.Background(), sqlitestore.Meta{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if meta.ID == "" {
		t.Fatal("Create returned an empty ID")
	}
	if meta.CreatedAt.IsZero() || meta.UpdatedAt.IsZero() {
		t.Fatalf("timestamps missing: %+v", meta)
	}
}

// TestAppendFillsTheTitleFromTheFirstUserMessage covers the sidebar title the
// directory store derives by reading the file head: here it is a column.
func TestAppendFillsTheTitleFromTheFirstUserMessage(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	meta, err := db.Create(ctx, sqlitestore.Meta{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	updated, err := db.Append(ctx, meta.ID, userMessage("  把发布清单整理一下  "))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if updated.Title != "把发布清单整理一下" {
		t.Fatalf("title = %q, want the trimmed first user message", updated.Title)
	}
	if updated.Messages != 1 {
		t.Fatalf("messages = %d, want 1", updated.Messages)
	}
	// A second user message must not overwrite the title.
	again, err := db.Append(ctx, meta.ID, userMessage("第二条"))
	if err != nil {
		t.Fatalf("Append second: %v", err)
	}
	if again.Title != "把发布清单整理一下" {
		t.Fatalf("title after second message = %q", again.Title)
	}
}

// TestLoadReturnsMessagesInOrder pins the contract rig.History depends on.
func TestLoadReturnsMessagesInOrder(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	meta, _ := db.Create(ctx, sqlitestore.Meta{Title: "顺序"})
	if _, err := db.AppendBatch(ctx, meta.ID, userMessage("一"), assistantMessage("二"), userMessage("三")); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	loaded, messages, err := db.Load(ctx, meta.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(messages) != 3 {
		t.Fatalf("loaded %d messages, want 3", len(messages))
	}
	if messages[0].Content[0].Text != "一" || messages[2].Content[0].Text != "三" {
		t.Fatalf("messages out of order: %+v", messages)
	}
	if loaded.Messages != 3 {
		t.Fatalf("meta.Messages = %d, want 3", loaded.Messages)
	}
}

// TestLoadUnknownSessionReportsNotFound keeps the error a UI can branch on.
func TestLoadUnknownSessionReportsNotFound(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	if _, _, err := db.Load(context.Background(), "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Load unknown = %v, want ErrNotFound", err)
	}
}

// TestListOrdersNewestFirstWithFilters covers the sidebar query.
func TestListOrdersNewestFirstWithFilters(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	older, _ := db.Create(ctx, sqlitestore.Meta{Title: "旧", Workspace: "a"})
	newer, _ := db.Create(ctx, sqlitestore.Meta{Title: "新", Workspace: "b"})
	if _, err := db.AppendBatch(ctx, newer.ID, userMessage("最近")); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	time.Sleep(2 * time.Millisecond) // the two creations differ anyway; be explicit
	metas, err := db.List(ctx, sqlitestore.Filter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(metas) != 2 || metas[0].ID != newer.ID {
		t.Fatalf("List order = %+v, want the updated session first", metas)
	}
	if filtered, err := db.List(ctx, sqlitestore.Filter{Workspace: "a"}); err != nil || len(filtered) != 1 || filtered[0].ID != older.ID {
		t.Fatalf("List by workspace = %+v (err %v)", filtered, err)
	}
	if limited, err := db.List(ctx, sqlitestore.Filter{Limit: 1}); err != nil || len(limited) != 1 {
		t.Fatalf("List limit = %+v (err %v)", limited, err)
	}
	if none, err := db.List(ctx, sqlitestore.Filter{Since: time.Now().Add(time.Hour)}); err != nil || len(none) != 0 {
		t.Fatalf("List since future = %+v (err %v)", none, err)
	}
}

// TestDeleteCascades covers "delete the conversation" leaving nothing behind.
func TestDeleteCascades(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	meta, _ := db.Create(ctx, sqlitestore.Meta{})
	if _, err := db.AppendBatch(ctx, meta.ID, userMessage("x")); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if err := db.Delete(ctx, meta.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, _, err := db.Load(ctx, meta.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Load after delete = %v, want ErrNotFound", err)
	}
	if metas, _ := db.List(ctx, sqlitestore.Filter{}); len(metas) != 0 {
		t.Fatalf("List after delete = %+v", metas)
	}
}

// TestCallsAfterCloseReportClosed keeps a late writer from panicking.
func TestCallsAfterCloseReportClosed(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := db.Create(context.Background(), sqlitestore.Meta{}); err == nil {
		t.Fatal("Create after Close: want an error, got none")
	}
}
