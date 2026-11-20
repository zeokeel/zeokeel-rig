package memvfs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/memoryvfs"
)

func TestOpenCreatesPrivateLayout(t *testing.T) {
	dir := t.TempDir()
	store := newStoreAt(t, dir, memoryvfs.WithClock(testClock))
	mustWrite(t, store, memEntry("mem://user/self/profile/me", "profile", "engineer", "works on Go"))

	for _, name := range []string{"entries", "dirs", "sessions", "snapshots", "trash"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("layout %s: %v", name, err)
		}
		if !info.IsDir() {
			t.Fatalf("layout %s is not a directory", name)
		}
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Fatalf("layout %s perm = %o, want 0700", name, perm)
		}
	}

	path := filepath.Join(dir, "entries", "user", "self", "profile", "me.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("entry file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("entry file perm = %o, want 0600", perm)
	}
}

func TestWriteReadLevelsAndStat(t *testing.T) {
	store := newStore(t, memoryvfs.WithClock(testClock))
	written := mustWrite(t, store, memoryvfs.Entry{
		URI:     "mem://resources/docs/guide",
		Kind:    memoryvfs.KindResource,
		Title:   "Guide",
		Content: "# Guide\n\nfirst paragraph.",
	})
	if written.Version != 1 {
		t.Fatalf("version = %d, want 1", written.Version)
	}
	if written.Summary == "" {
		t.Fatal("summary should be derived from content")
	}
	if written.Overview == "" {
		t.Fatal("overview should be derived from content")
	}
	if written.Created != testNow || written.Updated != testNow {
		t.Fatalf("timestamps = %v / %v, want %v", written.Created, written.Updated, testNow)
	}

	l0 := mustRead(t, store, "mem://resources/docs/guide", memoryvfs.Level0)
	if l0.Summary == "" || l0.Overview != "" || l0.Content != "" {
		t.Fatalf("L0 should be summary only, got %+v", l0)
	}
	l1 := mustRead(t, store, "mem://resources/docs/guide", memoryvfs.Level1)
	if l1.Summary == "" || l1.Overview == "" || l1.Content != "" {
		t.Fatalf("L1 should be summary+overview, got %+v", l1)
	}
	l2 := mustRead(t, store, "mem://resources/docs/guide", memoryvfs.Level2)
	if l2.Content != "# Guide\n\nfirst paragraph." {
		t.Fatalf("L2 content = %q", l2.Content)
	}

	stat := mustStat(t, store, "mem://resources/docs/guide")
	if stat.Kind != memoryvfs.KindResource || stat.Version != 1 {
		t.Fatalf("stat = %+v", stat)
	}
	if stat.Tokens != l2.TokenEstimate(memoryvfs.Level2) {
		t.Fatalf("tokens = %d, want %d", stat.Tokens, l2.TokenEstimate(memoryvfs.Level2))
	}
}

func TestWriteUsesInjectedSummarizer(t *testing.T) {
	summarizer := &mapSummarizer{summary: "S0", overview: "O1"}
	store := newStore(t, memoryvfs.WithSummarizer(summarizer))
	written := mustWrite(t, store, memoryvfs.Entry{
		URI:     "mem://resources/docs/sum",
		Kind:    memoryvfs.KindResource,
		Content: "body",
	})
	if written.Summary != "S0" || written.Overview != "O1" {
		t.Fatalf("summarizer not used: %+v", written)
	}
}

func TestWriteValidationAndErrors(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	if _, err := store.Write(ctx, memoryvfs.Entry{URI: "bad", Kind: memoryvfs.KindMemory, Type: "profile", Summary: "x"}); err == nil {
		t.Fatal("invalid uri should fail")
	}
	if _, err := store.Write(ctx, memoryvfs.Entry{URI: "mem://a/b", Kind: "nope", Summary: "x"}); err == nil {
		t.Fatal("invalid kind should fail")
	}
	if _, err := store.Write(ctx, memoryvfs.Entry{URI: "mem://a/b", Kind: memoryvfs.KindMemory, Summary: "x"}); err == nil {
		t.Fatal("memory entry without type should fail")
	}
	_, err := store.Write(ctx, memoryvfs.Entry{URI: "mem://a/empty", Kind: memoryvfs.KindResource})
	wantErrIs(t, err, memoryvfs.ErrSummaryRequired, "empty summary+content")

	long := strings.Repeat("x", memoryvfs.MaxSummaryBytes+1)
	_, err = store.Write(ctx, memEntry("mem://a/long", "profile", long, ""))
	wantErrIs(t, err, memoryvfs.ErrInvalidEntry, "oversized summary")
}

