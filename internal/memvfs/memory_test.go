package memvfs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/memoryvfs"
)

func TestRememberCreatesThenMergesDuplicate(t *testing.T) {
	store := newStore(t, memoryvfs.WithClock(testClock))
	ctx := context.Background()

	created, err := store.Remember(ctx, memEntry(
		"mem://user/self/preferences/tabs", "preferences",
		"prefers tabs over spaces", "tabs for indentation in go code"))
	if err != nil {
		t.Fatalf("Remember: %v", err)
	}
	if created.Version != 1 {
		t.Fatalf("version = %d, want 1", created.Version)
	}
	if created.Tokens == 0 {
		t.Fatal("tokens should be computed")
	}

	// 完全相同（不同 URI）→ 视为重复：返回已有条目，不新增。
	duplicate, err := store.Remember(ctx, memEntry(
		"mem://user/self/preferences/tabs-copy", "preferences",
		"prefers tabs over spaces", "tabs for indentation in go code"))
	if err != nil {
		t.Fatalf("Remember duplicate: %v", err)
	}
	if duplicate.URI != "mem://user/self/preferences/tabs" {
		t.Fatalf("duplicate returned %s", duplicate.URI)
	}
	if duplicate.Version != 1 {
		t.Fatalf("duplicate should not be rewritten, version = %d", duplicate.Version)
	}
	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Entries != 1 {
		t.Fatalf("entries = %d, want 1", stats.Entries)
	}
}

func TestRememberMergesSimilarEntry(t *testing.T) {
	store := newStore(t, memoryvfs.WithClock(testClock))
	ctx := context.Background()

	existing := memoryvfs.Entry{
		URI:     "mem://user/self/preferences/theme",
		Kind:    memoryvfs.KindMemory,
		Type:    "preferences",
		Summary: "prefers dark mode theme for editor always",
		Content: "dark mode theme editor",
		Tags:    map[string]string{"topic": "editor"},
	}
	if _, err := store.Remember(ctx, existing); err != nil {
		t.Fatalf("Remember: %v", err)
	}

	merged, err := store.Remember(ctx, memoryvfs.Entry{
		URI:     "mem://user/self/preferences/theme-extra",
		Kind:    memoryvfs.KindMemory,
		Type:    "preferences",
		Summary: "prefers dark mode theme for editor always plus high contrast",
		Content: "dark mode theme editor high contrast",
		Tags:    map[string]string{"source": "chat"},
	})
	if err != nil {
		t.Fatalf("Remember merge: %v", err)
	}
	if merged.URI != "mem://user/self/preferences/theme" {
		t.Fatalf("merged into %s", merged.URI)
	}
	if merged.Version != 2 {
		t.Fatalf("merged version = %d, want 2", merged.Version)
	}
	if !strings.Contains(merged.Content, "high contrast") {
		t.Fatalf("content not appended: %q", merged.Content)
	}
	if merged.Tags["topic"] != "editor" || merged.Tags["source"] != "chat" {
		t.Fatalf("tags not merged: %+v", merged.Tags)
	}
	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Entries != 1 {
		t.Fatalf("entries = %d, want 1", stats.Entries)
	}
}

func TestRememberScopesByParentAndType(t *testing.T) {
	store := newStore(t, memoryvfs.WithClock(testClock))
	ctx := context.Background()
	if _, err := store.Remember(ctx, memEntry("mem://user/self/preferences/theme", "preferences", "prefers dark mode", "dark")); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	// 同文本但不同 scope：必须新建。
	elsewhere, err := store.Remember(ctx, memEntry("mem://resources/notes/theme", "preferences", "prefers dark mode", "dark"))
	if err != nil {
		t.Fatalf("Remember: %v", err)
	}
	if elsewhere.URI != "mem://resources/notes/theme" {
		t.Fatalf("cross-scope merge: %s", elsewhere.URI)
	}
	// 同 scope 不同 type：也必须新建。
	otherType, err := store.Remember(ctx, memEntry("mem://user/self/preferences/theme-event", "events", "prefers dark mode", "dark"))
	if err != nil {
		t.Fatalf("Remember: %v", err)
	}
	if otherType.URI != "mem://user/self/preferences/theme-event" {
		t.Fatalf("cross-type merge: %s", otherType.URI)
	}
}

func TestRememberValidationAndForget(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	_, err := store.Remember(ctx, memoryvfs.Entry{URI: "mem://user/self/preferences/x", Kind: memoryvfs.KindMemory})
	wantErrIs(t, err, memoryvfs.ErrInvalidEntry, "remember without type")

	created, err := store.Remember(ctx, memEntry("mem://user/self/profile/me", "profile", "engineer", "go"))
	if err != nil {
		t.Fatalf("Remember: %v", err)
	}
	if err := store.Forget(ctx, created.URI); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if _, err := store.Stat(ctx, created.URI); !errorsIs(err, memoryvfs.ErrNotFound) {
		t.Fatalf("forget left entry: %v", err)
	}
	err = store.Forget(ctx, "mem://user/self/profile/me")
	wantErrIs(t, err, memoryvfs.ErrNotFound, "forget twice")
}

func TestRememberTrashKeepsSnapshot(t *testing.T) {
	dir := t.TempDir()
	store := newStoreAt(t, dir)
	created, err := store.Remember(context.Background(), memEntry("mem://user/self/events/done", "events", "shipped feature", "detail"))
	if err != nil {
		t.Fatalf("Remember: %v", err)
	}
	if err := store.Forget(context.Background(), created.URI); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "trash"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("forget did not write a trash snapshot")
	}
	data, err := os.ReadFile(filepath.Join(dir, "trash", entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "mem://user/self/events/done") {
		t.Fatalf("trash snapshot = %s", data)
	}
}
