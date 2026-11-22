package sqlite

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/memoryvfs"
)

func TestStoreLifecycleAndSessions(t *testing.T) {
	ctx := context.Background()
	store := openTest(t, "ws")
	uri := memoryvfs.URI("mem://user/self/memories/preferences/theme")
	entry := memoryvfs.Entry{URI: uri, Kind: memoryvfs.KindMemory, Type: "preferences", Summary: "dark mode", Content: "prefers dark"}
	if _, err := store.Write(ctx, entry); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got, err := store.Stat(ctx, uri); err != nil || got.Summary != "dark mode" {
		t.Fatalf("Stat = %+v/%v", got, err)
	}
	for _, level := range []memoryvfs.Level{memoryvfs.Level0, memoryvfs.Level1, memoryvfs.Level2} {
		if _, err := store.Read(ctx, uri, level); err != nil {
			t.Fatalf("Read(%v): %v", level, err)
		}
	}
	if _, err := store.Append(ctx, uri, "\nmore"); err != nil {
		t.Fatalf("Append: %v", err)
	}
	dir := memoryvfs.URI("mem://user/self/memories/preferences")
	if _, err := store.Mkdir(ctx, dir, "prefs"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if list, err := store.List(ctx, dir); err != nil || len(list) == 0 {
		t.Fatalf("List = %+v/%v", list, err)
	}
	if tree, err := store.Tree(ctx, memoryvfs.RootURI, 3); err != nil || len(tree) == 0 {
		t.Fatalf("Tree = %+v/%v", tree, err)
	}
	moved := memoryvfs.URI("mem://user/self/memories/preferences/theme2")
	if err := store.Move(ctx, uri, moved); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if _, err := store.Remember(ctx, memoryvfs.Entry{URI: memoryvfs.URI("mem://user/self/memories/profile/name"), Kind: memoryvfs.KindMemory, Type: "profile", Summary: "name"}); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	if err := store.Forget(ctx, moved); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if result, err := store.Search(ctx, memoryvfs.Query{Text: "name"}); err != nil || len(result.Entries) == 0 {
		t.Fatalf("Search = %+v/%v", result, err)
	}
}

func TestSnapshotsRulesExportAndMaintenance(t *testing.T) {
	ctx := context.Background()
	store := openTest(t, "ws")
	uri := memoryvfs.URI("mem://user/self/memories/profile/name")
	if _, err := store.Write(ctx, memoryvfs.Entry{URI: uri, Kind: memoryvfs.KindMemory, Type: "profile", Summary: "name"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Capture(ctx, "sess-1", memoryvfs.Message{Role: "user", Content: "remember this"}); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if state, err := store.Pending(ctx, "sess-1"); err != nil || state.Pending == 0 {
		t.Fatalf("Pending = %+v/%v", state, err)
	}
	if _, err := store.Commit(ctx, "sess-1", memoryvfs.CommitOptions{}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	snap, err := store.Snapshot(ctx, "before")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snaps, err := store.Snapshots(ctx); err != nil || len(snaps) == 0 {
		t.Fatalf("Snapshots = %+v/%v", snaps, err)
	}
	_ = snap
	if _, err := store.Diff(ctx, "", ""); err == nil {
		t.Log("empty diff returned no error")
	}
	if _, err := store.CompileRules(ctx, "always test", nil); err != nil {
		t.Fatalf("CompileRules: %v", err)
	}
	if _, err := store.SearchRules(ctx, "test", 0); err != nil {
		t.Fatalf("SearchRules: %v", err)
	}
	if stats, err := store.Stats(ctx); err != nil || stats.Entries == 0 {
		t.Fatalf("Stats = %+v/%v", stats, err)
	}
	if _, err := store.GC(ctx, memoryvfs.GCOptions{}); err != nil {
		t.Fatalf("GC: %v", err)
	}
	if err := store.Rebuild(ctx); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	pack, err := store.Export(ctx, memoryvfs.RootURI)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if _, err := store.Import(ctx, pack, memoryvfs.ImportOptions{}); err != nil {
		t.Fatalf("Import: %v", err)
	}
}

func TestOptionConstructorsAndHelpers(t *testing.T) {
	store := openTest(t, "ws")
	WithExtractor(nil)(store)
	WithOptions(memoryvfs.WithMaxEntries(3))(store)
	WithClock(func() time.Time { return time.Unix(0, 0) })(store)
	if messageKey(memoryvfs.Message{Role: "user", Content: "x"}) == "" || messageKey(memoryvfs.Message{Key: "k"}) != "k" {
		t.Fatal("messageKey wrong")
	}
	if got := sessionSegment("My Session!!"); got == "" || got == "My Session!!" {
		t.Fatalf("sessionSegment = %q", got)
	}
	if sessionSegment("plain-id") != "plain-id" || !strings.HasPrefix(sessionSegment("---"), "session-") {
		t.Fatalf("sessionSegment normalization wrong")
	}
	if !containsKind([]memoryvfs.Kind{memoryvfs.KindMemory}, memoryvfs.KindMemory) || containsKind(nil, memoryvfs.KindMemory) {
		t.Fatal("containsKind wrong")
	}
	if !containsString([]string{"a"}, "a") || containsString(nil, "a") {
		t.Fatal("containsString wrong")
	}
	if !hasLevel(memoryvfs.Entry{Overview: "o"}, []memoryvfs.Level{memoryvfs.Level1}) || hasLevel(memoryvfs.Entry{}, []memoryvfs.Level{memoryvfs.Level2}) {
		t.Fatal("hasLevel wrong")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.check(context.Background()); err == nil {
		t.Fatal("closed store check succeeded")
	}
	if _, err := Open(""); err == nil {
		t.Fatal("empty dsn accepted")
	}
}
