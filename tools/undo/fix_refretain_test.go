package undo_test

// 第 3 项修复的回归用例（TDD）：宿主必须能把 refs/rig/undo/* 的保留数调小。

import (
	"context"
	"testing"

	"github.com/zeokeel/zeokeel-rig/tools/undo"
)

// RefRetain 生效：20 次快照后只保留 5 个 ref。
func TestFixRefRetainOption(t *testing.T) {
	dir := newRepo(t)
	writeTree(t, dir, map[string]string{"a.txt": "a\n"})
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "base")

	for i := 0; i < 20; i++ {
		writeTree(t, dir, map[string]string{"a.txt": "content " + string(rune('a'+i)) + "\n"})
		if _, err := undo.Take(dir, undo.Options{RefRetain: 5}); err != nil {
			t.Fatalf("Take(%d): %v", i, err)
		}
	}
	if refs := refCount(t, dir); refs != 5 {
		t.Fatalf("RefRetain=5 时 ref 数 = %d, want 5", refs)
	}
}

// 导出的 Prune 可用（包级 + Tracker 方法两种入口）。
func TestFixPruneExported(t *testing.T) {
	dir := newRepo(t)
	writeTree(t, dir, map[string]string{"a.txt": "a\n"})
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "base")

	tracker := undo.NewTracker(dir, undo.Options{})
	for i := 0; i < 20; i++ {
		if _, err := tracker.Begin(); err != nil {
			t.Fatalf("Begin: %v", err)
		}
	}
	if refs := refCount(t, dir); refs != 20 {
		t.Fatalf("默认保留下 20 次快照的 ref 数 = %d, want 20", refs)
	}
	if err := tracker.Prune(3); err != nil {
		t.Fatalf("tracker.Prune: %v", err)
	}
	if refs := refCount(t, dir); refs != 3 {
		t.Fatalf("tracker.Prune(3) 后 ref 数 = %d, want 3", refs)
	}
	// 包级入口。
	if err := undo.Prune(context.Background(), dir, 1); err != nil {
		t.Fatalf("undo.Prune: %v", err)
	}
	if refs := refCount(t, dir); refs != 1 {
		t.Fatalf("undo.Prune(1) 后 ref 数 = %d, want 1", refs)
	}
}
