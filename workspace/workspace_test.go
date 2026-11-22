package workspace_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/workspace"
)

// captureLogger records recovery warnings.
type captureLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *captureLogger) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *captureLogger) contains(sub string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	joined := strings.ToLower(strings.Join(l.lines, "\n"))
	return strings.Contains(joined, strings.ToLower(sub))
}

func makeDir(t *testing.T, base string, parts ...string) string {
	t.Helper()
	path := filepath.Join(append([]string{base}, parts...)...)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", path, err)
	}
	return path
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func mustNew(t *testing.T, dataDir string, cfg workspace.Config) *workspace.Service {
	t.Helper()
	service, err := workspace.New(dataDir, cfg)
	if err != nil {
		t.Fatalf("workspace.New(%q): %v", dataDir, err)
	}
	return service
}

func mustCreate(t *testing.T, service *workspace.Service, name, root string) workspace.Workspace {
	t.Helper()
	created, err := service.Create(context.Background(), name, root)
	if err != nil {
		t.Fatalf("Create(%q, %q): %v", name, root, err)
	}
	return created
}

// assertCurrent fails unless the service's current workspace is wantID ("" means
// "no current workspace").
func assertCurrent(t *testing.T, service *workspace.Service, wantID string) {
	t.Helper()
	current, ok, err := service.Current(context.Background())
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if wantID == "" {
		if ok {
			t.Fatalf("current workspace = %+v, want none", current)
		}
		return
	}
	if !ok || current.ID != wantID {
		t.Fatalf("current workspace = %+v/%t, want %q", current, ok, wantID)
	}
}

func canonicalPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", path, err)
	}
	return resolved
}

func TestNewMissingRegistryIsEmpty(t *testing.T) {
	dataDir := t.TempDir()
	service := mustNew(t, dataDir, workspace.Config{})
	if got, want := service.Path(), filepath.Join(dataDir, workspace.DefaultFileName); got != want {
		t.Fatalf("Path() = %q, want %q", got, want)
	}
	if list, err := service.List(context.Background()); err != nil || len(list) != 0 {
		t.Fatalf("List = %+v/%v, want empty", list, err)
	}
	if _, ok, err := service.Current(context.Background()); err != nil || ok {
		t.Fatalf("Current = %v/%v, want no workspace", ok, err)
	}
	if _, err := os.Stat(service.Path()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("New created the registry file (Stat err = %v), want lazy creation", err)
	}
}

func TestNewAbsoluteRegistryPathAndConfigValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "custom.json")
	service, err := workspace.New("", workspace.Config{Path: path})
	if err != nil {
		t.Fatalf("New with an absolute path and no data dir: %v", err)
	}
	if service.Path() != path {
		t.Fatalf("Path() = %q, want %q", service.Path(), path)
	}

	for name, cfg := range map[string]workspace.Config{
		"nul path":       {Path: "bad\x00path"},
		"negative read":  {ReadLimitBytes: -1},
		"negative depth": {SearchDepth: -1},
	} {
		if _, err := workspace.New(t.TempDir(), cfg); err == nil {
			t.Fatalf("New(%s) err = nil, want a config error", name)
		}
	}
	if _, err := workspace.New("", workspace.Config{}); err == nil {
		t.Fatal("New with no data dir and no path err = nil, want an error")
	}
}

