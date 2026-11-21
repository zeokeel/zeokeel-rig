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
