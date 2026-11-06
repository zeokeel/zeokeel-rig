package host

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/zeokeel/zeokeel-rig/contracts/v1"
)

// SettingsFileName is the settings file inside a product's data directory.
const SettingsFileName = "settings.json"

// Settings is the process host's file-backed v1.Settings implementation: a flat
// JSON object of string keys and values, loaded once at startup and written
// atomically on every Set.
//
// Write path: encode -> create a temporary file in the same directory -> sync
// -> rename over settings.json -> sync the directory. A crash leaves either the
// previous complete file (before the rename) or the new complete file (after);
// a truncated settings file is never observable.
type Settings struct {
	path string

	mu     sync.Mutex
	values map[string]string
}

// compile-time check: the host provides this as v1.Settings.
var _ v1.Settings = (*Settings)(nil)

// NewSettings loads the settings in dataDir. A missing file (first run) is the
// empty/default set, not an error; a malformed file is an error, because
// silently resetting user settings would hide data loss.
func NewSettings(dataDir string) (*Settings, error) {
	dataDir = strings.TrimSpace(dataDir)
	if dataDir == "" {
		return nil, errors.New("rig: settings data directory is required")
	}
	settings := &Settings{
		path:   filepath.Join(dataDir, SettingsFileName),
		values: make(map[string]string),
	}
	data, err := os.ReadFile(settings.path)
	switch {
	case err == nil:
		var values map[string]string
		if err := json.Unmarshal(data, &values); err != nil {
			return nil, fmt.Errorf("rig: parse settings %q: %w", settings.path, err)
		}
		if values != nil {
			settings.values = values
		}
	case errors.Is(err, fs.ErrNotExist):
		// First run: defaults. The file is created by the first Set.
	default:
		return nil, fmt.Errorf("rig: read settings %q: %w", settings.path, err)
	}
	return settings, nil
}

// Path returns the backing file path (diagnostics and tests).
func (s *Settings) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// Get returns the value for key, or def when key is unset.
func (s *Settings) Get(key, def string) string {
	if s == nil {
		return def
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if value, ok := s.values[key]; ok {
		return value
	}
	return def
}

// Set stores one setting and makes it durable before returning.
func (s *Settings) Set(key, value string) error {
	if s == nil {
		return errors.New("rig: nil settings")
	}
	if key == "" {
		return errors.New("rig: settings key is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, ok := s.values[key]; ok && current == value {
		return nil
	}
	next := make(map[string]string, len(s.values)+1)
	for k, v := range s.values {
		next[k] = v
	}
	next[key] = value
	if err := writeJSONAtomic(s.path, next); err != nil {
		return err
	}
	s.values = next
	return nil
}

// All returns a copy of every setting (diagnostics and tests).
func (s *Settings) All() map[string]string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.values))
	for key, value := range s.values {
		out[key] = value
	}
	return out
}

// writeJSONAtomic writes values to path as indented JSON through a temporary
// file and a rename.
func writeJSONAtomic(path string, values map[string]string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("rig: create settings directory %q: %w", dir, err)
	}
	data, err := json.MarshalIndent(values, "", "  ")
	if err != nil {
		return fmt.Errorf("rig: encode settings: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, ".settings-*.tmp")
	if err != nil {
		return fmt.Errorf("rig: create temporary settings file: %w", err)
	}
	tmpName := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("rig: write temporary settings file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("rig: sync temporary settings file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("rig: close temporary settings file: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("rig: set settings file mode: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rig: replace settings %q: %w", path, err)
	}
	renamed = true
	// fsync the directory so the rename itself survives a power loss.
	if dirFile, err := os.Open(dir); err == nil {
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}
	return nil
}
