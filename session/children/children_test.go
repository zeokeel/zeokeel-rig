package children_test

// The child-session document is the durable half of delegation: hosts list
// children after a restart, resume historical ones and deliver one receipt.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/session/children"
)

func newStore(t *testing.T, opts children.Options) (*children.Store, string) {
	t.Helper()
	dir := t.TempDir()
	opts.Path = filepath.Join(dir, "child-sessions.json")
	opts.Logger = func(string, ...any) {}
	return children.Open(opts), opts.Path
}

func TestUpsertPersistsAndReloads(t *testing.T) {
	store, path := newStore(t, children.Options{Version: 2})
	record := children.Record{ID: "child-1", ParentSessionID: "s1", Kind: "subagent", Name: "researcher", Status: "running"}
	if !store.Upsert(record) {
		t.Fatal("Upsert reported no change")
	}
	stored, _ := store.Record("child-1")
	if store.Upsert(stored) {
		t.Fatal("Upsert of an identical record reported a change")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("document missing: %v", err)
	}
	reloaded := children.Open(children.Options{Path: path, Version: 2, Logger: func(string, ...any) {}})
	got, ok := reloaded.Record("child-1")
	if !ok || got.Name != "researcher" || got.ParentSessionID != "s1" {
		t.Fatalf("reloaded record = %+v (ok=%v)", got, ok)
	}
}

func TestVersionMismatchQuarantines(t *testing.T) {
	store, path := newStore(t, children.Options{Version: 2})
	store.Upsert(children.Record{ID: "child-1"})
	// A document written with another version is moved aside instead of read.
	other := children.Open(children.Options{Path: path, Version: 3, Logger: func(string, ...any) {}})
	if _, ok := other.Record("child-1"); ok {
		t.Fatal("version 2 document read by a version 3 store")
	}
	matches, _ := filepath.Glob(path + ".corrupt-*")
	if len(matches) != 1 {
		t.Fatalf("quarantine files = %v", matches)
	}
}

func TestOnLoadAndLegacySeed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "child-sessions.json")
	store := children.Open(children.Options{
		Path: path, Version: 2,
		OnLoad: func(records []children.Record) []children.Record {
			for index := range records {
				if records[index].Status == "running" {
					records[index].Status = "interrupted"
				}
			}
			return records
		},
		Legacy: func() []children.Record {
			return []children.Record{{ID: "child-legacy", Status: "running", Name: "old"}}
		},
		Logger: func(string, ...any) {},
	})
	got, ok := store.Record("child-legacy")
	if !ok || got.Name != "old" {
		t.Fatalf("legacy seed = %+v (ok=%v)", got, ok)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("legacy seed was not persisted: %v", err)
	}
	// Reopening applies OnLoad to the stored record.
	reloaded := children.Open(children.Options{Path: path, Version: 2,
		OnLoad: func(records []children.Record) []children.Record {
			for index := range records {
				if records[index].Status == "running" {
					records[index].Status = "interrupted"
				}
			}
			return records
		}, Logger: func(string, ...any) {}})
	if got, _ := reloaded.Record("child-legacy"); got.Status != "interrupted" {
		t.Fatalf("OnLoad not applied on reload: %+v", got)
	}
}

func TestPruneAndOrder(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	store, _ := newStore(t, children.Options{
		Keep: 2, TTL: 24 * time.Hour,
		Now: func() time.Time { return now },
	})
	store.Upsert(children.Record{ID: "old", Updated: now.Add(-48 * time.Hour)})
	store.Upsert(children.Record{ID: "a", Updated: now.Add(-time.Hour)})
	store.Upsert(children.Record{ID: "b", Updated: now.Add(-30 * time.Minute)})
	store.Upsert(children.Record{ID: "c", Updated: now})
	records := store.Records()
	if len(records) != 2 || records[0].ID != "c" || records[1].ID != "b" {
		t.Fatalf("records = %+v, want newest two", records)
	}
}

func TestMarkNotifiedAndBackendRef(t *testing.T) {
	store, _ := newStore(t, children.Options{})
	store.Upsert(children.Record{ID: "child-1", ParentSessionID: "s1", Kind: "team", BackendRef: "m1"})
	if got, ok := store.ByBackendRef("m1", "s1", "team"); !ok || got.ID != "child-1" {
		t.Fatalf("ByBackendRef = %+v (ok=%v)", got, ok)
	}
	if _, ok := store.ByBackendRef("m1", "other", "subagent"); ok {
		t.Fatal("ByBackendRef matched the wrong kind")
	}
	if !store.MarkNotified("child-1", time.Now()) {
		t.Fatal("MarkNotified reported no change")
	}
	if store.MarkNotified("child-1", time.Now()) {
		t.Fatal("MarkNotified is not idempotent")
	}
	if got, _ := store.Record("child-1"); !got.Notified {
		t.Fatal("record not marked")
	}
}

func TestClearRemovesTheDocument(t *testing.T) {
	store, path := newStore(t, children.Options{})
	store.Upsert(children.Record{ID: "child-1"})
	store.Clear()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("document still exists: %v", err)
	}
	if len(store.Records()) != 0 {
		t.Fatal("records survived clear")
	}
}

func TestCorruptDocumentIsQuarantined(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "child-sessions.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := children.Open(children.Options{Path: path, Logger: func(string, ...any) {}})
	store.Upsert(children.Record{ID: "child-1"})
	matches, _ := filepath.Glob(path + ".corrupt-*")
	if len(matches) != 1 {
		t.Fatalf("quarantine files = %v", matches)
	}
}