func TestCreateListCurrentDeleteAndPersistence(t *testing.T) {
	base := t.TempDir()
	rootA := makeDir(t, base, "project-a")
	rootB := makeDir(t, base, "project-b")
	dataDir := t.TempDir()
	service := mustNew(t, dataDir, workspace.Config{})
	ctx := context.Background()

	first := mustCreate(t, service, "Project One", rootA)
	if !strings.HasPrefix(first.ID, "ws-") || first.Name != "Project One" || first.CreatedAt.IsZero() {
		t.Fatalf("created workspace = %+v", first)
	}
	if first.Root != canonicalPath(t, rootA) {
		t.Fatalf("root = %q, want canonical %q", first.Root, canonicalPath(t, rootA))
	}
	assertCurrent(t, service, first.ID)

	second := mustCreate(t, service, "", rootB)
	if second.Name != filepath.Base(rootB) {
		t.Fatalf("default name = %q, want %q", second.Name, filepath.Base(rootB))
	}
	list, err := service.List(ctx)
	if err != nil || len(list) != 2 || list[0].ID != first.ID || list[1].ID != second.ID {
		t.Fatalf("List = %+v/%v, want insertion order", list, err)
	}

	// The same canonical root is reused and becomes current again.
	duplicate := mustCreate(t, service, "Other name", rootA)
	if duplicate.ID != first.ID {
		t.Fatalf("duplicate Create ID = %q, want %q", duplicate.ID, first.ID)
	}
	assertCurrent(t, service, first.ID)
	if list, _ := service.List(ctx); len(list) != 2 {
		t.Fatalf("duplicate Create added an entry: %+v", list)
	}

	if opened, err := service.Open(ctx, second.ID); err != nil || opened.ID != second.ID {
		t.Fatalf("Open(second) = %+v/%v", opened, err)
	}
	if _, err := service.Open(ctx, "ws-missing"); !errors.Is(err, workspace.ErrWorkspaceNotFound) {
		t.Fatalf("Open(unknown) = %v, want ErrWorkspaceNotFound", err)
	}

	// The registry file is the documented document.
	raw, err := os.ReadFile(filepath.Join(dataDir, workspace.DefaultFileName))
	if err != nil {
		t.Fatalf("read registry: %v", err)
	}
	var document struct {
		Workspaces []struct {
			ID        string    `json:"id"`
			Name      string    `json:"name"`
			Root      string    `json:"root"`
			CreatedAt time.Time `json:"createdAt"`
		} `json:"workspaces"`
		CurrentID string `json:"currentId"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("registry JSON: %v", err)
	}
	if len(document.Workspaces) != 2 || document.CurrentID != second.ID {
		t.Fatalf("registry document = %+v, want two workspaces and current second", document)
	}

	// A fresh service over the same dataDir restores the registry.
	restarted := mustNew(t, dataDir, workspace.Config{})
	list, err = restarted.List(ctx)
	if err != nil || len(list) != 2 || list[0].ID != first.ID || list[1].ID != second.ID {
		t.Fatalf("restored List = %+v/%v", list, err)
	}
	assertCurrent(t, restarted, second.ID)

	if err := restarted.Delete(ctx, first.ID); err != nil {
		t.Fatalf("Delete(first): %v", err)
	}
	assertCurrent(t, restarted, second.ID)
	if err := restarted.Delete(ctx, second.ID); err != nil {
		t.Fatalf("Delete(second): %v", err)
	}
	assertCurrent(t, restarted, "")
	if err := restarted.Delete(ctx, second.ID); !errors.Is(err, workspace.ErrWorkspaceNotFound) {
		t.Fatalf("Delete(unknown) = %v, want ErrWorkspaceNotFound", err)
	}
}

func TestCreateValidation(t *testing.T) {
	base := t.TempDir()
	root := makeDir(t, base, "root")
	file := filepath.Join(base, "plain.txt")
	writeFile(t, file, []byte("x"))
	service := mustNew(t, t.TempDir(), workspace.Config{})
	ctx := context.Background()

	if _, err := service.Create(ctx, "x", ""); err == nil {
		t.Fatal("Create accepted an empty root")
	}
	if _, err := service.Create(ctx, "x", "bad\x00root"); err == nil {
		t.Fatal("Create accepted a NUL root")
	}
	if _, err := service.Create(ctx, "x", filepath.Join(base, "missing")); err == nil {
		t.Fatal("Create accepted a missing root")
	}
	if _, err := service.Create(ctx, "x", file); !errors.Is(err, workspace.ErrNotDirectory) {
		t.Fatalf("Create(file) = %v, want ErrNotDirectory", err)
	}

	created := mustCreate(t, service, "  ", root)
	if created.Root != canonicalPath(t, root) {
		t.Fatalf("root = %q, want canonical %q", created.Root, canonicalPath(t, root))
	}
	if created.Name != filepath.Base(root) {
		t.Fatalf("blank name = %q, want the root base name", created.Name)
	}

	// A symlinked root canonicalizes to its target.
	real := makeDir(t, base, "real")
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	fromLink := mustCreate(t, service, "via-link", link)
	if fromLink.Root != canonicalPath(t, real) {
		t.Fatalf("symlink root = %q, want canonical %q", fromLink.Root, canonicalPath(t, real))
	}
}

func TestRegistryCorruptFileQuarantined(t *testing.T) {
	dataDir := t.TempDir()
	path := filepath.Join(dataDir, workspace.DefaultFileName)
	writeFile(t, path, []byte("{broken"))
	logger := &captureLogger{}

	service := mustNew(t, dataDir, workspace.Config{Logger: logger.logf})
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("corrupt registry still at %s (err=%v)", path, err)
	}
	backups, globErr := filepath.Glob(path + ".corrupt-*")
	if globErr != nil || len(backups) != 1 {
		t.Fatalf("quarantine backups = %v/%v, want exactly one", backups, globErr)
	}
	preserved, err := os.ReadFile(backups[0])
	if err != nil || string(preserved) != "{broken" {
		t.Fatalf("quarantined content = %q/%v, want the original bytes", preserved, err)
	}
	if !logger.contains("corrupt") || !logger.contains("quarantine") {
		t.Fatalf("recovery was not logged: %v", logger.lines)
	}
	if list, err := service.List(context.Background()); err != nil || len(list) != 0 {
		t.Fatalf("List after quarantine = %+v/%v, want empty", list, err)
	}

	// The registry keeps working: a new workspace persists to the canonical path.
	root := makeDir(t, t.TempDir(), "recovered")
	mustCreate(t, service, "recovered", root)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("registry not rewritten after recovery: %v", err)
	}
}

func TestRegistryReadErrorQuarantined(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink-loop fault injection needs Unix symlinks")
	}
	dataDir := t.TempDir()
	path := filepath.Join(dataDir, workspace.DefaultFileName)
	if err := os.Symlink(path, path); err != nil {
		t.Skipf("cannot inject a symlink loop: %v", err)
	}
	if _, err := os.ReadFile(path); err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("fault injection invalid: ReadFile err = %v, want a non-NotExist read error", err)
	}
	logger := &captureLogger{}
	service := mustNew(t, dataDir, workspace.Config{Logger: logger.logf})
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("unreadable registry entry still at %s (err=%v)", path, err)
	}
	backups, globErr := filepath.Glob(path + ".corrupt-*")
	if globErr != nil || len(backups) != 1 {
		t.Fatalf("quarantine backups = %v/%v, want exactly one", backups, globErr)
	}
	if !logger.contains("could not be read") || !logger.contains("quarantine") {
		t.Fatalf("read-error recovery was not logged: %v", logger.lines)
	}
	if list, err := service.List(context.Background()); err != nil || len(list) != 0 {
		t.Fatalf("List after read-error quarantine = %+v/%v, want empty", list, err)
	}
	root := makeDir(t, t.TempDir(), "recovered")
	mustCreate(t, service, "recovered", root)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("registry not rewritten after recovery: %v", err)
	}
}

func TestRegistryBareArrayBlankAndUnknownCurrent(t *testing.T) {
	root := makeDir(t, t.TempDir(), "root")

	arrayDir := t.TempDir()
	writeFile(t, filepath.Join(arrayDir, workspace.DefaultFileName),
		[]byte(`[{"id":"w1","name":"one","root":`+quote(root)+`}]`))
	arrayService := mustNew(t, arrayDir, workspace.Config{})
	if list, err := arrayService.List(context.Background()); err != nil || len(list) != 1 || list[0].ID != "w1" {
		t.Fatalf("bare-array List = %+v/%v, want one workspace", list, err)
	}
	if _, ok, err := arrayService.Current(context.Background()); err != nil || ok {
		t.Fatalf("bare-array Current = %v/%v, want no current", ok, err)
	}

	blankDir := t.TempDir()
	writeFile(t, filepath.Join(blankDir, workspace.DefaultFileName), []byte("  \n"))
	if list, err := mustNew(t, blankDir, workspace.Config{}).List(context.Background()); err != nil || len(list) != 0 {
		t.Fatalf("blank registry List = %+v/%v, want empty", list, err)
	}

	unknownDir := t.TempDir()
	writeFile(t, filepath.Join(unknownDir, workspace.DefaultFileName),
		[]byte(`{"workspaces":[{"id":"w1","name":"one","root":`+quote(root)+`}],"currentId":"ws-gone"}`))
	unknownService := mustNew(t, unknownDir, workspace.Config{})
	if _, ok, err := unknownService.Current(context.Background()); err != nil || ok {
		t.Fatalf("Current with an unknown currentId = %v/%v, want no current", ok, err)
	}
}

func TestRegistryFiltersIncompleteRecords(t *testing.T) {
	root := makeDir(t, t.TempDir(), "root")
	dataDir := t.TempDir()
	document := map[string]any{
		"workspaces": []map[string]any{
			{"id": "", "name": "blank id", "root": root},
			{"id": "ws-blank-root", "name": "blank root", "root": "   "},
			{"id": "ws-ok", "name": "ok", "root": root},
		},
		"currentId": "  ws-ok  ",
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("marshal registry: %v", err)
	}
	writeFile(t, filepath.Join(dataDir, workspace.DefaultFileName), raw)

	service := mustNew(t, dataDir, workspace.Config{})
	list, err := service.List(context.Background())
	if err != nil || len(list) != 1 || list[0].ID != "ws-ok" {
		t.Fatalf("List = %+v/%v, want only the complete record", list, err)
	}
	assertCurrent(t, service, "ws-ok")
}

func TestPersistenceWriteFailureRollsBack(t *testing.T) {
	base := t.TempDir()
	rootA := makeDir(t, base, "a")
	rootB := makeDir(t, base, "b")
	dataDir := t.TempDir()
	service := mustNew(t, dataDir, workspace.Config{})
	first := mustCreate(t, service, "a", rootA)
	second := mustCreate(t, service, "b", rootB)
	if _, err := service.Open(context.Background(), first.ID); err != nil {
		t.Fatalf("Open(first): %v", err)
	}

	// Replace the registry file with a directory so the next atomic write fails.
	if err := os.Remove(service.Path()); err != nil {
		t.Fatalf("remove registry: %v", err)
	}
	if err := os.Mkdir(service.Path(), 0o700); err != nil {
		t.Fatalf("replace registry with directory: %v", err)
	}
	ctx := context.Background()
	if _, err := service.Create(ctx, "b again", rootB); err == nil {
		t.Fatal("Create with an unwritable registry returned nil error")
	}
	if _, err := service.Open(ctx, second.ID); err == nil {
		t.Fatal("Open with an unwritable registry returned nil error")
	}
	if err := service.Delete(ctx, second.ID); err == nil {
		t.Fatal("Delete with an unwritable registry returned nil error")
	}
	if _, err := service.Rename(ctx, first.ID, "renamed"); err == nil {
		t.Fatal("Rename with an unwritable registry returned nil error")
	}

	list, err := service.List(ctx)
	if err != nil || len(list) != 2 || list[0].ID != first.ID || list[0].Name != "a" {
		t.Fatalf("List after failed mutations = %+v/%v, want the original registry", list, err)
	}
	assertCurrent(t, service, first.ID)
}

func TestDeleteCurrentFallsBackToMostRecentRemaining(t *testing.T) {
	base := t.TempDir()
	dataDir := t.TempDir()
	service := mustNew(t, dataDir, workspace.Config{})
	ctx := context.Background()

	first := mustCreate(t, service, "first", makeDir(t, base, "first"))
	second := mustCreate(t, service, "second", makeDir(t, base, "second"))
	third := mustCreate(t, service, "third", makeDir(t, base, "third"))
	assertCurrent(t, service, third.ID)

	if err := service.Delete(ctx, third.ID); err != nil {
		t.Fatalf("Delete(current third): %v", err)
	}
	assertCurrent(t, service, second.ID)
	assertCurrent(t, mustNew(t, dataDir, workspace.Config{}), second.ID)

	if _, err := service.Open(ctx, first.ID); err != nil {
		t.Fatalf("Open(first): %v", err)
	}
	if err := service.Delete(ctx, first.ID); err != nil {
		t.Fatalf("Delete(current first): %v", err)
	}
	assertCurrent(t, service, second.ID)
	assertCurrent(t, mustNew(t, dataDir, workspace.Config{}), second.ID)

	fourth := mustCreate(t, service, "fourth", makeDir(t, base, "fourth"))
	if _, err := service.Open(ctx, second.ID); err != nil {
		t.Fatalf("Open(second): %v", err)
	}
	if err := service.Delete(ctx, fourth.ID); err != nil {
		t.Fatalf("Delete(non-current fourth): %v", err)
	}
	assertCurrent(t, service, second.ID)
	assertCurrent(t, mustNew(t, dataDir, workspace.Config{}), second.ID)
}

func TestDeleteCurrentPrefersCreatedAtOverRegistryOrder(t *testing.T) {
	base := t.TempDir()
	dataDir := t.TempDir()
	now := time.Now().UTC()
	document := map[string]any{
		"workspaces": []map[string]any{
			{"id": "ws-newest", "name": "newest", "root": makeDir(t, base, "newest"), "createdAt": now},
			{"id": "ws-oldest", "name": "oldest", "root": makeDir(t, base, "oldest"), "createdAt": now.Add(-2 * time.Hour)},
			{"id": "ws-middle", "name": "middle", "root": makeDir(t, base, "middle"), "createdAt": now.Add(-time.Hour)},
		},
		"currentId": "ws-middle",
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("marshal registry: %v", err)
	}
	writeFile(t, filepath.Join(dataDir, workspace.DefaultFileName), raw)

	service := mustNew(t, dataDir, workspace.Config{})
	if err := service.Delete(context.Background(), "ws-middle"); err != nil {
		t.Fatalf("Delete(current middle): %v", err)
	}
	assertCurrent(t, service, "ws-newest")
	assertCurrent(t, mustNew(t, dataDir, workspace.Config{}), "ws-newest")
}

func TestDeleteCurrentTieBreakIsDeterministic(t *testing.T) {
	base := t.TempDir()
	dataDir := t.TempDir()
	created := time.Now().UTC()
	document := map[string]any{
		"workspaces": []map[string]any{
			{"id": "ws-a", "name": "a", "root": makeDir(t, base, "a"), "createdAt": created.Add(-time.Hour)},
			{"id": "ws-b", "name": "b", "root": makeDir(t, base, "b"), "createdAt": created},
			{"id": "ws-c", "name": "c", "root": makeDir(t, base, "c"), "createdAt": created},
		},
		"currentId": "ws-a",
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("marshal registry: %v", err)
	}
	writeFile(t, filepath.Join(dataDir, workspace.DefaultFileName), raw)

	service := mustNew(t, dataDir, workspace.Config{})
	if err := service.Delete(context.Background(), "ws-a"); err != nil {
		t.Fatalf("Delete(current ws-a): %v", err)
	}
	assertCurrent(t, service, "ws-c")
	assertCurrent(t, mustNew(t, dataDir, workspace.Config{}), "ws-c")
}

func TestDeleteOnlyWorkspaceClearsCurrent(t *testing.T) {
	dataDir := t.TempDir()
	service := mustNew(t, dataDir, workspace.Config{})
	ctx := context.Background()
	only := mustCreate(t, service, "only", makeDir(t, t.TempDir(), "only"))

	if err := service.Delete(ctx, only.ID); err != nil {
		t.Fatalf("Delete(only): %v", err)
	}
	assertCurrent(t, service, "")
	assertCurrent(t, mustNew(t, dataDir, workspace.Config{}), "")

	if list, err := service.List(ctx); err != nil || len(list) != 0 {
		t.Fatalf("List after deleting the only workspace = %+v/%v", list, err)
	}
	raw, err := os.ReadFile(filepath.Join(dataDir, workspace.DefaultFileName))
	if err != nil {
		t.Fatalf("read registry: %v", err)
	}
	var document struct {
		Workspaces []json.RawMessage `json:"workspaces"`
		CurrentID  string            `json:"currentId"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("registry JSON: %v", err)
	}
	if len(document.Workspaces) != 0 || document.CurrentID != "" {
		t.Fatalf("registry after deleting the only workspace = %+v", document)
	}
}

