package memvfs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/memoryvfs"
)

func TestSnapshotDiffRestore(t *testing.T) {
	dir := t.TempDir()
	store := newStoreAt(t, dir, memoryvfs.WithClock(testClock))
	ctx := context.Background()

	mustWrite(t, store, memEntry("mem://user/self/profile/me", "profile", "engineer", "go developer"))
	mustWrite(t, store, memEntry("mem://user/self/preferences/tabs", "preferences", "prefers tabs", "tabs"))

	first, err := store.Snapshot(ctx, "before")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if first.ID == "" || first.Label != "before" || first.Entries != 2 {
		t.Fatalf("snapshot = %+v", first)
	}
	if first.Bytes <= 0 {
		t.Fatalf("snapshot bytes = %d", first.Bytes)
	}
	snapshotFile := filepath.Join(dir, "snapshots", first.ID+".json")
	info, err := os.Stat(snapshotFile)
	if err != nil {
		t.Fatalf("snapshot file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("snapshot perm = %o, want 0600", perm)
	}

	mustWrite(t, store, memEntry("mem://user/self/profile/me", "profile", "engineer", "rust developer"))
	mustWrite(t, store, memEntry("mem://user/self/cases/new", "cases", "new case", "body"))
	second, err := store.Snapshot(ctx, "after-write")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	diff, err := store.Diff(ctx, first.ID, second.ID)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if diff == "" {
		t.Fatal("no diff between snapshots")
	}

	list, err := store.Snapshots(ctx)
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("snapshots = %+v", list)
	}

	if err := store.Restore(ctx, first.ID); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	restored := mustRead(t, store, "mem://user/self/profile/me", memoryvfs.Level2)
	if restored.Content != "go developer" {
		t.Fatalf("restored content = %q", restored.Content)
	}
	if _, err := store.Stat(ctx, "mem://user/self/cases/new"); !errorsIs(err, memoryvfs.ErrNotFound) {
		t.Fatalf("post-snapshot entry survived restore: %v", err)
	}

	// 恢复后的磁盘状态必须和内存一致（重开验证）。
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := Open(dir, memoryvfs.WithClock(testClock))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	for _, uri := range []memoryvfs.URI{"mem://user/self/profile/me", "mem://user/self/preferences/tabs"} {
		if _, err := reopened.Stat(ctx, uri); err != nil {
			t.Fatalf("restored entry %s missing on disk: %v", uri, err)
		}
	}

	if err := reopened.Restore(ctx, "nope"); !errorsIs(err, memoryvfs.ErrNotFound) {
		t.Fatalf("restore unknown = %v", err)
	}
	if _, err := reopened.Diff(ctx, "nope", first.ID); !errorsIs(err, memoryvfs.ErrNotFound) {
		t.Fatalf("diff unknown = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "snapshots", "broken.json"), []byte("{oops"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Restore(ctx, "broken"); !errorsIs(err, memoryvfs.ErrCorrupt) {
		t.Fatalf("restore corrupt = %v", err)
	}
}

func TestStats(t *testing.T) {
	store := newStore(t, memoryvfs.WithClock(testClock))
	ctx := context.Background()
	mustWrite(t, store, memEntry("mem://user/self/profile/a", "profile", "alpha", "body"))
	mustWrite(t, store, memEntry("mem://user/self/preferences/b", "preferences", "beta", ""))
	mustWrite(t, store, memEntry("mem://user/self/preferences/c", "preferences", "gamma", ""))
	if err := store.Capture(ctx, "sess-stats", memoryvfs.Message{Role: "user", Content: "hello"}); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if _, err := store.Snapshot(ctx, "s"); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Entries != 3 {
		t.Fatalf("entries = %d", stats.Entries)
	}
	if stats.Dirs < 2 {
		t.Fatalf("dirs = %d", stats.Dirs)
	}
	if stats.Bytes <= 0 || stats.Tokens <= 0 {
		t.Fatalf("bytes/tokens = %d/%d", stats.Bytes, stats.Tokens)
	}
	if stats.ByKind[memoryvfs.KindMemory] != 3 {
		t.Fatalf("by kind = %+v", stats.ByKind)
	}
	if stats.ByType["preferences"] != 2 || stats.ByType["profile"] != 1 {
		t.Fatalf("by type = %+v", stats.ByType)
	}
	if stats.Sessions != 1 {
		t.Fatalf("sessions = %d", stats.Sessions)
	}
	if stats.Snapshots != 1 {
		t.Fatalf("snapshots = %d", stats.Snapshots)
	}
	if stats.Vectors != 0 {
		t.Fatalf("vectors = %d (no embedder)", stats.Vectors)
	}
	if stats.Oldest.IsZero() || stats.Newest.IsZero() {
		t.Fatalf("timestamps = %v / %v", stats.Oldest, stats.Newest)
	}
}

func TestGCTTLAndEviction(t *testing.T) {
	now := testNow
	clock := func() time.Time { return now }
	store := newStore(t, memoryvfs.WithClock(clock), memoryvfs.WithTTL(24*time.Hour), memoryvfs.WithMaxEntries(0))
	ctx := context.Background()
	mustWrite(t, store, memEntry("mem://user/self/events/one", "events", "one", "body"))
	mustWrite(t, store, memEntry("mem://user/self/events/two", "events", "two", "body"))

	dry, err := store.GC(ctx, memoryvfs.GCOptions{DryRun: true})
	if err != nil {
		t.Fatalf("GC dry: %v", err)
	}
	if dry.Expired != 0 {
		t.Fatalf("nothing should be expired yet: %+v", dry)
	}
	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Entries != 2 {
		t.Fatalf("dry run deleted entries: %d", stats.Entries)
	}

	now = now.Add(48 * time.Hour)
	preview, err := store.GC(ctx, memoryvfs.GCOptions{DryRun: true})
	if err != nil {
		t.Fatalf("GC dry: %v", err)
	}
	if preview.Expired != 2 {
		t.Fatalf("expired = %d, want 2", preview.Expired)
	}
	stats, err = store.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Entries != 2 {
		t.Fatalf("dry run deleted entries: %d", stats.Entries)
	}

	applied, err := store.GC(ctx, memoryvfs.GCOptions{})
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if applied.Expired != 2 || applied.RemovedBytes == 0 {
		t.Fatalf("gc = %+v", applied)
	}
	stats, err = store.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Entries != 0 {
		t.Fatalf("entries after gc = %d", stats.Entries)
	}
}

func TestGCEvictsDownToMaxEntriesAndKeepsPinned(t *testing.T) {
	now := testNow
	clock := func() time.Time { return now }
	store := newStore(t, memoryvfs.WithClock(clock))
	ctx := context.Background()

	pinned := memEntry("mem://user/self/profile/pinned", "profile", "pinned note", "body")
	pinned.Pinned = true
	mustWrite(t, store, pinned)
	for index := 0; index < 4; index++ {
		now = now.Add(time.Hour)
		mustWrite(t, store, memEntry(
			"mem://user/self/cases/case"+string(rune('a'+index)),
			"cases", "case "+string(rune('a'+index)), "body"))
	}

	preview, err := store.GC(ctx, memoryvfs.GCOptions{MaxEntries: 2, DryRun: true})
	if err != nil {
		t.Fatalf("GC dry: %v", err)
	}
	if preview.Evicted != 3 {
		t.Fatalf("evicted preview = %+v", preview)
	}
	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Entries != 5 {
		t.Fatalf("dry run evicted: %d", stats.Entries)
	}

	result, err := store.GC(ctx, memoryvfs.GCOptions{MaxEntries: 2})
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if result.Evicted != 3 {
		t.Fatalf("evicted = %+v", result)
	}
	if result.Kept != 2 {
		t.Fatalf("kept = %d, want 2", result.Kept)
	}
	if _, err := store.Stat(ctx, "mem://user/self/profile/pinned"); err != nil {
		t.Fatalf("pinned entry evicted: %v", err)
	}
	stats, err = store.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Entries != 2 {
		t.Fatalf("entries = %d, want 2", stats.Entries)
	}
}

func TestRebuildWithEmbedderWritesVectors(t *testing.T) {
	dir := t.TempDir()
	embedder := &fakeEmbedder{dimension: 32}
	store := newStoreAt(t, dir, memoryvfs.WithClock(testClock), memoryvfs.WithEmbedder(embedder))
	ctx := context.Background()
	mustWrite(t, store, memEntry("mem://user/self/preferences/dark", "preferences", "prefers dark mode", "dark mode"))
	mustWrite(t, store, memEntry("mem://user/self/preferences/tabs", "preferences", "prefers tabs", "tabs"))

	if err := store.Rebuild(ctx); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	vectorsPath := filepath.Join(dir, "index", "vectors.json")
	data, err := os.ReadFile(vectorsPath)
	if err != nil {
		t.Fatalf("vectors file: %v", err)
	}
	if !strings.Contains(string(data), "mem://user/self/preferences/dark") {
		t.Fatalf("vectors = %s", data)
	}
	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Vectors != 2 {
		t.Fatalf("vectors = %d", stats.Vectors)
	}

	// 失败的 Embedder 必须保留旧向量并返回错误。
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	broken := &fakeEmbedder{dimension: 32, err: errEmbedFail}
	reopened, err := Open(dir, memoryvfs.WithClock(testClock), memoryvfs.WithEmbedder(broken))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if err := reopened.Rebuild(ctx); err == nil {
		t.Fatal("rebuild with failing embedder should error")
	}
	after, err := os.ReadFile(vectorsPath)
	if err != nil {
		t.Fatalf("vectors file after failure: %v", err)
	}
	if string(after) != string(data) {
		t.Fatal("failing rebuild overwrote vectors")
	}
	if _, err := reopened.Find(ctx, memoryvfs.Query{Text: "prefers"}); err != nil {
		t.Fatalf("Find after failed rebuild: %v", err)
	}
}

func TestExportImportRoundTrip(t *testing.T) {
	source := newStore(t, memoryvfs.WithClock(testClock))
	ctx := context.Background()
	mustWrite(t, source, memEntry("mem://user/self/profile/me", "profile", "engineer", "go"))
	mustWrite(t, source, memEntry("mem://user/self/preferences/tabs", "preferences", "tabs", "tabs"))
	mustWrite(t, source, memEntry("mem://resources/docs/go", "profile", "go guide", "guide"))
	if err := source.Capture(ctx, "sess-pack", memoryvfs.Message{Role: "user", Content: "hello"}); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if _, err := source.Commit(ctx, "sess-pack", memoryvfs.CommitOptions{}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	pack, err := source.Export(ctx, "mem://user/self")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if pack.Version != 1 {
		t.Fatalf("pack version = %d", pack.Version)
	}
	var scopedMemories, scopedDirs int
	for _, entry := range pack.Entries {
		if entry.Kind == memoryvfs.KindDir {
			scopedDirs++
		} else {
			scopedMemories++
		}
	}
	if scopedMemories != 2 || scopedDirs == 0 {
		t.Fatalf("scoped export = %d memories / %d dirs (%+v)", scopedMemories, scopedDirs, pack.Entries)
	}
	full, err := source.Export(ctx, "")
	if err != nil {
		t.Fatalf("Export all: %v", err)
	}
	var sawSession bool
	for _, entry := range full.Entries {
		if entry.Kind == memoryvfs.KindSession {
			sawSession = true
		}
	}
	if !sawSession {
		t.Fatal("full export should include session archives")
	}

	target := newStore(t, memoryvfs.WithClock(testClock))
	result, err := target.Import(ctx, pack, memoryvfs.ImportOptions{})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if result.Imported != len(pack.Entries) || result.Skipped != 0 {
		t.Fatalf("import = %+v (entries %d)", result, len(pack.Entries))
	}
	got := mustRead(t, target, "mem://user/self/profile/me", memoryvfs.Level2)
	if got.Content != "go" {
		t.Fatalf("imported content = %q", got.Content)
	}

	again, err := target.Import(ctx, pack, memoryvfs.ImportOptions{})
	if err != nil {
		t.Fatalf("Import again: %v", err)
	}
	if again.Skipped != len(pack.Entries) {
		t.Fatalf("second import = %+v (entries %d)", again, len(pack.Entries))
	}

	overwrite, err := target.Import(ctx, pack, memoryvfs.ImportOptions{Overwrite: true})
	if err != nil {
		t.Fatalf("Import overwrite: %v", err)
	}
	if overwrite.Updated != len(pack.Entries) {
		t.Fatalf("overwrite import = %+v (entries %d)", overwrite, len(pack.Entries))
	}

	dryStore := newStore(t, memoryvfs.WithClock(testClock))
	dry, err := dryStore.Import(ctx, pack, memoryvfs.ImportOptions{DryRun: true})
	if err != nil {
		t.Fatalf("Import dry: %v", err)
	}
	if dry.Imported != len(pack.Entries) {
		t.Fatalf("dry import = %+v (entries %d)", dry, len(pack.Entries))
	}
	if _, err := dryStore.Stat(ctx, "mem://user/self/profile/me"); !errorsIs(err, memoryvfs.ErrNotFound) {
		t.Fatalf("dry run wrote entries: %v", err)
	}

	bad := pack
	bad.Version = 99
	if _, err := target.Import(ctx, bad, memoryvfs.ImportOptions{}); err == nil {
		t.Fatal("unknown pack version should fail")
	}
	invalid := memoryvfs.Pack{Version: 1, Entries: []memoryvfs.Entry{{URI: "bad", Kind: memoryvfs.KindMemory}}}
	if _, err := target.Import(ctx, invalid, memoryvfs.ImportOptions{}); err == nil {
		t.Fatal("invalid entry should fail")
	}
}

var errEmbedFail = &embedError{"embedder down"}

type embedError struct{ message string }

func (e *embedError) Error() string { return e.message }
