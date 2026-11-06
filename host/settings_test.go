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
	"testing"
)

func newSettingsDir(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	return dir, filepath.Join(dir, SettingsFileName)
}

func TestSettingsDefaultsOnFirstRun(t *testing.T) {
	dir, path := newSettingsDir(t)
	settings, err := NewSettings(dir)
	if err != nil {
		t.Fatalf("NewSettings: %v", err)
	}
	if got := settings.Get("agent.model", "default-model"); got != "default-model" {
		t.Fatalf("Get(missing) = %q, want the default", got)
	}
	if all := settings.All(); len(all) != 0 {
		t.Fatalf("All() on first run = %v, want empty", all)
	}
	if got := settings.Path(); got != path {
		t.Fatalf("Path() = %q, want %q", got, path)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("settings file exists after a read-only first run: %v", err)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	dir, path := newSettingsDir(t)
	first, err := NewSettings(dir)
	if err != nil {
		t.Fatalf("NewSettings: %v", err)
	}
	if err := first.Set("agent.model", "gpt-test"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := first.Set("agent.temperature", "0.2"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// The file is plain JSON, editable by hand and readable by tooling.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var onDisk map[string]string
	if err := json.Unmarshal(data, &onDisk); err != nil {
		t.Fatalf("settings file is not valid JSON: %v\n%s", err, data)
	}
	if onDisk["agent.model"] != "gpt-test" || onDisk["agent.temperature"] != "0.2" {
		t.Fatalf("settings file = %v, want the written values", onDisk)
	}

	// The next process run reads the values back.
	second, err := NewSettings(dir)
	if err != nil {
		t.Fatalf("NewSettings (second): %v", err)
	}
	if got := second.Get("agent.model", ""); got != "gpt-test" {
		t.Fatalf("reloaded agent.model = %q, want gpt-test", got)
	}
	all := second.All()
	all["agent.model"] = "mutated"
	if got := second.Get("agent.model", ""); got != "gpt-test" {
		t.Fatalf("mutating All() result changed the store: %q", got)
	}
}

func TestSettingsWriteIsAtomic(t *testing.T) {
	dir, path := newSettingsDir(t)
	settings, err := NewSettings(dir)
	if err != nil {
		t.Fatalf("NewSettings: %v", err)
	}
	for index := 0; index < 10; index++ {
		if err := settings.Set(fmt.Sprintf("key.%02d", index), "value"); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("temporary file left behind: %s", entry.Name())
		}
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if mode := fileInfo.Mode().Perm(); mode != 0o600 {
		t.Fatalf("settings file mode = %o, want 600", mode)
	}
}

func TestSettingsRejectsCorruptFile(t *testing.T) {
	dir, path := newSettingsDir(t)
	if err := os.WriteFile(path, []byte("{ not json"), 0o600); err != nil {
		t.Fatalf("write corrupt settings: %v", err)
	}
	if _, err := NewSettings(dir); err == nil {
		t.Fatal("NewSettings accepted a corrupt settings file")
	}
}

func TestSettingsInvalidUse(t *testing.T) {
	if _, err := NewSettings(""); err == nil {
		t.Fatal("NewSettings(\"\") succeeded, want an error")
	}
	if err := (*Settings)(nil).Set("k", "v"); err == nil {
		t.Fatal("nil Settings Set succeeded")
	}
	if got := (*Settings)(nil).Get("k", "fallback"); got != "fallback" {
		t.Fatalf("nil Settings Get = %q, want fallback", got)
	}
}

func TestSettingsConcurrentAccess(t *testing.T) {
	dir, _ := newSettingsDir(t)
	settings, err := NewSettings(dir)
	if err != nil {
		t.Fatalf("NewSettings: %v", err)
	}
	const workers, rounds = 4, 25
	var group sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			for round := 0; round < rounds; round++ {
				key := fmt.Sprintf("worker.%d.round.%d", worker, round)
				if err := settings.Set(key, "value"); err != nil {
					t.Errorf("Set(%s): %v", key, err)
					return
				}
				_ = settings.Get(key, "")
				_ = settings.All()
			}
		}(worker)
	}
	group.Wait()

	reloaded, err := NewSettings(dir)
	if err != nil {
		t.Fatalf("NewSettings after concurrent writes: %v", err)
	}
	if got, want := len(reloaded.All()), workers*rounds; got != want {
		t.Fatalf("persisted settings = %d, want %d", got, want)
	}
}