func TestRenameKeepsIdentityAndCurrent(t *testing.T) {
	dataDir := t.TempDir()
	service := mustNew(t, dataDir, workspace.Config{})
	ctx := context.Background()
	first := mustCreate(t, service, "one", t.TempDir())
	second := mustCreate(t, service, "two", t.TempDir()) // becomes current

	renamed, err := service.Rename(ctx, first.ID, "  项目甲  ")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if renamed.ID != first.ID {
		t.Fatalf("renamed ID = %q, want %q", renamed.ID, first.ID)
	}
	if renamed.Name != "项目甲" {
		t.Fatalf("renamed name = %q, want the trimmed 项目甲", renamed.Name)
	}
	if renamed.Root != first.Root || !renamed.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("rename changed identity fields: got root %q created %v, want %q / %v",
			renamed.Root, renamed.CreatedAt, first.Root, first.CreatedAt)
	}

	reopened := mustNew(t, dataDir, workspace.Config{})
	listed, err := reopened.List(ctx)
	if err != nil || len(listed) != 2 {
		t.Fatalf("List after reopen = %+v/%v", listed, err)
	}
	if listed[0].Name != "项目甲" {
		t.Fatalf("persisted name = %q, want 项目甲", listed[0].Name)
	}
	assertCurrent(t, reopened, second.ID)

	// Renaming the current workspace updates its display name without moving
	// the selection.
	if _, err := service.Rename(ctx, second.ID, "当前项目"); err != nil {
		t.Fatalf("Rename(current): %v", err)
	}
	current, ok, err := service.Current(ctx)
	if err != nil || !ok || current.ID != second.ID || current.Name != "当前项目" {
		t.Fatalf("current after rename = %+v/%v/%v, want %q 当前项目", current, ok, err, second.ID)
	}
}

