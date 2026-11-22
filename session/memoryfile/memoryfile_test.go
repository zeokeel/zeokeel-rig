package memoryfile_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/session/memoryfile"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.json")
	entries := []rig.Memory{{ID: "1", Text: "hello", Scope: "global", Created: time.Now().UTC()}}
	if err := memoryfile.Save(path, entries); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, recovery := memoryfile.Load(path)
	if recovery != "" || len(loaded) != 1 || loaded[0].Text != "hello" {
		t.Fatalf("Load = %+v/%q", loaded, recovery)
	}
}

func TestLoadMissingAndLegacy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.json")
	if loaded, recovery := memoryfile.Load(path); loaded != nil || recovery != "" {
		t.Fatalf("missing = %+v/%q", loaded, recovery)
	}
	if err := os.WriteFile(path, []byte(`[{"id":"1","text":"legacy"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if loaded, recovery := memoryfile.Load(path); recovery != "" || len(loaded) != 1 || loaded[0].Text != "legacy" {
		t.Fatalf("legacy = %+v/%q", loaded, recovery)
	}
}

func TestCorruptFileIsQuarantined(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.json")
	if err := os.WriteFile(path, []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if loaded, recovery := memoryfile.Load(path); len(loaded) != 0 || recovery == "" {
		t.Fatalf("corrupt = %+v/%q", loaded, recovery)
	}
	matches, _ := filepath.Glob(path + ".corrupt-*")
	if len(matches) != 1 {
		t.Fatalf("quarantine files = %v", matches)
	}
}

func TestConsolidateExpiryAndCap(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	entries := []rig.Memory{
		{ID: "old", Created: now.Add(-48 * time.Hour)},
		{ID: "a", Created: now.Add(-3 * time.Hour)},
		{ID: "b", Created: now.Add(-2 * time.Hour)},
		{ID: "c", Created: now.Add(-time.Hour)},
		{ID: "zero"},
	}
	kept, report := memoryfile.Consolidate(entries, memoryfile.Policy{MaxAge: 24 * time.Hour, MaxEntries: 3}, now)
	if report.RemovedExpired != 1 || report.RemovedOverCap != 1 || report.Remaining != 3 {
		t.Fatalf("report = %+v", report)
	}
	if len(kept) != 3 {
		t.Fatalf("kept = %+v", kept)
	}
	// Oldest kept entries are dropped first; zero-time entries never expire.
	ids := []string{kept[0].ID, kept[1].ID, kept[2].ID}
	want := []string{"a", "b", "c"}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("kept ids = %v, want %v", ids, want)
		}
	}
	if _, empty := memoryfile.Consolidate(entries, memoryfile.Policy{}, now); empty.Remaining != len(entries) {
		t.Fatalf("empty policy report = %+v", empty)
	}
}
