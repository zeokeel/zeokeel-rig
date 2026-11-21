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
	"sort"
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

// Policy is the deterministic retention policy of a memory store.
type Policy struct {
	// MaxEntries is the maximum number of entries to keep (0 = unlimited).
	MaxEntries int
	// MaxAge is the retention window (0 = no expiry).
	MaxAge time.Duration
}

// Report summarizes one consolidation.
type Report struct {
	RemovedExpired int
	RemovedOverCap int
	Remaining      int
}

// Consolidate applies the expiry window (created strictly before now-MaxAge)
// and then the entry cap (removing the oldest entries). Entries with a zero
// Created time never expire. It returns the kept entries and the counts; the
// caller owns persistence.
func Consolidate(entries []rig.Memory, policy Policy, now time.Time) ([]rig.Memory, Report) {
	report := Report{Remaining: len(entries)}
	if policy.MaxAge <= 0 && policy.MaxEntries <= 0 {
		return entries, report
	}
	keep := make([]bool, len(entries))
	for index := range keep {
		keep[index] = true
	}
	if policy.MaxAge > 0 {
		cutoff := now.Add(-policy.MaxAge)
		for index, memory := range entries {
			if memory.Created.IsZero() || !memory.Created.Before(cutoff) {
				continue
			}
			keep[index] = false
			report.RemovedExpired++
		}
	}
	if policy.MaxEntries > 0 {
		remaining := 0
		for index := range entries {
			if keep[index] {
				remaining++
			}
		}
		if remaining > policy.MaxEntries {
			order := make([]int, 0, remaining)
			for index := range entries {
				if keep[index] {
					order = append(order, index)
				}
			}
			sort.SliceStable(order, func(left, right int) bool {
				return entries[order[left]].Created.Before(entries[order[right]].Created)
			})
			for _, index := range order[:remaining-policy.MaxEntries] {
				keep[index] = false
				report.RemovedOverCap++
			}
		}
	}
	kept := make([]rig.Memory, 0, len(entries))
	for index, memory := range entries {
		if keep[index] {
			kept = append(kept, memory)
		}
	}
	report.Remaining = len(kept)
	return kept, report
}