func TestRenameValidation(t *testing.T) {
	dataDir := t.TempDir()
	service := mustNew(t, dataDir, workspace.Config{})
	ctx := context.Background()
	alpha := mustCreate(t, service, "alpha", t.TempDir())
	beta := mustCreate(t, service, "beta", t.TempDir())

	for _, name := range []string{"", "   ", "\t\n"} {
		if _, err := service.Rename(ctx, alpha.ID, name); !errors.Is(err, workspace.ErrNameEmpty) {
			t.Fatalf("Rename(name %q) error = %v, want ErrNameEmpty", name, err)
		}
	}
	for _, name := range []string{"beta", "  BETA ", "Beta"} {
		if _, err := service.Rename(ctx, alpha.ID, name); !errors.Is(err, workspace.ErrNameTaken) {
			t.Fatalf("Rename(taken name %q) error = %v, want ErrNameTaken", name, err)
		}
	}
	if _, err := service.Rename(ctx, "ws-missing", "gamma"); !errors.Is(err, workspace.ErrWorkspaceNotFound) {
		t.Fatalf("Rename(unknown id) error = %v, want ErrWorkspaceNotFound", err)
	}
	if updated, err := service.Rename(ctx, alpha.ID, " alpha "); err != nil || updated.Name != "alpha" {
		t.Fatalf("Rename(self name) = %q, %v; want alpha, nil", updated.Name, err)
	}

	reopened := mustNew(t, dataDir, workspace.Config{})
	listed, err := reopened.List(ctx)
	if err != nil {
		t.Fatalf("List after reopen: %v", err)
	}
	names := map[string]string{}
	for _, item := range listed {
		names[item.ID] = item.Name
	}
	if names[alpha.ID] != "alpha" || names[beta.ID] != "beta" {
		t.Fatalf("names after the rejected renames = %v, want alpha/beta", names)
	}
}

