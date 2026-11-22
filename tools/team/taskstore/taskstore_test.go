package taskstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	rig "github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/team"
)

var testStart = time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)

// createTask 在 manager 上创建一个任务并返回它。
func createTask(t *testing.T, manager *team.TaskManager, subject string, mutate func(*rig.TeamTask)) rig.TeamTask {
	t.Helper()
	task := rig.TeamTask{Subject: subject, Status: team.TaskStatusPending}
	if mutate != nil {
		mutate(&task)
	}
	created, err := manager.Create(context.Background(), task)
	if err != nil {
		t.Fatalf("Create(%q): %v", subject, err)
	}
	return created
}

// writeDocument 手工写入一份任务板文档。
func writeDocument(t *testing.T, path string, document Document) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatalf("marshal document: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write document: %v", err)
	}
}

// assertSameTasks 比较任务列表，忽略 Updated（依赖更新会刷新它）。
func assertSameTasks(t *testing.T, got, want []rig.TeamTask) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("tasks = %d, want %d (%+v)", len(got), len(want), got)
	}
	for index := range want {
		left, right := got[index], want[index]
		left.Updated, right.Updated = time.Time{}, time.Time{}
		if !reflect.DeepEqual(left, right) {
			t.Fatalf("task[%d] = %+v, want %+v", index, left, right)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := New(dir, "session-1")
	manager := newManager(t)
	first := createTask(t, manager, "first", func(task *rig.TeamTask) {
		task.Description = "first details"
		task.Created = testStart
	})
	second := createTask(t, manager, "second", func(task *rig.TeamTask) {
		task.Status = team.TaskStatusInProgress
		task.Owner = "alice"
		task.WriteScopes = []string{"internal/a", "internal/b"}
		task.Created = testStart.Add(time.Second)
	})
	if _, err := manager.Update(context.Background(), second.ID, rig.TeamTaskUpdate{AddBlocked: []string{first.ID}}); err != nil {
		t.Fatalf("Update dependency: %v", err)
	}
	want, err := manager.List(context.Background(), rig.TeamTaskFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if err := store.Save(context.Background(), manager); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if store.Path() != filepath.Join(dir, SessionDirName, "session-1.json") {
		t.Fatalf("Path = %q", store.Path())
	}

	fresh := newManager(t)
	board, err := New(dir, "session-1").Load(context.Background(), fresh)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !board.Loaded || board.Renumbered {
		t.Fatalf("board = %+v, want loaded and not renumbered", board)
	}
	if len(board.Deleted) != 0 {
		t.Fatalf("deleted = %v, want none", board.Deleted)
	}
	assertSameTasks(t, board.Tasks, want)
	if board.Tasks[1].BlockedBy[0] != first.ID {
		t.Fatalf("blockedBy = %v, want %q", board.Tasks[1].BlockedBy, first.ID)
	}
}

func TestStableIDsAndOrderingAcrossReloads(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	manager := newManager(t)
	for index := 0; index < 3; index++ {
		createTask(t, manager, fmt.Sprintf("task %d", index), func(task *rig.TeamTask) {
			task.Created = testStart.Add(time.Duration(index) * time.Second)
		})
	}
	if err := New(dir, "s").Save(ctx, manager); err != nil {
		t.Fatalf("Save: %v", err)
	}
	second := newManager(t)
	secondStore := New(dir, "s")
	if _, err := secondStore.Load(ctx, second); err != nil {
		t.Fatalf("Load: %v", err)
	}
	firstPass, err := second.List(ctx, rig.TeamTaskFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := taskIDs(firstPass); !reflect.DeepEqual(got, []string{"task-1", "task-2", "task-3"}) {
		t.Fatalf("ids = %v", got)
	}
	if err := secondStore.Save(ctx, second); err != nil {
		t.Fatalf("Save reloaded: %v", err)
	}
	third := newManager(t)
	board, err := New(dir, "s").Load(ctx, third)
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	assertSameTasks(t, board.Tasks, firstPass)
	if got := taskIDs(board.Tasks); !reflect.DeepEqual(got, []string{"task-1", "task-2", "task-3"}) {
		t.Fatalf("reloaded ids = %v", got)
	}
}

func TestGapsAreRenumberedAndReferencesRemapped(t *testing.T) {
	dir := t.TempDir()
	store := New(dir, "s")
	writeDocument(t, store.Path(), Document{
		Version: DocumentVersion,
		Tasks: []rig.TeamTask{
			{ID: "task-1", Subject: "one", Status: team.TaskStatusPending, Created: testStart},
			{ID: "task-3", Subject: "three", Status: team.TaskStatusPending,
				BlockedBy: []string{"task-1"}, Created: testStart.Add(time.Second)},
		},
		Deleted: []string{"task-3", "task-7"},
	})
	manager := newManager(t)
	board, err := store.Load(context.Background(), manager)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !board.Loaded || !board.Renumbered {
		t.Fatalf("board = %+v, want loaded and renumbered", board)
	}
	if got := taskIDs(board.Tasks); !reflect.DeepEqual(got, []string{"task-1", "task-2"}) {
		t.Fatalf("ids = %v, want task-1/task-2", got)
	}
	if got := board.Tasks[1].BlockedBy; !reflect.DeepEqual(got, []string{"task-1"}) {
		t.Fatalf("remapped blockedBy = %v", got)
	}
	if got := board.Deleted; !reflect.DeepEqual(got, []string{"task-2", "task-7"}) {
		t.Fatalf("deleted = %v", got)
	}
	if got := store.Deleted(); !reflect.DeepEqual(got, []string{"task-2", "task-7"}) {
		t.Fatalf("store.Deleted = %v", got)
	}
	// The next save persists the renumbered ids, so the following load is stable.
	if err := store.Save(context.Background(), manager); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reloaded := newManager(t)
	next, err := New(dir, "s").Load(context.Background(), reloaded)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if next.Renumbered {
		t.Fatalf("reload renumbered again: %+v", next)
	}
	assertSameTasks(t, next.Tasks, board.Tasks)
}

func TestTombstonesRoundTrip(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	store := New(dir, "s")
	manager := newManager(t)
	createTask(t, manager, "keep", nil)
	removed := createTask(t, manager, "remove", nil)
	store.MarkDeleted(removed.ID)
	if err := store.Save(ctx, manager); err != nil {
		t.Fatalf("Save: %v", err)
	}
	var document Document
	data, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatalf("read document: %v", err)
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode document: %v", err)
	}
	if !reflect.DeepEqual(document.Deleted, []string{removed.ID}) {
		t.Fatalf("document deleted = %v", document.Deleted)
	}

	reloadedStore := New(dir, "s")
	board, err := reloadedStore.Load(ctx, newManager(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(board.Deleted, []string{removed.ID}) {
		t.Fatalf("board deleted = %v", board.Deleted)
	}
	// Tombstones hide tasks at the host layer; the manager itself keeps them
	// (rig's task manager has no delete operation).
	if len(board.Tasks) != 2 {
		t.Fatalf("replayed tasks = %d, want 2", len(board.Tasks))
	}
	if got := reloadedStore.Deleted(); !reflect.DeepEqual(got, []string{removed.ID}) {
		t.Fatalf("store deleted = %v", got)
	}

	reloadedStore.ClearDeleted(removed.ID)
	if err := reloadedStore.Save(ctx, newManagerWithTasks(t, board.Tasks)); err != nil {
		t.Fatalf("Save after clear: %v", err)
	}
	board, err = New(dir, "s").Load(ctx, newManager(t))
	if err != nil {
		t.Fatalf("Load after clear: %v", err)
	}
	if len(board.Deleted) != 0 {
		t.Fatalf("deleted after clear = %v", board.Deleted)
	}
}

// newManagerWithTasks replays a task list into a fresh manager without copying
// ids (create path), used only to build a manager for a save after a clear.
func newManagerWithTasks(t *testing.T, tasks []rig.TeamTask) *team.TaskManager {
	t.Helper()
	manager := newManager(t)
	for _, task := range tasks {
		createTask(t, manager, task.Subject, func(copy *rig.TeamTask) {
			copy.Description = task.Description
			copy.Status = task.Status
			copy.Owner = task.Owner
			copy.WriteScopes = task.WriteScopes
			copy.Created = task.Created
		})
	}
	return manager
}

func TestCorruptDocumentIsQuarantined(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	store := New(dir, "s")
	if err := os.MkdirAll(filepath.Dir(store.Path()), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(store.Path(), []byte("not json at all"), 0o644); err != nil {
		t.Fatalf("write corrupt document: %v", err)
	}
	board, err := store.Load(ctx, newManager(t))
	if err != nil {
		t.Fatalf("Load returned an error for a corrupt document: %v", err)
	}
	if board.Loaded || len(board.Tasks) != 0 || len(board.Deleted) != 0 {
		t.Fatalf("board = %+v, want empty", board)
	}
	if _, err := os.Stat(store.Path()); !os.IsNotExist(err) {
		t.Fatalf("corrupt document still at its path: %v", err)
	}
	quarantined, err := filepath.Glob(store.Path() + ".corrupt-*")
	if err != nil || len(quarantined) != 1 {
		t.Fatalf("quarantined files = %v (err %v), want 1", quarantined, err)
	}
	// The board stays usable: the next save writes a valid document.
	manager := newManager(t)
	createTask(t, manager, "after recovery", nil)
	if err := store.Save(ctx, manager); err != nil {
		t.Fatalf("Save after quarantine: %v", err)
	}
	fresh, err := New(dir, "s").Load(ctx, newManager(t))
	if err != nil || !fresh.Loaded || len(fresh.Tasks) != 1 {
		t.Fatalf("recovered board = %+v (err %v)", fresh, err)
	}
}

func TestShortDocuments(t *testing.T) {
	ctx := context.Background()
	t.Run("empty file is an empty board without quarantine", func(t *testing.T) {
		dir := t.TempDir()
		store := New(dir, "s")
		if err := os.MkdirAll(filepath.Dir(store.Path()), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(store.Path(), []byte("  \n\t\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		board, err := store.Load(ctx, newManager(t))
		if err != nil || board.Loaded {
			t.Fatalf("board = %+v (err %v), want empty and not loaded", board, err)
		}
		if _, err := os.Stat(store.Path()); err != nil {
			t.Fatalf("empty document was removed: %v", err)
		}
	})
	t.Run("missing version is unsupported", func(t *testing.T) {
		dir := t.TempDir()
		store := New(dir, "s")
		writeDocument(t, store.Path(), Document{})
		board, err := store.Load(ctx, newManager(t))
		if err != nil || board.Loaded {
			t.Fatalf("board = %+v (err %v)", board, err)
		}
		quarantined, _ := filepath.Glob(store.Path() + ".corrupt-*")
		if len(quarantined) != 1 {
			t.Fatalf("quarantined files = %v, want 1", quarantined)
		}
	})
	t.Run("future version is unsupported", func(t *testing.T) {
		dir := t.TempDir()
		store := New(dir, "s")
		writeDocument(t, store.Path(), Document{Version: 9})
		if _, err := store.Load(ctx, newManager(t)); err != nil {
			t.Fatalf("Load: %v", err)
		}
		quarantined, _ := filepath.Glob(store.Path() + ".corrupt-*")
		if len(quarantined) != 1 {
			t.Fatalf("quarantined files = %v, want 1", quarantined)
		}
	})
	t.Run("version one with no tasks is a loaded empty board", func(t *testing.T) {
		dir := t.TempDir()
		store := New(dir, "s")
		writeDocument(t, store.Path(), Document{Version: DocumentVersion})
		board, err := store.Load(ctx, newManager(t))
		if err != nil || !board.Loaded || len(board.Tasks) != 0 {
			t.Fatalf("board = %+v (err %v)", board, err)
		}
	})
}

func TestAtomicWritesNeverExposePartialJSON(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	store := New(dir, "s")
	manager := newManager(t)
	for index := 0; index < 8; index++ {
		createTask(t, manager, fmt.Sprintf("row %d", index), nil)
	}
	if err := store.Save(ctx, manager); err != nil {
		t.Fatalf("initial Save: %v", err)
	}
	var (
		wg     sync.WaitGroup
		errCh  = make(chan error, 1)
		stopCh = make(chan struct{})
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		for index := 0; index < 80; index++ {
			if err := store.Save(ctx, manager); err != nil {
				select {
				case errCh <- err:
				default:
				}
				return
			}
		}
		close(stopCh)
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stopCh:
				return
			default:
			}
			data, err := os.ReadFile(store.Path())
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				select {
				case errCh <- err:
				default:
				}
				return
			}
			var document Document
			if err := json.Unmarshal(data, &document); err != nil {
				select {
				case errCh <- fmt.Errorf("partial document observed: %w (%q)", err, data):
				default:
				}
				return
			}
			if document.Version != DocumentVersion {
				select {
				case errCh <- fmt.Errorf("unexpected version %d", document.Version):
				default:
				}
				return
			}
		}
	}()
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("atomic write race: %v", err)
	}
	leftovers, err := filepath.Glob(filepath.Join(dir, SessionDirName, ".task-board-*.tmp"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("temporary files left behind: %v (err %v)", leftovers, err)
	}
}

func TestConcurrentCalls(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	shared := New(dir, "s")
	var (
		wg    sync.WaitGroup
		errCh = make(chan error, 16)
	)
	// Writers save different managers into the same document.
	for writer := 0; writer < 4; writer++ {
		wg.Add(1)
		go func(writer int) {
			defer wg.Done()
			manager := team.NewTaskManager()
			if _, err := manager.Create(ctx, rig.TeamTask{Subject: fmt.Sprintf("writer %d", writer)}); err != nil {
				errCh <- err
				return
			}
			for iteration := 0; iteration < 10; iteration++ {
				if err := shared.Save(ctx, manager); err != nil {
					errCh <- err
					return
				}
			}
		}(writer)
	}
	// Readers load the document concurrently into fresh managers.
	for reader := 0; reader < 4; reader++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for iteration := 0; iteration < 10; iteration++ {
				if _, err := New(dir, "s").Load(ctx, team.NewTaskManager()); err != nil {
					errCh <- err
					return
				}
			}
		}()
	}
	// Tombstone updates race with saves on the shared store.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for iteration := 0; iteration < 20; iteration++ {
			shared.MarkDeleted(fmt.Sprintf("task-%d", iteration))
			shared.ClearDeleted(fmt.Sprintf("task-%d", iteration))
			_ = shared.Deleted()
		}
	}()
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent call failed: %v", err)
	}
	// The final document must still be valid.
	board, err := New(dir, "s").Load(ctx, team.NewTaskManager())
	if err != nil {
		t.Fatalf("final Load: %v", err)
	}
	if !board.Loaded || len(board.Tasks) == 0 {
		t.Fatalf("final board = %+v", board)
	}
}

