package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/zeokeel/zeokeel-rig/memoryvfs"
)

func openTest(t *testing.T, workspace string) *Store {
	t.Helper()
	dsn := "file:" + filepath.Join(t.TempDir(), "memory.db") + "?_pragma=busy_timeout(5000)"
	store, err := Open(dsn, WithWorkspace(workspace))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestWorkspaceIsolationAndRecall(t *testing.T) {
	ctx := context.Background()
	dsn := "file:" + filepath.Join(t.TempDir(), "shared.db") + "?_pragma=busy_timeout(5000)"
	a, err := Open(dsn, WithWorkspace("repo-a"))
	if err != nil {
		t.Fatalf("Open A: %v", err)
	}
	defer func() { _ = a.Close() }()
	b, err := Open(dsn, WithWorkspace("repo-b"))
	if err != nil {
		t.Fatalf("Open B: %v", err)
	}
	defer func() { _ = b.Close() }()
	uri := memoryvfs.URI("mem://user/self/memories/preferences/theme")
	if _, err := a.Write(ctx, memoryvfs.Entry{URI: uri, Kind: memoryvfs.KindMemory, Type: "preferences", Summary: "A prefers dark mode", Content: "dark"}); err != nil {
		t.Fatalf("A Write: %v", err)
	}
	if _, err := b.Write(ctx, memoryvfs.Entry{URI: uri, Kind: memoryvfs.KindMemory, Type: "preferences", Summary: "B prefers light mode", Content: "light"}); err != nil {
		t.Fatalf("B Write: %v", err)
	}
	gotA, err := a.Read(ctx, uri, memoryvfs.Level2)
	if err != nil || gotA.Summary != "A prefers dark mode" {
		t.Fatalf("A read = %+v, %v", gotA, err)
	}
	gotB, err := b.Read(ctx, uri, memoryvfs.Level2)
	if err != nil || gotB.Summary != "B prefers light mode" {
		t.Fatalf("B read = %+v, %v", gotB, err)
	}
	result, err := a.Find(ctx, memoryvfs.Query{Text: "dark", IncludeBody: true, Limit: 5})
	if err != nil || len(result.Entries) != 1 || result.Entries[0].Summary != "A prefers dark mode" {
		t.Fatalf("A recall = %+v, %v", result.Entries, err)
	}
	result, err = b.Find(ctx, memoryvfs.Query{Text: "dark", Limit: 5})
	if err != nil || len(result.Entries) != 0 {
		t.Fatalf("B 看到了 A 的记忆: %+v, %v", result.Entries, err)
	}
}

func TestFolderStructureListTreeAndSessions(t *testing.T) {
	ctx := context.Background()
	store := openTest(t, "ws")
	if _, err := store.Write(ctx, memoryvfs.Entry{URI: "mem://user/self/memories/preferences/theme", Kind: memoryvfs.KindMemory, Type: "preferences", Summary: "theme", Content: "dark"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	children, err := store.List(ctx, "mem://user/self/memories")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(children) != 1 || children[0].Kind != memoryvfs.KindDir || children[0].URI.Base() != "preferences" {
		t.Fatalf("List = %+v", children)
	}
	tree, err := store.Tree(ctx, "mem://user/self/memories", 3)
	if err != nil || len(tree) == 0 {
		t.Fatalf("Tree = %+v, %v", tree, err)
	}
	if err := store.Capture(ctx, "s1", memoryvfs.Message{Role: "user", Content: "hello"}); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	state, err := store.Pending(ctx, "s1")
	if err != nil || state.Pending != 1 {
		t.Fatalf("Pending = %+v, %v", state, err)
	}
	result, err := store.Commit(ctx, "s1", memoryvfs.CommitOptions{})
	if err != nil || result.Archive == "" {
		t.Fatalf("Commit = %+v, %v", result, err)
	}
	archives, err := store.List(ctx, "mem://sessions/s1")
	if err != nil || len(archives) == 0 {
		t.Fatalf("archives = %+v, %v", archives, err)
	}
}

func TestRulesSnapshotRestore(t *testing.T) {
	ctx := context.Background()
	store := openTest(t, "ws")
	rules, err := store.CompileRules(ctx, "always run tests before commit", nil)
	if err != nil {
		t.Fatalf("CompileRules: %v", err)
	}
	found, err := store.SearchRules(ctx, "run tests", 0.5)
	if err != nil || len(found) == 0 || found[0].URI != rules.URI {
		t.Fatalf("SearchRules = %+v, %v", found, err)
	}
	snapshot, err := store.Snapshot(ctx, "before")
	if err != nil || snapshot.Entries == 0 {
		t.Fatalf("Snapshot = %+v, %v", snapshot, err)
	}
	if err := store.Delete(ctx, rules.URI); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := store.Restore(ctx, snapshot.ID); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, err := store.Stat(ctx, rules.URI); err != nil {
		t.Fatalf("Restore 后规则不见了: %v", err)
	}
}