func TestWritePreservesCreatedAndBumpsVersion(t *testing.T) {
	store := newStore(t, memoryvfs.WithClock(testClock))
	first := mustWrite(t, store, memEntry("mem://user/self/profile/me", "profile", "one", "body"))
	second := mustWrite(t, store, memEntry("mem://user/self/profile/me", "profile", "two", "body2"))
	if second.Version != first.Version+1 {
		t.Fatalf("version = %d, want %d", second.Version, first.Version+1)
	}
	if !second.Created.Equal(first.Created) {
		t.Fatalf("created changed: %v -> %v", first.Created, second.Created)
	}
	if second.Summary != "two" {
		t.Fatalf("summary = %q", second.Summary)
	}
}

func TestWriteAdvancesUpdatedOnOverwrite(t *testing.T) {
	clock := testNow
	store := newStore(t, memoryvfs.WithClock(func() time.Time { return clock }))
	ctx := context.Background()
	first := mustWrite(t, store, memEntry("mem://user/self/profile/me", "profile", "one", "body"))
	clock = clock.Add(2 * time.Hour)
	second := mustWrite(t, store, memEntry("mem://user/self/profile/me", "profile", "two", "body2"))
	if !second.Updated.After(first.Updated) {
		t.Fatalf("updated did not advance: %v -> %v", first.Updated, second.Updated)
	}
	if !second.Created.Equal(first.Created) {
		t.Fatalf("created changed: %v -> %v", first.Created, second.Created)
	}

	// 目录不能被 Write 覆盖。
	if _, err := store.Mkdir(ctx, "mem://user/self/notes", "notes"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if _, err := store.Write(ctx, memoryvfs.Entry{URI: "mem://user/self/notes", Kind: memoryvfs.KindResource, Summary: "x"}); err == nil {
		t.Fatal("write onto a directory should fail")
	}
}

func TestStructuralConflicts(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	mustWrite(t, store, memEntry("mem://user/self/profile/a", "profile", "alpha", ""))

	_, err := store.Write(ctx, memEntry("mem://user/self/profile/a/child", "profile", "child", ""))
	wantErrIs(t, err, memoryvfs.ErrInvalidEntry, "nested under an entry")

	_, err = store.Write(ctx, memoryvfs.Entry{URI: "mem://user/self/dir-as-entry", Kind: memoryvfs.KindDir})
	wantErrIs(t, err, memoryvfs.ErrInvalidEntry, "kind=dir via Write")

	if _, err := store.Mkdir(ctx, "mem://user/self/profile/a/sub", "under entry"); err == nil {
		t.Fatal("mkdir under an entry should fail")
	}
}

func TestQuota(t *testing.T) {
	store := newStore(t, memoryvfs.WithMaxEntries(2))
	mustWrite(t, store, memEntry("mem://a/one", "profile", "one", ""))
	mustWrite(t, store, memEntry("mem://a/two", "profile", "two", ""))
	_, err := store.Write(context.Background(), memEntry("mem://a/three", "profile", "three", ""))
	wantErrIs(t, err, memoryvfs.ErrQuotaExceeded, "max entries")

	bytesStore := newStore(t, memoryvfs.WithMaxBytes(256))
	_, err = bytesStore.Write(context.Background(), memEntry("mem://a/big", "profile", "big", strings.Repeat("z", 512)))
	wantErrIs(t, err, memoryvfs.ErrQuotaExceeded, "max bytes")
}

func TestAppend(t *testing.T) {
	store := newStore(t, memoryvfs.WithClock(testClock))
	mustWrite(t, store, memEntry("mem://user/self/events/log", "events", "log", "first"))

	appended, err := store.Append(context.Background(), "mem://user/self/events/log", "second")
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if !strings.Contains(appended.Content, "first") || !strings.Contains(appended.Content, "second") {
		t.Fatalf("content = %q", appended.Content)
	}
	if appended.Version != 2 {
		t.Fatalf("version = %d, want 2", appended.Version)
	}
	if appended.Tokens != appended.TokenEstimate(memoryvfs.Level2) {
		t.Fatalf("tokens = %d, want %d", appended.Tokens, appended.TokenEstimate(memoryvfs.Level2))
	}

	_, err = store.Append(context.Background(), "mem://missing", "x")
	wantErrIs(t, err, memoryvfs.ErrNotFound, "append missing")
}

func TestMove(t *testing.T) {
	store := newStore(t, memoryvfs.WithClock(testClock))
	written := mustWrite(t, store, memEntry("mem://user/self/profile/a", "profile", "alpha", "body"))

	if err := store.Move(context.Background(), "mem://user/self/profile/a", "mem://user/self/profile/b"); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if _, err := store.Stat(context.Background(), "mem://user/self/profile/a"); !errorsIs(err, memoryvfs.ErrNotFound) {
		t.Fatalf("source still present: %v", err)
	}
	moved := mustStat(t, store, "mem://user/self/profile/b")
	if !moved.Created.Equal(written.Created) {
		t.Fatalf("created not preserved: %v -> %v", written.Created, moved.Created)
	}
	if moved.Version != written.Version+1 {
		t.Fatalf("version = %d, want %d", moved.Version, written.Version+1)
	}

	mustWrite(t, store, memEntry("mem://user/self/profile/c", "profile", "gamma", ""))
	err := store.Move(context.Background(), "mem://user/self/profile/b", "mem://user/self/profile/c")
	wantErrIs(t, err, memoryvfs.ErrExists, "move onto existing")

	err = store.Move(context.Background(), "mem://nope", "mem://nope2")
	wantErrIs(t, err, memoryvfs.ErrNotFound, "move missing")

	// 目录移动要带上全部子项。
	if _, err := store.Mkdir(context.Background(), "mem://user/self/old", "old dir"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	mustWrite(t, store, memEntry("mem://user/self/old/child", "profile", "child", ""))
	if err := store.Move(context.Background(), "mem://user/self/old", "mem://user/self/new"); err != nil {
		t.Fatalf("Move dir: %v", err)
	}
	if _, err := store.Stat(context.Background(), "mem://user/self/new/child"); err != nil {
		t.Fatalf("child not moved: %v", err)
	}
}

func TestDeleteWritesTrash(t *testing.T) {
	dir := t.TempDir()
	store := newStoreAt(t, dir)
	mustWrite(t, store, memEntry("mem://user/self/profile/gone", "profile", "gone", "body"))

	if err := store.Delete(context.Background(), "mem://user/self/profile/gone"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Stat(context.Background(), "mem://user/self/profile/gone"); !errorsIs(err, memoryvfs.ErrNotFound) {
		t.Fatalf("entry still present: %v", err)
	}
	trash, err := os.ReadDir(filepath.Join(dir, "trash"))
	if err != nil {
		t.Fatal(err)
	}
	if len(trash) == 0 {
		t.Fatal("trash snapshot not written")
	}

	err = store.Delete(context.Background(), "mem://user/self/profile/gone")
	wantErrIs(t, err, memoryvfs.ErrNotFound, "delete twice")

	// 目录递归删除。
	mustWrite(t, store, memEntry("mem://user/self/tree/a", "profile", "a", ""))
	mustWrite(t, store, memEntry("mem://user/self/tree/deep/b", "profile", "b", ""))
	if err := store.Delete(context.Background(), "mem://user/self/tree"); err != nil {
		t.Fatalf("Delete dir: %v", err)
	}
	if _, err := store.Stat(context.Background(), "mem://user/self/tree/deep/b"); !errorsIs(err, memoryvfs.ErrNotFound) {
		t.Fatalf("nested entry survived: %v", err)
	}
	if _, err := store.Stat(context.Background(), "mem://user/self/tree"); !errorsIs(err, memoryvfs.ErrNotFound) {
		t.Fatalf("dir survived: %v", err)
	}
}

func TestMkdirListAndTree(t *testing.T) {
	store := newStore(t, memoryvfs.WithClock(testClock))
	ctx := context.Background()
	created, err := store.Mkdir(ctx, "mem://user/self/notes", "team notes")
	if err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if created.Kind != memoryvfs.KindDir || created.Summary != "team notes" {
		t.Fatalf("mkdir entry = %+v", created)
	}
	mustWrite(t, store, memEntry("mem://user/self/notes/one", "events", "one", ""))
	mustWrite(t, store, memEntry("mem://user/self/notes/sub/two", "events", "two", ""))

	children, err := store.List(ctx, "mem://user/self/notes")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var sawEntry, sawDir bool
	for _, child := range children {
		switch child.URI {
		case "mem://user/self/notes/one":
			sawEntry = true
		case "mem://user/self/notes/sub":
			sawDir = true
			if child.Kind != memoryvfs.KindDir || child.Freshness == nil {
				t.Fatalf("dir child = %+v", child)
			}
			if child.Freshness.TotalEntries != 1 || child.Freshness.SampledEntries != 1 {
				t.Fatalf("freshness = %+v", child.Freshness)
			}
		}
	}
	if !sawEntry || !sawDir {
		t.Fatalf("List children = %+v", children)
	}

	_, err = store.List(ctx, "mem://user/self/notes/one")
	if err == nil {
		t.Fatal("List on entry should fail")
	}

	tree, err := store.Tree(ctx, "mem://user/self", 0)
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if len(tree) < 4 {
		t.Fatalf("tree = %+v", tree)
	}
	shallow, err := store.Tree(ctx, "mem://user/self", 1)
	if err != nil {
		t.Fatalf("Tree depth 1: %v", err)
	}
	for _, item := range shallow {
		if item.URI.Parent() != "mem://user/self" {
			t.Fatalf("depth 1 returned non-direct entry %s", item.URI)
		}
	}

	root, err := store.Stat(ctx, memoryvfs.RootURI)
	if err != nil {
		t.Fatalf("Stat root: %v", err)
	}
	if root.Kind != memoryvfs.KindDir || root.Freshness == nil || root.Freshness.TotalEntries != 1 {
		t.Fatalf("root = %+v", root)
	}
}

func TestFreshnessSamplingAndPendingReset(t *testing.T) {
	dir := t.TempDir()
	store := newStoreAt(t, dir, memoryvfs.WithClock(testClock))
	if _, err := store.Mkdir(context.Background(), "mem://bulk", "bulk dir"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	for index := 0; index < 40; index++ {
		mustWrite(t, store, memEntry(fmt.Sprintf("mem://bulk/e%02d", index), "profile", fmt.Sprintf("entry %d", index), ""))
	}
	dirEntry := mustStat(t, store, "mem://bulk")
	if dirEntry.Freshness == nil {
		t.Fatal("no freshness")
	}
	if dirEntry.Freshness.TotalEntries != 40 {
		t.Fatalf("total = %d", dirEntry.Freshness.TotalEntries)
	}
	if dirEntry.Freshness.SampledEntries != 32 {
		t.Fatalf("sampled = %d, want 32", dirEntry.Freshness.SampledEntries)
	}
	if dirEntry.Freshness.PendingChanges == 0 {
		t.Fatal("pending changes should be non-zero after child writes")
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := Open(dir, memoryvfs.WithClock(testClock))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	after := mustStat(t, reopened, "mem://bulk")
	if after.Freshness.PendingChanges != 0 {
		t.Fatalf("pending after rebuild = %d, want 0", after.Freshness.PendingChanges)
	}
	if after.Freshness.TotalEntries != 40 {
		t.Fatalf("total after reopen = %d", after.Freshness.TotalEntries)
	}
}

func TestPersistenceAcrossReopenAndCorruptIsolation(t *testing.T) {
	dir := t.TempDir()
	store := newStoreAt(t, dir, memoryvfs.WithClock(testClock))
	mustWrite(t, store, memEntry("mem://user/self/profile/keep", "profile", "keep me", "body"))
	mustWrite(t, store, memEntry("mem://user/self/profile/lost", "profile", "lost me", "body"))
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	corruptPath := filepath.Join(dir, "entries", "user", "self", "profile", "lost.json")
	if err := os.WriteFile(corruptPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dir, memoryvfs.WithClock(testClock))
	if err != nil {
		t.Fatalf("Open with corrupt file must succeed: %v", err)
	}
	defer reopened.Close()
	if len(reopened.LoadIssues()) == 0 {
		t.Fatal("expected diagnostic load issues")
	}
	if _, err := reopened.Stat(context.Background(), "mem://user/self/profile/keep"); err != nil {
		t.Fatalf("healthy entry lost: %v", err)
	}
	if _, err := reopened.Stat(context.Background(), "mem://user/self/profile/lost"); !errorsIs(err, memoryvfs.ErrNotFound) {
		t.Fatalf("corrupt entry should be skipped, got %v", err)
	}

	// 损坏文件不应挡住后续写入（覆盖它）。
	mustWrite(t, reopened, memEntry("mem://user/self/profile/lost", "profile", "rewritten", ""))
	if got := mustStat(t, reopened, "mem://user/self/profile/lost"); got.Summary != "rewritten" {
		t.Fatalf("summary = %q", got.Summary)
	}
}

func TestURItoPathTraversalGuard(t *testing.T) {
	for _, raw := range []string{"mem://a/../b", "mem://a/./b", "mem://a//b", "mem://A/b", "mem://a%b/c"} {
		if _, err := memoryvfs.ParseURI(raw); err == nil {
			t.Fatalf("contract should reject %q", raw)
		}
	}
	if _, err := relativeURIPath(memoryvfs.RootURI); err == nil {
		t.Fatal("root should not map to an entry path")
	}
	rel, err := relativeURIPath(memoryvfs.URI("mem://a/b-c_d.e"))
	if err != nil {
		t.Fatalf("relativeURIPath: %v", err)
	}
	if rel != "a/b-c_d.e" {
		t.Fatalf("rel = %q", rel)
	}
	// 兜底：即使有人绕过 ParseURI 直接构造 URI，路径映射也必须拒绝相对段。
	if _, err := relativeURIPath(memoryvfs.URI("mem://../etc/passwd")); err == nil {
		t.Fatal("traversal must be rejected")
	}
}

func TestConcurrentWrites(t *testing.T) {
	store := newStore(t, memoryvfs.WithClock(testClock))
	ctx := context.Background()

	var wait sync.WaitGroup
	errs := make(chan error, 64)
	for index := 0; index < 20; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			if _, err := store.Write(ctx, memEntry(fmt.Sprintf("mem://conc/n%02d", index), "profile", fmt.Sprintf("entry %d", index), "body")); err != nil {
				errs <- err
			}
		}(index)
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent distinct write: %v", err)
	}

	secondErrs := make(chan error, 20)
	var second sync.WaitGroup
	for index := 0; index < 20; index++ {
		second.Add(1)
		go func(index int) {
			defer second.Done()
			if _, err := store.Write(ctx, memEntry("mem://conc/same", "profile", fmt.Sprintf("same %d", index), "body")); err != nil {
				secondErrs <- err
			}
		}(index)
	}
	second.Wait()
	close(secondErrs)
	for err := range secondErrs {
		t.Fatalf("concurrent same write: %v", err)
	}

	if _, err := os.Stat(store.entryFilePath(mustParseURI(t, "mem://conc/same"))); err != nil {
		t.Fatalf("entry file: %v", err)
	}
	final := mustStat(t, store, "mem://conc/same")
	if final.Version != 20 {
		t.Fatalf("version = %d, want 20", final.Version)
	}
	var decoded map[string]any
	data, err := os.ReadFile(store.entryFilePath(final.URI))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("entry file is not valid json (torn write): %v", err)
	}
}

func TestCloseSemantics(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("second Close must be idempotent: %v", err)
	}
	if _, err := store.Stat(context.Background(), "mem://a/b"); !errorsIs(err, memoryvfs.ErrClosed) {
		t.Fatalf("Stat after close = %v", err)
	}
	if _, err := store.Write(context.Background(), memEntry("mem://a/b", "profile", "x", "")); !errorsIs(err, memoryvfs.ErrClosed) {
		t.Fatalf("Write after close = %v", err)
	}
}

func TestOpenMemoryIsProcessLocal(t *testing.T) {
	store := newMemStore(t, memoryvfs.WithClock(testClock))
	mustWrite(t, store, memEntry("mem://user/self/profile/me", "profile", "me", "body"))
	if store.Dir() != "" {
		t.Fatalf("memory store dir = %q", store.Dir())
	}
	if _, err := store.Read(context.Background(), "mem://user/self/profile/me", memoryvfs.Level2); err != nil {
		t.Fatalf("Read: %v", err)
	}
	// 内存后端不需要磁盘布局，但其余语义一致。
	if _, err := store.Snapshot(context.Background(), "mem"); err != nil {
		t.Fatalf("Snapshot on memory store: %v", err)
	}
}

func mustParseURI(t *testing.T, raw string) memoryvfs.URI {
	t.Helper()
	parsed, err := memoryvfs.ParseURI(raw)
	if err != nil {
		t.Fatalf("ParseURI(%q): %v", raw, err)
	}
	return parsed
}

func errorsIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		unwrapped, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapped.Unwrap()
	}
	return false
}
