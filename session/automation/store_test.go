package automation

// Tests for the durable registry: CRUD, atomic persistence, restart reloading,
// corrupt-document quarantine and defensive copies. Written before the
// implementation (TDD red phase).

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func storeTask(id, name string, created time.Time) Task {
	return Task{
		ID:        id,
		Name:      name,
		Prompt:    "prompt-" + id,
		Schedule:  Schedule("every 60s"),
		Enabled:   true,
		CreatedAt: created,
		UpdatedAt: created,
		NextRun:   created.Add(time.Minute),
	}
}

func TestStoreCRUDPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := store.Path(); got != filepath.Join(dir, DefaultFileName) {
		t.Fatalf("Path = %q", got)
	}

	base := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	second := storeTask("t2", "second", base.Add(time.Minute))
	first := storeTask("t1", "first", base)
	if _, err := store.Create(second); err != nil {
		t.Fatalf("Create second: %v", err)
	}
	created, err := store.Create(first)
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}
	if created.ID != "t1" || created.Name != "first" {
		t.Fatalf("created = %+v", created)
	}
	if _, err := store.Create(first); !errors.Is(err, ErrTaskExists) {
		t.Fatalf("duplicate Create error = %v, want ErrTaskExists", err)
	}
	if _, err := store.Create(Task{Name: "no id"}); !errors.Is(err, ErrTaskIDRequired) {
		t.Fatalf("empty-id Create error = %v, want ErrTaskIDRequired", err)
	}

	tasks, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(tasks) != 2 || tasks[0].ID != "t1" || tasks[1].ID != "t2" {
		t.Fatalf("List = %+v, want creation order t1,t2", tasks)
	}

	updated := tasks[1]
	updated.Name = "renamed"
	updated.Prompt = "new prompt"
	updated.NextRun = base.Add(2 * time.Minute)
	if _, err := store.Update(updated); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := store.Update(Task{ID: "missing"}); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("Update(missing) error = %v, want ErrTaskNotFound", err)
	}
	if err := store.Delete("missing"); err != nil {
		t.Fatalf("Delete(unknown) = %v, want no-op", err)
	}
	if err := store.Delete("t1"); err != nil {
		t.Fatalf("Delete(t1): %v", err)
	}

	// The on-disk document is valid JSON with the surviving task.
	raw, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatalf("read registry: %v", err)
	}
	var document struct {
		Tasks []Task `json:"tasks"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("registry is not valid JSON: %v\n%s", err, raw)
	}
	if len(document.Tasks) != 1 || document.Tasks[0].ID != "t2" || document.Tasks[0].Name != "renamed" {
		t.Fatalf("persisted tasks = %+v", document.Tasks)
	}
	temporary, err := filepath.Glob(filepath.Join(dir, ".automations-*.tmp"))
	if err != nil || len(temporary) != 0 {
		t.Fatalf("temporary files left behind: %v (%v)", temporary, err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	tasks, err = reopened.List()
	if err != nil || len(tasks) != 1 || tasks[0].ID != "t2" || tasks[0].Name != "renamed" {
		t.Fatalf("reopened tasks = %+v/%v", tasks, err)
	}
	if !tasks[0].NextRun.Equal(base.Add(2 * time.Minute)) {
		t.Fatalf("reopened NextRun = %s", tasks[0].NextRun)
	}
}

func TestStoreQuarantinesCorruptDocument(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DefaultFileName)
	if err := os.WriteFile(path, []byte("{corrupt"), 0o600); err != nil {
		t.Fatalf("write corrupt registry: %v", err)
	}
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open(corrupt) = %v, want a quarantined empty store", err)
	}
	tasks, err := store.List()
	if err != nil || len(tasks) != 0 {
		t.Fatalf("tasks after quarantine = %+v/%v, want empty", tasks, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("corrupt registry still at %s (err=%v)", path, err)
	}
	backups, err := filepath.Glob(path + ".corrupt-*")
	if err != nil || len(backups) != 1 {
		t.Fatalf("quarantine backups = %v/%v, want exactly one", backups, err)
	}
	// The recovered store still accepts writes.
	if _, err := store.Create(storeTask("t1", "first", time.Now().UTC())); err != nil {
		t.Fatalf("Create after quarantine: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("registry not recreated after quarantine: %v", err)
	}
}

func TestStoreLoadsRegistryWithoutTasksOrRunHistory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DefaultFileName)
	// An older/simple document: no run history, no schedule.
	legacy := `{"tasks":[{"id":"old","name":"legacy","prompt":"p","enabled":true,` +
		`"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z",` +
		`"lastRun":"0001-01-01T00:00:00Z","nextRun":"2026-01-01T00:01:00Z","lastError":""}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatalf("write registry: %v", err)
	}
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tasks, err := store.List()
	if err != nil || len(tasks) != 1 {
		t.Fatalf("List = %+v/%v", tasks, err)
	}
	if tasks[0].ID != "old" || tasks[0].RunHistory != nil || tasks[0].Schedule != "" {
		t.Fatalf("legacy task = %+v", tasks[0])
	}
}

func TestStorePersistFailureRollsBackMemory(t *testing.T) {
	dir := t.TempDir()
	// A regular file where the registry directory should be makes MkdirAll fail.
	blocked := filepath.Join(dir, "blocked")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	store, err := Open(blocked)
	if err != nil {
		t.Fatalf("Open(blocked): %v", err)
	}
	if _, err := store.Create(storeTask("t1", "first", time.Now().UTC())); err == nil {
		t.Fatal("Create succeeded although the registry directory is unusable")
	}
	tasks, err := store.List()
	if err != nil || len(tasks) != 0 {
		t.Fatalf("in-memory state after failed persist = %+v/%v, want rollback", tasks, err)
	}
}

func TestStoreReturnsDefensiveCopies(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	base := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	task := storeTask("t1", "first", base)
	task.RunHistory = []Run{{ID: "r1", Status: StatusSuccess, StartedAt: base}}
	if _, err := store.Create(task); err != nil {
		t.Fatalf("Create: %v", err)
	}
	listed, _ := store.List()
	listed[0].RunHistory[0].Error = "mutated outside"
	listed[0].Name = "mutated"
	again, _ := store.List()
	if again[0].Name != "first" || again[0].RunHistory[0].Error != "" {
		t.Fatalf("List aliased store state: %+v", again[0])
	}
	got, ok := store.Get("t1")
	if !ok {
		t.Fatal("Get(t1) not found")
	}
	got.RunHistory[0].Status = StatusFailure
	again, _ = store.List()
	if again[0].RunHistory[0].Status != StatusSuccess {
		t.Fatalf("Get aliased store state: %+v", again[0])
	}
	if _, ok := store.Get("missing"); ok {
		t.Fatal("Get(missing) reported found")
	}
}

func TestOpenRejectsEmptyDirectory(t *testing.T) {
	if _, err := Open("   "); !errors.Is(err, ErrStoreRequired) {
		t.Fatalf("Open(empty) error = %v, want ErrStoreRequired", err)
	}
}