func TestCustomRegistryPathAndDefaults(t *testing.T) {
	base := t.TempDir()
	root := makeDir(t, base, "root")
	writeFile(t, filepath.Join(root, "big.txt"), []byte(strings.Repeat("x", 10)))
	dataDir := t.TempDir()
	service := mustNew(t, dataDir, workspace.Config{
		Path:           "nested/workspaces.json",
		ReadLimitBytes: 3,
		SearchDepth:    1,
	})
	mustCreate(t, service, "custom", root)
	registryPath := filepath.Join(dataDir, "nested", "workspaces.json")
	if _, err := os.Stat(registryPath); err != nil {
		t.Fatalf("custom registry file missing: %v", err)
	}
	data, err := service.ReadFile(context.Background(), "big.txt", 0)
	if err != nil || len(data) != 3 {
		t.Fatalf("ReadFile(default) = %d/%v, want the configured 3-byte default", len(data), err)
	}
}

func TestRegistryFileAndDirectoryPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not meaningful on Windows")
	}
	dataDir := t.TempDir()
	service := mustNew(t, dataDir, workspace.Config{Path: "nested/workspaces.json"})
	mustCreate(t, service, "perms", makeDir(t, t.TempDir(), "root"))

	info, err := os.Stat(service.Path())
	if err != nil {
		t.Fatalf("Stat registry: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("registry mode = %o, want 600", got)
	}
	directoryInfo, err := os.Stat(filepath.Dir(service.Path()))
	if err != nil {
		t.Fatalf("Stat registry directory: %v", err)
	}
	if got := directoryInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("registry directory mode = %o, want 700", got)
	}
}

