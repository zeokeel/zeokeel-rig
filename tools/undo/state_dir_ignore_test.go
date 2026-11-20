package undo_test

// 忽略表必须按"实际状态目录"推导，两个后端同一个来源：
// git 后端的回合号在 Options.Dir 下，文件后端的状态与快照在 FileOptions.Dir 下。

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zeokeel/zeokeel-rig/tools/undo"
)

// TestGitBackendIgnoresStateFileWhenDirIsRoot 守边角：Dir 就是工作区根本身时，
// 第二个回合写进的 `<Root>/state.json` 不能变成第一回合的"回合改动"——否则 undo 会
// 建议用户回退自己的回合号文件，Revert 还会把回合号写回旧值。
func TestGitBackendIgnoresStateFileWhenDirIsRoot(t *testing.T) {
	root := newRepo(t)
	writeTree(t, root, map[string]string{"a.txt": "v1\n"})
	commitAll(t, root)

	tracker := undo.NewTracker(root, undo.Options{Dir: root})
	if _, err := tracker.Begin(); err != nil {
		t.Fatalf("Begin #1: %v", err)
	}
	writeTree(t, root, map[string]string{"a.txt": "v2\n"})
	// 第二次 Begin 会把回合号写进 <Root>/state.json。
	if _, err := tracker.Begin(); err != nil {
		t.Fatalf("Begin #2: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "state.json")); err != nil {
		t.Fatalf("回合号没有落在 <Root>/state.json: %v", err)
	}

	changes, err := tracker.Changes(1, "")
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	for _, change := range changes {
		if change.Path == "state.json" {
			t.Fatalf("Dir == Root 时 state.json 被当成了回合改动: %+v", changes)
		}
	}
	if len(changes) != 1 || changes[0].Path != "a.txt" {
		t.Fatalf("Changes(1) = %+v, want 只有 a.txt", changes)
	}
}

// TestFileBackendStillSkipsItsOwnStateDir 守另一半：文件后端的快照目录在 Root 内时
// 不能被自己的快照收进去（忽略表与遍历共用同一个"实际状态目录"来源）。
func TestFileBackendStillSkipsItsOwnStateDir(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.txt": "v1\n"})
	dir := filepath.Join(root, "nested", "snapshots")

	tracker, err := undo.NewFileTracker(root, undo.FileOptions{Dir: dir})
	if err != nil {
		t.Fatalf("NewFileTracker: %v", err)
	}
	if _, err := tracker.Begin(); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	changes, err := tracker.Changes(1, "")
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	for _, change := range changes {
		if change.Path == "nested" || change.Path == "nested/snapshots" ||
			len(change.Path) > len("nested/snapshots/") && change.Path[:len("nested/snapshots/")] == "nested/snapshots/" {
			t.Fatalf("快照目录被当成了回合改动: %+v", change)
		}
	}
}