func TestDeleteRetiresStore(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	store := New(dir, "s")
	manager := newManager(t)
	createTask(t, manager, "task", nil)
	if err := store.Save(ctx, manager); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := store.Delete(); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(store.Path()); !os.IsNotExist(err) {
		t.Fatalf("document still exists: %v", err)
	}
	// A late save must never recreate the removed document.
	if err := store.Save(ctx, manager); err != nil {
		t.Fatalf("Save after Delete: %v", err)
	}
	if _, err := os.Stat(store.Path()); !os.IsNotExist(err) {
		t.Fatalf("late Save recreated the document: %v", err)
	}
	if err := store.Delete(); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
}

func TestEmptyDirDisablesPersistence(t *testing.T) {
	ctx := context.Background()
	store := New("", "")
	if store.Path() != "" {
		t.Fatalf("Path = %q, want empty", store.Path())
	}
	manager := newManager(t)
	createTask(t, manager, "memory only", nil)
	if err := store.Save(ctx, manager); err != nil {
		t.Fatalf("Save: %v", err)
	}
	board, err := store.Load(ctx, newManager(t))
	if err != nil || board.Loaded || len(board.Tasks) != 0 {
		t.Fatalf("board = %+v (err %v)", board, err)
	}
	store.MarkDeleted("task-1")
	if got := store.Deleted(); !reflect.DeepEqual(got, []string{"task-1"}) {
		t.Fatalf("Deleted = %v", got)
	}
	store.ClearDeleted("task-1")
	if err := store.Delete(); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestMigrateLegacy(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	legacyStore := New(dir, "")
	legacyManager := newManager(t)
	first := createTask(t, legacyManager, "legacy one", func(task *rig.TeamTask) {
		task.Created = testStart
	})
	second := createTask(t, legacyManager, "legacy two", func(task *rig.TeamTask) {
		task.Created = testStart.Add(time.Second)
	})
	if _, err := legacyManager.Update(ctx, second.ID, rig.TeamTaskUpdate{AddBlocked: []string{first.ID}}); err != nil {
		t.Fatalf("legacy dependency: %v", err)
	}
	if err := legacyStore.Save(ctx, legacyManager); err != nil {
		t.Fatalf("legacy Save: %v", err)
	}
	legacyBytes, err := os.ReadFile(legacyStore.Path())
	if err != nil {
		t.Fatalf("read legacy: %v", err)
	}

	targetStore := New(dir, "session-a")
	targetManager := newManager(t)
	created, err := targetStore.MigrateLegacy(ctx, targetManager, legacyStore.Path())
	if err != nil {
		t.Fatalf("MigrateLegacy: %v", err)
	}
	if created != 2 {
		t.Fatalf("created = %d, want 2", created)
	}
	if _, err := os.Stat(legacyStore.Path()); !os.IsNotExist(err) {
		t.Fatalf("legacy document still exists: %v", err)
	}
	board, err := New(dir, "session-a").Load(ctx, newManager(t))
	if err != nil || !board.Loaded || len(board.Tasks) != 2 {
		t.Fatalf("migrated board = %+v (err %v)", board, err)
	}
	if got := board.Tasks[1].BlockedBy; !reflect.DeepEqual(got, []string{board.Tasks[0].ID}) {
		t.Fatalf("migrated dependency = %v", got)
	}
	// Re-running after a crash between save and remove must not duplicate.
	if err := os.WriteFile(legacyStore.Path(), legacyBytes, 0o644); err != nil {
		t.Fatalf("rewrite legacy: %v", err)
	}
	created, err = targetStore.MigrateLegacy(ctx, targetManager, legacyStore.Path())
	if err != nil {
		t.Fatalf("second MigrateLegacy: %v", err)
	}
	if created != 0 {
		t.Fatalf("second migration created %d tasks, want 0", created)
	}
	tasks, err := targetManager.List(ctx, rig.TeamTaskFilter{})
	if err != nil || len(tasks) != 2 {
		t.Fatalf("target tasks after retry = %d (err %v), want 2", len(tasks), err)
	}
	if _, err := os.Stat(legacyStore.Path()); !os.IsNotExist(err) {
		t.Fatalf("legacy document not removed on retry: %v", err)
	}
}

func TestLoadRejectsNonEmptyManager(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	store := New(dir, "s")
	manager := newManager(t)
	createTask(t, manager, "existing", nil)
	if err := store.Save(ctx, manager); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := store.Load(ctx, manager); !errors.Is(err, ErrManagerNotEmpty) {
		t.Fatalf("Load error = %v, want ErrManagerNotEmpty", err)
	}
}

func TestSanitizedSessionPaths(t *testing.T) {
	dir := t.TempDir()
	left := New(dir, "team/a b")
	right := New(dir, "team/a-b")
	if left.Path() == right.Path() {
		t.Fatalf("distinct sessions share a path: %q", left.Path())
	}
	if !strings.Contains(filepath.Base(left.Path()), "team_a_b-") {
		t.Fatalf("sanitized name = %q", filepath.Base(left.Path()))
	}
	if got := New(dir, "").Path(); got != filepath.Join(dir, LegacyFileName) {
		t.Fatalf("legacy path = %q", got)
	}
}

// taskIDs 返回任务的 id 列表。
func taskIDs(tasks []rig.TeamTask) []string {
	ids := make([]string, 0, len(tasks))
	for _, task := range tasks {
		ids = append(ids, task.ID)
	}
	return ids
}

// newManager 返回一个空的内存任务板。
func newManager(t *testing.T) *team.TaskManager {
	t.Helper()
	return team.NewTaskManager()
}

func TestAtomicWriteFailureLeavesNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	store := New(dir, "s")
	// Make the document path a directory so the final rename must fail.
	if err := os.MkdirAll(store.Path(), 0o755); err != nil {
		t.Fatalf("MkdirAll path-as-dir: %v", err)
	}
	manager := newManager(t)
	createTask(t, manager, "task", nil)
	if err := store.Save(ctx, manager); err == nil {
		t.Fatalf("Save succeeded with a directory at the document path")
	}
	leftovers, err := filepath.Glob(filepath.Join(dir, SessionDirName, ".task-board-*.tmp"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("temporary files left behind after a failed save: %v (err %v)", leftovers, err)
	}
}

func TestCanceledContextDoesNotWrite(t *testing.T) {
	dir := t.TempDir()
	store := New(dir, "s")
	manager := newManager(t)
	createTask(t, manager, "task", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.Save(ctx, manager); !errors.Is(err, context.Canceled) {
		t.Fatalf("Save error = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(store.Path()); !os.IsNotExist(err) {
		t.Fatalf("canceled Save wrote a document: %v", err)
	}
	if _, err := store.Load(ctx, newManager(t)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Load error = %v, want context.Canceled", err)
	}
}

func TestOptionsAndTombstoneAPI(t *testing.T) {
	dir := t.TempDir()
	store := New(dir, "session-a", WithMaxTasks(3), WithInProgressOwner("owner-x"))
	if store.Path() == "" {
		t.Fatal("persistent store Path is empty")
	}
	store.SetDeleted([]string{"task-1", "task-2"})
	if got := store.Deleted(); !reflect.DeepEqual(got, []string{"task-1", "task-2"}) {
		t.Fatalf("Deleted = %v", got)
	}
	store.MarkDeleted("task-3")
	store.MarkDeleted("task-3")
	if got := store.Deleted(); len(got) != 3 {
		t.Fatalf("MarkDeleted = %v", got)
	}
	store.ClearDeleted("task-2")
	if got := store.Deleted(); !reflect.DeepEqual(got, []string{"task-1", "task-3"}) {
		t.Fatalf("ClearDeleted = %v", got)
	}

	manager := team.NewTaskManager()
	if _, err := store.Load(context.Background(), manager); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := store.Save(context.Background(), manager); err != nil {
		t.Fatalf("Save: %v", err)
	}

	disabled := New("", "session-a")
	if disabled.Path() != "" {
		t.Fatalf("disabled store Path = %q", disabled.Path())
	}
	if err := disabled.Save(context.Background(), manager); err != nil {
		t.Fatalf("disabled Save: %v", err)
	}
	if err := disabled.Delete(); err != nil {
		t.Fatalf("disabled Delete: %v", err)
	}
}
