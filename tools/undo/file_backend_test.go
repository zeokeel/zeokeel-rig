package undo_test

// 纯文件系统快照后端（非 git 工作区）的契约测试。
//
// 与 git 后端共用 *undo.Tracker 的对外方法：Begin/Turns/Changes/Revert 语义一致，
// 区别只在"快照存在哪"。因此这里的用例只验证行为，不碰内部结构。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/tools/undo"
)

// newFileTracker 建一个根目录不在 git 里的工作区 + 快照目录。
func newFileTracker(t *testing.T, opts undo.FileOptions) (*undo.Tracker, string) {
	t.Helper()
	root := t.TempDir()
	if opts.Dir == "" {
		opts.Dir = filepath.Join(t.TempDir(), "undo")
	}
	tracker, err := undo.NewFileTracker(root, opts)
	if err != nil {
		t.Fatalf("NewFileTracker: %v", err)
	}
	return tracker, root
}

// TestFileTrackerSnapshotsAndRestoresEdits 证明回合里被改的文件在 undo 后回到回合开始的内容。
func TestFileTrackerSnapshotsAndRestoresEdits(t *testing.T) {
	tracker, root := newFileTracker(t, undo.FileOptions{})
	writeTree(t, root, map[string]string{"src/a.txt": "v1\n", "keep.txt": "keep\n"})
	turn, err := tracker.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if turn != 1 {
		t.Fatalf("首个回合号 = %d, want 1", turn)
	}
	writeTree(t, root, map[string]string{"src/a.txt": "v2\n"})

	changes, err := tracker.Changes(0, "")
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes) != 1 || changes[0].Path != "src/a.txt" || changes[0].Op != "modify" {
		t.Fatalf("Changes = %+v, want 只有 src/a.txt 的 modify", changes)
	}

	if _, err := tracker.Revert(0); err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if got := read(t, root, "src/a.txt"); got != "v1\n" {
		t.Fatalf("src/a.txt = %q, want v1", got)
	}
	if got := read(t, root, "keep.txt"); got != "keep\n" {
		t.Fatalf("keep.txt = %q, want 未被碰过", got)
	}
	// 再撤一次：已经与快照一致。
	again, err := tracker.Changes(0, "")
	if err != nil {
		t.Fatalf("Changes again: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("回退后仍有变更: %+v", again)
	}
}

// TestFileTrackerRemovesFilesAddedInTurn 证明回合里新增的文件在 undo 后被删除。
func TestFileTrackerRemovesFilesAddedInTurn(t *testing.T) {
	tracker, root := newFileTracker(t, undo.FileOptions{})
	writeTree(t, root, map[string]string{"a.txt": "a\n"})
	if _, err := tracker.Begin(); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	writeTree(t, root, map[string]string{"docs/new.md": "hello\nworld\n"})

	changes, err := tracker.Changes(0, "")
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes) != 1 || changes[0].Path != "docs/new.md" || changes[0].Op != "add" {
		t.Fatalf("Changes = %+v, want docs/new.md 的 add", changes)
	}
	if _, err := tracker.Revert(0); err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "docs", "new.md")); !os.IsNotExist(err) {
		t.Fatalf("回合内新增的文件应被删除: err=%v", err)
	}
	if got := read(t, root, "a.txt"); got != "a\n" {
		t.Fatalf("a.txt = %q", got)
	}
}

// TestFileTrackerRestoresDeletedFiles 证明回合里被删的文件在 undo 后被恢复。
func TestFileTrackerRestoresDeletedFiles(t *testing.T) {
	tracker, root := newFileTracker(t, undo.FileOptions{})
	writeTree(t, root, map[string]string{"gone.txt": "precious\n"})
	if _, err := tracker.Begin(); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := os.Remove(filepath.Join(root, "gone.txt")); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	changes, err := tracker.Changes(0, "")
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes) != 1 || changes[0].Path != "gone.txt" || changes[0].Op != "delete" {
		t.Fatalf("Changes = %+v, want gone.txt 的 delete", changes)
	}
	if _, err := tracker.Revert(0); err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if got := read(t, root, "gone.txt"); got != "precious\n" {
		t.Fatalf("gone.txt = %q, want 恢复原文", got)
	}
}

// TestFileTrackerSkippedLargeFileIsReported 证明超过 MaxBytes 的文件只记"未快照"，
// 回退时不改动它，并在 undo 结果里列出来。
func TestFileTrackerSkippedLargeFileIsReported(t *testing.T) {
	const limit = 64
	tracker, root := newFileTracker(t, undo.FileOptions{MaxBytes: limit})
	big := strings.Repeat("x", limit*2)
	writeTree(t, root, map[string]string{"data/big.bin": big, "small.txt": "one\n"})
	if _, err := tracker.Begin(); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	changed := big + "more"
	writeTree(t, root, map[string]string{"data/big.bin": changed, "small.txt": "two\n"})

	out := invokeTool(t, undo.New(tracker), `{}`)
	if !strings.Contains(out, "data/big.bin") {
		t.Fatalf("结果没有列出未快照的大文件: %q", out)
	}
	if !strings.Contains(out, "not-backed-up") {
		t.Fatalf("结果缺少未快照的原因: %q", out)
	}
	if !strings.Contains(out, "warning") {
		t.Fatalf("结果缺少 warning 段: %q", out)
	}
	if got := read(t, root, "data/big.bin"); got != changed {
		t.Fatalf("大文件被改动了: %q", got)
	}
	if got := read(t, root, "small.txt"); got != "one\n" {
		t.Fatalf("small.txt = %q, want one", got)
	}
}

// TestFileTrackerResumesTurnCounter 证明重启（关掉再开）后回合号继续，而不是回到 1。
func TestFileTrackerResumesTurnCounter(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(t.TempDir(), "undo")
	writeTree(t, root, map[string]string{"a.txt": "a\n"})

	first, err := undo.NewFileTracker(root, undo.FileOptions{Dir: dir})
	if err != nil {
		t.Fatalf("NewFileTracker: %v", err)
	}
	for want := 1; want <= 2; want++ {
		got, err := first.Begin()
		if err != nil {
			t.Fatalf("Begin #%d: %v", want, err)
		}
		if got != want {
			t.Fatalf("Begin 返回 %d, want %d", got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json")); err != nil {
		t.Fatalf("回合号没有持久化到 Dir/state.json: %v", err)
	}

	second, err := undo.NewFileTracker(root, undo.FileOptions{Dir: dir})
	if err != nil {
		t.Fatalf("重新打开: %v", err)
	}
	if turns := second.Turns(); len(turns) != 2 || turns[0] != 1 || turns[1] != 2 {
		t.Fatalf("重启后 Turns() = %v, want [1 2]", turns)
	}
	got, err := second.Begin()
	if err != nil {
		t.Fatalf("重启后 Begin: %v", err)
	}
	if got != 3 {
		t.Fatalf("重启后回合号 = %d, want 3（不能回到 1）", got)
	}
}

// TestFileTrackerAcceptsOnlyPathInsideRoot 证明过滤路径的越界检查与 git 后端一致。
func TestFileTrackerAcceptsOnlyPathInsideRoot(t *testing.T) {
	tracker, root := newFileTracker(t, undo.FileOptions{})
	writeTree(t, root, map[string]string{"a.txt": "a\n"})
	if _, err := tracker.Begin(); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := tracker.RevertPath(0, "../escape.txt"); err == nil {
		t.Fatal("越界路径应当报错")
	}
}
