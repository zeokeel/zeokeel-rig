package memstore_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/session/memstore"
)

func open(t *testing.T, path *string, workspace string) *memstore.Store {
	t.Helper()
	if *path == "" {
		*path = filepath.Join(t.TempDir(), "memory.json")
	}
	store, err := memstore.Open(memstore.Options{
		Path:         func() string { return *path },
		WorkspaceID:  func(context.Context) string { return workspace },
		DefaultScope: memstore.ScopeGlobal,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return store
}

func TestAddListUpdateRemoveAndSearch(t *testing.T) {
	path := ""
	store := open(t, &path, "ws-1")
	ctx := context.Background()
	first, err := store.Add(ctx, rig.Memory{Text: "alpha"})
	if err != nil || first.ID == "" || first.Scope != memstore.ScopeGlobal || first.Created.IsZero() {
		t.Fatalf("Add = %+v/%v", first, err)
	}
	if _, err := store.Add(ctx, rig.Memory{Text: "x", Scope: "bogus"}); err == nil {
		t.Fatal("invalid scope accepted")
	}
	scoped, err := store.Add(ctx, rig.Memory{Text: "beta", Scope: memstore.ScopeWorkspace})
	if err != nil || scoped.Scope != memstore.ScopeForWorkspace("ws-1") {
		t.Fatalf("workspace Add = %+v/%v", scoped, err)
	}
	list, err := store.List(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("List = %+v/%v", list, err)
	}
	if all, _ := store.ListAll(ctx); len(all) != 2 {
		t.Fatalf("ListAll = %+v", all)
	}
	if got, _ := store.Search(ctx, "BET"); len(got) != 1 || got[0].ID != scoped.ID {
		t.Fatalf("Search = %+v", got)
	}
	if got, _ := store.Search(ctx, ""); len(got) != 2 {
		t.Fatalf("Search empty = %+v", got)
	}
	updated, err := store.Update(ctx, first.ID, "  gamma  ", "")
	if err != nil || updated.Text != "gamma" || updated.Scope != memstore.ScopeGlobal {
		t.Fatalf("Update = %+v/%v", updated, err)
	}
	if _, err := store.Update(ctx, "missing", "x", ""); err == nil {
		t.Fatal("missing update accepted")
	}
	if _, err := store.Update(ctx, first.ID, "  ", ""); err == nil {
		t.Fatal("blank update accepted")
	}
	if !store.Remove(ctx, scoped.ID) || store.Remove(ctx, scoped.ID) || store.Remove(ctx, "") {
		t.Fatal("Remove result wrong")
	}
	if store.Version() == 0 {
		t.Fatal("Version did not advance")
	}
}

func TestWorkspaceVisibility(t *testing.T) {
	path := ""
	store := open(t, &path, "ws-1")
	ctx := context.Background()
	if _, err := store.Add(ctx, rig.Memory{Text: "one", Scope: memstore.ScopeForWorkspace("ws-1")}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Add(ctx, rig.Memory{Text: "two", Scope: memstore.ScopeForWorkspace("ws-2")}); err != nil {
		t.Fatal(err)
	}
	if list, _ := store.List(ctx); len(list) != 1 || list[0].Text != "one" {
		t.Fatalf("ws-1 List = %+v", list)
	}
	other := open(t, &path, "ws-2")
	if list, _ := other.List(ctx); len(list) != 1 || list[0].Text != "two" {
		t.Fatalf("ws-2 List = %+v", list)
	}
}

func TestConsolidateAndRecovery(t *testing.T) {
	path := ""
	store := open(t, &path, "")
	ctx := context.Background()
	now := time.Now().UTC()
	for _, text := range []string{"old", "a", "b"} {
		created := now.Add(-48 * time.Hour)
		if text != "old" {
			created = now.Add(-time.Hour)
		}
		if _, err := store.Add(ctx, rig.Memory{Text: text, Created: created}); err != nil {
			t.Fatal(err)
		}
	}
	report, err := store.ConsolidateAt(ctx, memstore.Policy{MaxAge: 24 * time.Hour, MaxEntries: 2}, now)
	if err != nil || report.RemovedExpired != 1 || report.Remaining != 2 {
		t.Fatalf("Consolidate = %+v/%v", report, err)
	}
	if _, err := store.Consolidate(ctx, memstore.Policy{}); err != nil {
		t.Fatal(err)
	}
	// A corrupt document is quarantined and reported.
	bad := filepath.Join(t.TempDir(), "memory.json")
	if err := os.WriteFile(bad, []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	corrupt := open(t, &bad, "")
	if !strings.Contains(corrupt.RecoveryNotice(), "corrupt") {
		t.Fatalf("recovery = %q", corrupt.RecoveryNotice())
	}
	if entries, _ := corrupt.List(ctx); len(entries) != 0 {
		t.Fatalf("corrupt List = %+v", entries)
	}
}

func TestPersistenceFailureRollsBack(t *testing.T) {
	path := ""
	store := open(t, &path, "")
	ctx := context.Background()
	if _, err := store.Add(ctx, rig.Memory{Text: "keep"}); err != nil {
		t.Fatal(err)
	}
	// A regular file in the parent path makes MkdirAll/rename fail.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	badPath := filepath.Join(blocker, "memory.json")
	bad := open(t, &badPath, "")
	if _, err := bad.Add(ctx, rig.Memory{Text: "fails"}); err == nil {
		t.Fatal("add with an unwritable path accepted")
	}
	if list, _ := bad.ListAll(ctx); len(list) != 0 {
		t.Fatalf("rollback ListAll = %+v", list)
	}
}

func TestScopeHelpers(t *testing.T) {
	if id, ok := memstore.ScopeWorkspaceID("workspace:abc"); !ok || id != "abc" {
		t.Fatalf("ScopeWorkspaceID = %q/%v", id, ok)
	}
	if _, ok := memstore.ScopeWorkspaceID("global"); ok {
		t.Fatal("global parsed as workspace")
	}
	if scope, err := memstore.NormalizeConfiguredScope(""); err != nil || scope != memstore.ScopeGlobal {
		t.Fatalf("Normalize = %q/%v", scope, err)
	}
	if _, err := memstore.NormalizeConfiguredScope("bogus"); err == nil {
		t.Fatal("invalid scope accepted")
	}
}