func TestAtomicWriteLeavesNoTemporaryFiles(t *testing.T) {
	dataDir := t.TempDir()
	service := mustNew(t, dataDir, workspace.Config{})
	ctx := context.Background()
	first := mustCreate(t, service, "one", makeDir(t, t.TempDir(), "one"))
	second := mustCreate(t, service, "two", makeDir(t, t.TempDir(), "two"))
	if _, err := service.Open(ctx, first.ID); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := service.Rename(ctx, second.ID, "renamed"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if err := service.Delete(ctx, second.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	temporaries, err := filepath.Glob(filepath.Join(dataDir, ".workspaces-*"))
	if err != nil {
		t.Fatalf("glob temporaries: %v", err)
	}
	if len(temporaries) != 0 {
		t.Fatalf("temporary registry files left behind: %v", temporaries)
	}
}

func TestConcurrentCreates(t *testing.T) {
	base := t.TempDir()
	dataDir := t.TempDir()
	service := mustNew(t, dataDir, workspace.Config{})
	const workers = 8
	roots := make([]string, 0, workers)
	for index := 0; index < workers; index++ {
		roots = append(roots, makeDir(t, base, fmt.Sprintf("project-%d", index)))
	}
	var group sync.WaitGroup
	errs := make(chan error, workers)
	for index := 0; index < workers; index++ {
		group.Add(1)
		go func(number int) {
			defer group.Done()
			if _, err := service.Create(context.Background(), fmt.Sprintf("p%d", number), roots[number]); err != nil {
				errs <- err
				return
			}
			_, _ = service.List(context.Background())
			_, _, _ = service.Current(context.Background())
			_, _ = service.Files(context.Background(), "")
			_, _ = service.Search(context.Background(), "project", 0)
		}(index)
	}
	group.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	list, err := service.List(context.Background())
	if err != nil || len(list) != workers {
		t.Fatalf("List after concurrent creates = %d/%v, want %d", len(list), err, workers)
	}
	ids := make(map[string]bool, workers)
	for _, item := range list {
		if ids[item.ID] {
			t.Fatalf("duplicate workspace ID %q", item.ID)
		}
		ids[item.ID] = true
	}
	reloaded, err := mustNew(t, dataDir, workspace.Config{}).List(context.Background())
	if err != nil {
		t.Fatalf("reloaded List: %v", err)
	}
	if len(reloaded) != workers {
		t.Fatalf("reloaded List = %d, want %d", len(reloaded), workers)
	}
}

func TestRegistryContextCancellation(t *testing.T) {
	service := mustNew(t, t.TempDir(), workspace.Config{})
	created := mustCreate(t, service, "one", makeDir(t, t.TempDir(), "one"))
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := service.List(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("List(cancelled) = %v, want context.Canceled", err)
	}
	if _, _, err := service.Current(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("Current(cancelled) = %v, want context.Canceled", err)
	}
	if _, err := service.Create(cancelled, "x", t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Create(cancelled) = %v, want context.Canceled", err)
	}
	if _, err := service.Open(cancelled, created.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("Open(cancelled) = %v, want context.Canceled", err)
	}
	if err := service.Delete(cancelled, created.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("Delete(cancelled) = %v, want context.Canceled", err)
	}
	if _, err := service.Rename(cancelled, created.ID, "x"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Rename(cancelled) = %v, want context.Canceled", err)
	}
	if list, err := service.List(context.Background()); err != nil || len(list) != 1 {
		t.Fatalf("List after cancelled calls = %+v/%v, want the workspace untouched", list, err)
	}
}

// quote returns a JSON string literal for building registry fixtures.
func quote(value string) string {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(raw)
}
