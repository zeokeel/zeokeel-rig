// Package memoryfile persists a memory entry list as one JSON document.
//
// It is the durable half of a memory store: load (with legacy bare-array
// support and quarantine of an unreadable/corrupt file) and an atomic save.
// The entry policy, scopes and ids belong to the host store.
package memoryfile

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

// document is the canonical file shape.
type document struct {
	Entries []rig.Memory `json:"entries"`
}

// Load reads path. A missing file is an empty list. An unreadable or corrupt
// file is quarantined (renamed *.corrupt-<nanos>) and reported through the
// returned recovery message instead of failing activation.
func Load(path string) ([]rig.Memory, string) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ""
		}
		return nil, quarantine(path, fmt.Sprintf("memory file %q could not be read (%v)", path, err))
	}
	if len(data) == 0 {
		return nil, ""
	}
	var decoded document
	if err := json.Unmarshal(data, &decoded); err == nil {
		return decoded.Entries, ""
	}
	// Legacy shape: a bare array of entries.
	var entries []rig.Memory
	if err := json.Unmarshal(data, &entries); err == nil {
		return entries, ""
	}
	return nil, quarantine(path, fmt.Sprintf("memory file %q is corrupt", path))
}

// Save writes entries atomically (temp file, fsync, rename, directory sync).
func Save(path string, entries []rig.Memory) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("memory: create directory: %w", err)
	}
	data, err := json.MarshalIndent(document{Entries: entries}, "", "  ")
	if err != nil {
		return fmt.Errorf("memory: encode: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".memory-*.tmp")
	if err != nil {
		return fmt.Errorf("memory: create temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	cleanup := func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}
	if _, err := temporary.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("memory: write: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("memory: sync: %w", err)
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("memory: close: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("memory: replace %q: %w", path, err)
	}
	if handle, err := os.Open(directory); err == nil {
		_ = handle.Sync()
		_ = handle.Close()
	}
	return nil
}

// quarantine moves an unusable file aside and returns the message.
func quarantine(path, reason string) string {
	backup := fmt.Sprintf("%s.corrupt-%d", path, time.Now().UTC().UnixNano())
	if err := os.Rename(path, backup); err != nil {
		return reason + fmt.Sprintf(" (could not quarantine: %v)", err)
	}
	return reason + " (quarantined as " + backup + ")"
}
