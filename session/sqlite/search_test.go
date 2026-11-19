package sqlite_test

// Searching is the reason a product wants a database rather than a directory:
// "find the sentence where I asked about the release" spans every conversation.

import (
	"context"
	"testing"

	rig "github.com/zeokeel/zeokeel-rig"
	sqlitestore "github.com/zeokeel/zeokeel-rig/session/sqlite"
)

// TestSearchSpansSessions covers the sidebar's global search.
func TestSearchSpansSessions(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	a, _ := db.Create(ctx, sqlitestore.Meta{Title: "发布"})
	b, _ := db.Create(ctx, sqlitestore.Meta{Title: "其它"})
	if _, err := db.Append(ctx, a.ID, userMessage("帮我整理发布清单")); err != nil {
		t.Fatalf("append a: %v", err)
	}
	if _, err := db.Append(ctx, b.ID, userMessage("今天天气不错")); err != nil {
		t.Fatalf("append b: %v", err)
	}
	hits, err := db.SearchMessages(ctx, sqlitestore.SearchQuery{Text: "发布"})
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %d, want 1", len(hits))
	}
	hit := hits[0]
	if hit.SessionID != a.ID || hit.Title != "发布" {
		t.Fatalf("hit = %+v", hit)
	}
	if hit.Snippet == "" || hit.Ordinal != 0 {
		t.Fatalf("snippet/ordinal = %q/%d", hit.Snippet, hit.Ordinal)
	}
}

// TestSearchIsCaseInsensitiveAndLiteral keeps LIKE wildcards from turning a
// user's underscore into "any character".
func TestSearchIsCaseInsensitiveAndLiteral(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	created, _ := db.Create(ctx, sqlitestore.Meta{})
	if _, err := db.Append(ctx, created.ID, userMessage("snake_case 命名")); err != nil {
		t.Fatalf("append: %v", err)
	}
	if hits, err := db.SearchMessages(ctx, sqlitestore.SearchQuery{Text: "SNAKE_CASE"}); err != nil || len(hits) != 1 {
		t.Fatalf("case-insensitive hit = %d (err %v)", len(hits), err)
	}
	if hits, err := db.SearchMessages(ctx, sqlitestore.SearchQuery{Text: "snakeXcase"}); err != nil || len(hits) != 0 {
		t.Fatalf("wildcard leaked: %d hits (err %v)", len(hits), err)
	}
}

// TestSearchPaginates keeps a long list from being cut at an arbitrary point.
func TestSearchPaginates(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	created, _ := db.Create(ctx, sqlitestore.Meta{})
	for i := 0; i < 5; i++ {
		if _, err := db.Append(ctx, created.ID, userMessage("关键字 命中")); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	first, err := db.SearchMessages(ctx, sqlitestore.SearchQuery{Text: "关键字", Limit: 2})
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("first page = %d", len(first))
	}
	next, err := db.SearchMessages(ctx, sqlitestore.SearchQuery{Text: "关键字", Limit: 2, AfterOrdinal: first[len(first)-1].Ordinal})
	if err != nil {
		t.Fatalf("SearchMessages page 2: %v", err)
	}
	if len(next) != 2 || next[0].Ordinal <= first[len(first)-1].Ordinal {
		t.Fatalf("page 2 = %+v", next)
	}
}

// TestSearchEventsByTypeAndTime covers the trajectory filter.
func TestSearchEventsByTypeAndTime(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	created, _ := db.Create(ctx, sqlitestore.Meta{})
	log := db.SessionLog()
	for _, kind := range []string{"tool.call", "tool.call", "turn.end"} {
		if err := log.AppendEvent(ctx, rig.SessionEvent{SessionID: created.ID, Type: kind}); err != nil {
			t.Fatalf("append event: %v", err)
		}
	}
	hits, err := db.SearchEvents(ctx, sqlitestore.EventQuery{Type: "tool.call"})
	if err != nil {
		t.Fatalf("SearchEvents: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("event hits = %d, want 2", len(hits))
	}
	if all, err := db.SearchEvents(ctx, sqlitestore.EventQuery{}); err != nil || len(all) != 3 {
		t.Fatalf("all events = %d (err %v)", len(all), err)
	}
}
