package undo_test

// undo 包的快照/回退契约。
//
// 这些用例先于实现写成（TDD），钉住的是"回退到底做了什么"：新增文件被删掉、改动与
// 删除被逐字节写回、未改文件不碰（mtime 不变）、不可信文件只报告不回退、以及失败时
// 的全有或全无。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/tools/undo"
)

// --- 测试辅助 -------------------------------------------------------------

// writeTree 按相对路径写入一组文件。
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile(%q): %v", path, err)
		}
	}
}

// take 取一次快照并要求成功。
func take(t *testing.T, root string, opts undo.Options) *undo.Snapshot {
	t.Helper()
	snapshot, err := undo.Take(root, opts)
	if err != nil {
		t.Fatalf("Take(%q): %v", root, err)
	}
	return snapshot
}

// revert 执行全量回退并要求成功。
func revert(t *testing.T, snapshot *undo.Snapshot) []undo.Change {
	t.Helper()
	changes, err := snapshot.Revert()
	if err != nil {
		t.Fatalf("Revert: %v", err)
	}
	return changes
}

// read 读取文件内容。
func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	return string(data)
}

// changesByPath 把变更按路径索引。
func changesByPath(changes []undo.Change) map[string]undo.Change {
	byPath := make(map[string]undo.Change, len(changes))
	for _, change := range changes {
		byPath[change.Path] = change
	}
	return byPath
}

// changePaths 返回变更路径（保持顺序）。
func changePaths(changes []undo.Change) []string {
	paths := make([]string, 0, len(changes))
	for _, change := range changes {
		paths = append(paths, change.Path)
	}
	return paths
}

// --- 快照基础 -------------------------------------------------------------

// TestTakeCountsFilesAndBytes 证明快照统计跟踪到的文件数与备份字节数。
func TestTakeCountsFilesAndBytes(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"a.txt":     "hello\n", // 6
		"b.txt":     "world",   // 5
		"sub/c.txt": "abc\n",   // 4
	})
	snapshot := take(t, root, undo.Options{})
	if got := snapshot.Files(); got != 3 {
		t.Fatalf("Files() = %d, want 3", got)
	}
	if got := snapshot.Bytes(); got != 15 {
		t.Fatalf("Bytes() = %d, want 15", got)
	}
}

// TestChangesEmptyWhenNothingChanged 证明无变化时返回空切片且不报错。
func TestChangesEmptyWhenNothingChanged(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.txt": "a\n"})
	snapshot := take(t, root, undo.Options{})
	changes, err := snapshot.Changes()
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("changes = %+v, want none", changes)
	}
	if reverted := revert(t, snapshot); len(reverted) != 0 {
		t.Fatalf("revert = %+v, want none", reverted)
	}
}

// TestChangesDetectsSameSizeEdit 证明"长度相同、内容不同"的改动也能被发现（哈希比较，
// 而不是只比大小）。
func TestChangesDetectsSameSizeEdit(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.txt": "aaaa\n"})
	snapshot := take(t, root, undo.Options{})
	writeTree(t, root, map[string]string{"a.txt": "bbbb\n"})

	changes, err := snapshot.Changes()
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes) != 1 || changes[0].Op != "modify" {
		t.Fatalf("changes = %+v, want one modify", changes)
	}
	revert(t, snapshot)
	if got := read(t, filepath.Join(root, "a.txt")); got != "aaaa\n" {
		t.Fatalf("content = %q, want %q", got, "aaaa\n")
	}
}

// --- 三类回退 -------------------------------------------------------------

// TestRevertAddedFile 证明回合中新建的文件被删除。
func TestRevertAddedFile(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"keep.txt": "k\n"})
	snapshot := take(t, root, undo.Options{})
	writeTree(t, root, map[string]string{"new.txt": "one\ntwo\n"})

	changes, err := snapshot.Changes()
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("changes = %+v, want one add", changes)
	}
	add := changes[0]
	if add.Path != "new.txt" || add.Op != "add" || add.Added != 2 || add.Removed != 0 {
		t.Fatalf("change = %+v, want add new.txt (+2/-0)", add)
	}

	reverted := revert(t, snapshot)
	if len(reverted) != 1 || !reverted[0].Reverted {
		t.Fatalf("reverted = %+v, want one reverted change", reverted)
	}
	if _, statErr := os.Stat(filepath.Join(root, "new.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("new.txt still exists after revert (err=%v)", statErr)
	}
	if got := read(t, filepath.Join(root, "keep.txt")); got != "k\n" {
		t.Fatalf("keep.txt = %q, want untouched", got)
	}
}

// TestRevertModifiedFileIsByteExact 证明改动的文件被逐字节写回（含 CRLF 与尾随空白）。
func TestRevertModifiedFileIsByteExact(t *testing.T) {
	root := t.TempDir()
	original := "line one\r\nline two   \r\n"
	writeTree(t, root, map[string]string{"a.txt": original})
	snapshot := take(t, root, undo.Options{})
	writeTree(t, root, map[string]string{"a.txt": "rewritten\n"})

	changes := changesByPath(revert(t, snapshot))
	if change, ok := changes["a.txt"]; !ok || !change.Reverted || change.Op != "modify" {
		t.Fatalf("changes = %+v, want a reverted modify for a.txt", changes)
	}
	if got := read(t, filepath.Join(root, "a.txt")); got != original {
		t.Fatalf("content = %q, want the original bytes %q", got, original)
	}
}

// TestRevertDeletedFile 证明回合中删除的文件被写回，且行数统计正确。
func TestRevertDeletedFile(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"gone.txt": "one\ntwo\nthree\n"})
	snapshot := take(t, root, undo.Options{})
	if err := os.Remove(filepath.Join(root, "gone.txt")); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	changes, err := snapshot.Changes()
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes) != 1 || changes[0].Op != "delete" || changes[0].Removed != 3 || changes[0].Added != 0 {
		t.Fatalf("changes = %+v, want delete gone.txt (+0/-3)", changes)
	}
	revert(t, snapshot)
	if got := read(t, filepath.Join(root, "gone.txt")); got != "one\ntwo\nthree\n" {
		t.Fatalf("content = %q, want the original", got)
	}
}

// TestChangesCountsLinesForModify 证明 modify 的 +/- 行数来自真实行级差异。
func TestChangesCountsLinesForModify(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.txt": "one\ntwo\nthree\n"})
	snapshot := take(t, root, undo.Options{})
	writeTree(t, root, map[string]string{"a.txt": "one\ntwo-changed\nthree\nfour\n"})

	changes, err := snapshot.Changes()
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("changes = %+v, want one modify", changes)
	}
	if changes[0].Added != 2 || changes[0].Removed != 1 {
		t.Fatalf("change = %+v, want (+2/-1)", changes[0])
	}
}

// TestRevertMultipleChangesAreSorted 证明多文件变更按路径排序（结果稳定可断言）。
func TestRevertMultipleChangesAreSorted(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"a.txt":      "a\n",
		"b.txt":      "b\n",
		"c/gone.txt": "c\n",
	})
	snapshot := take(t, root, undo.Options{})
	writeTree(t, root, map[string]string{"a.txt": "a2\n", "z.txt": "z\n"})
	if err := os.Remove(filepath.Join(root, "c", "gone.txt")); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	changes := revert(t, snapshot)
	want := []string{"a.txt", "c/gone.txt", "z.txt"}
	if got := changePaths(changes); len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("paths = %v, want %v", got, want)
	}
	for _, change := range changes {
		if !change.Reverted {
			t.Fatalf("change %+v was not reverted", change)
		}
	}
}

// TestUnchangedFilesKeepMtime 证明未改动文件不被触碰（mtime 不变）。
func TestUnchangedFilesKeepMtime(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"untouched.txt": "u\n", "changed.txt": "c\n"})
	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(root, "untouched.txt"), old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	snapshot := take(t, root, undo.Options{})
	writeTree(t, root, map[string]string{"changed.txt": "c2\n"})
	revert(t, snapshot)

	info, err := os.Stat(filepath.Join(root, "untouched.txt"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !info.ModTime().Equal(old) {
		t.Fatalf("mtime = %v, want unchanged %v", info.ModTime(), old)
	}
}

// TestRevertPreservesFileMode 证明回退保留原文件权限。
func TestRevertPreservesFileMode(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"script.sh": "#!/bin/sh\n"})
	path := filepath.Join(root, "script.sh")
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	snapshot := take(t, root, undo.Options{})
	writeTree(t, root, map[string]string{"script.sh": "rewritten\n"})
	revert(t, snapshot)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %o, want 600", got)
	}
}

// TestRevertAddedFileKeepsParentDirectory 证明回退新增文件只删文件，不删目录
// （目录里可能还有别的东西，删目录属于越权）。
func TestRevertAddedFileKeepsParentDirectory(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"sub/keep.txt": "k\n"})
	snapshot := take(t, root, undo.Options{})
	writeTree(t, root, map[string]string{"sub/new.txt": "n\n"})
	revert(t, snapshot)

	if _, err := os.Stat(filepath.Join(root, "sub", "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("new.txt still exists (err=%v)", err)
	}
	if info, err := os.Stat(filepath.Join(root, "sub")); err != nil || !info.IsDir() {
		t.Fatalf("sub directory disappeared: info=%v err=%v", info, err)
	}
}

// TestRevertTwiceIsIdempotent 证明重复回退第二次没有变更。
func TestRevertTwiceIsIdempotent(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.txt": "a\n"})
	snapshot := take(t, root, undo.Options{})
	writeTree(t, root, map[string]string{"a.txt": "b\n"})
	revert(t, snapshot)

	second := revert(t, snapshot)
	if len(second) != 0 {
		t.Fatalf("second revert = %+v, want none", second)
	}
	if got := read(t, filepath.Join(root, "a.txt")); got != "a\n" {
		t.Fatalf("content = %q, want a", got)
	}
}

// --- 忽略规则 -------------------------------------------------------------

// TestIgnoreDefaultDirectories 证明默认忽略 .git/node_modules/vendor/dist/build。
func TestIgnoreDefaultDirectories(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"src/a.go":                  "package a\n",
		".git/HEAD":                 "ref: refs/heads/main\n",
		"node_modules/pkg/index.js": "module.exports\n",
		"vendor/lib.go":             "package lib\n",
		"dist/bundle.js":            "bundled\n",
		"build/out.o":               "binary-ish\n",
	})
	snapshot := take(t, root, undo.Options{})
	if got := snapshot.Files(); got != 1 {
		t.Fatalf("Files() = %d, want only src/a.go", got)
	}
	writeTree(t, root, map[string]string{"node_modules/pkg/index.js": "changed\n"})
	changes, err := snapshot.Changes()
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("changes = %+v, want none for ignored paths", changes)
	}
}

// TestIgnoreRulesAppendToDefaults 证明用户规则是追加而不是替换（.git 永远被忽略）。
func TestIgnoreRulesAppendToDefaults(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		".git/HEAD":   "ref: refs/heads/main\n",
		"cache/x.bin": "x\n",
		"src/a.go":    "a\n",
	})
	snapshot := take(t, root, undo.Options{Ignore: []string{"cache"}})
	if got := snapshot.Files(); got != 1 {
		t.Fatalf("Files() = %d, want only src/a.go (custom rule + defaults)", got)
	}
}

// TestSnapshotTracksSymlinks 证明符号链接被跟踪（记录目标文本）但**不读**目标内容。
//
// 语义随 internal/fsguard 迁移而变：旧实现直接跳过链接，导致"链接被换成文件/被删除"
// 完全不可见；现在链接是快照里的第一类条目（目标文本可回退），但内容预算只算普通文件。
func TestSnapshotTracksSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeTree(t, outside, map[string]string{"secret.txt": "s\n"})
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	writeTree(t, root, map[string]string{"real.txt": "r\n"})

	snapshot := take(t, root, undo.Options{})
	if got := snapshot.Files(); got != 2 {
		t.Fatalf("Files() = %d, want real.txt and link.txt", got)
	}
	if got := snapshot.Bytes(); got != 2 {
		t.Fatalf("Bytes() = %d, want 2 (the link's target is never read)", got)
	}
	changes, err := snapshot.Changes()
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("changes = %+v, want none", changes)
	}
}

// --- 不可信内容：不备份、回退时跳过并在 Changes 里说明 ---------------------

// TestBinaryFileIsNotBackedUp 证明含 NUL 的二进制文件不备份、回退时跳过并给出原因。
func TestBinaryFileIsNotBackedUp(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"blob.bin": "head\x00tail"})
	snapshot := take(t, root, undo.Options{})
	if got := snapshot.Bytes(); got != 0 {
		t.Fatalf("Bytes() = %d, want 0 for a binary file", got)
	}
	writeTree(t, root, map[string]string{"blob.bin": "head\x00tail2"})

	changes, err := snapshot.Changes()
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("changes = %+v, want one modify", changes)
	}
	if !strings.HasPrefix(changes[0].Reason, "not-backed-up") {
		t.Fatalf("reason = %q, want a not-backed-up prefix", changes[0].Reason)
	}

	reverted := revert(t, snapshot)
	if len(reverted) != 1 || reverted[0].Reverted || reverted[0].Reason == "" {
		t.Fatalf("reverted = %+v, want the change reported as skipped", reverted)
	}
	if got := read(t, filepath.Join(root, "blob.bin")); got != "head\x00tail2" {
		t.Fatalf("content = %q, want it left as-is", got)
	}
}

// TestLargeFileIsNotBackedUp 证明超过 MaxFileBytes 的文件不备份、只报告。
func TestLargeFileIsNotBackedUp(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"big.txt": "0123456789"})
	snapshot := take(t, root, undo.Options{MaxFileBytes: 4})
	writeTree(t, root, map[string]string{"big.txt": "0123456789X"})

	changes, err := snapshot.Changes()
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes) != 1 || !strings.HasPrefix(changes[0].Reason, "not-backed-up") {
		t.Fatalf("changes = %+v, want one not-backed-up modify", changes)
	}
	revert(t, snapshot)
	if got := read(t, filepath.Join(root, "big.txt")); got != "0123456789X" {
		t.Fatalf("content = %q, want the large file left as-is", got)
	}
}

// TestTotalBudgetExceededSkipsLaterFiles 证明总预算用尽后，后续文件不再备份。
func TestTotalBudgetExceededSkipsLaterFiles(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.txt": "aaaa\n", "b.txt": "bbbb\n"})
	snapshot := take(t, root, undo.Options{MaxTotalBytes: 6})
	if got := snapshot.Bytes(); got != 5 {
		t.Fatalf("Bytes() = %d, want 5 (only the first file fits)", got)
	}
	writeTree(t, root, map[string]string{"b.txt": "bbbb-changed\n"})
	changes, err := snapshot.Changes()
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes) != 1 || !strings.HasPrefix(changes[0].Reason, "not-backed-up") {
		t.Fatalf("changes = %+v, want the unbudgeted file reported as not-backed-up", changes)
	}
}

// TestMaxFilesExceededFailsLoud 证明超过 MaxFiles 时 Take 返回错误（绝不静默快照一部分：
// 部分快照会把未记录的文件误判为"新增"，回退时删掉它们）。
func TestMaxFilesExceededFailsLoud(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.txt": "a\n", "b.txt": "b\n"})
	if _, err := undo.Take(root, undo.Options{MaxFiles: 1}); err == nil {
		t.Fatal("Take accepted a workspace with more files than MaxFiles")
	}
}

// TestTakeRejectsInvalidRoot 证明 root 不存在或不是目录时报错。
func TestTakeRejectsInvalidRoot(t *testing.T) {
	if _, err := undo.Take(filepath.Join(t.TempDir(), "nope"), undo.Options{}); err == nil {
		t.Fatal("Take accepted a nonexistent root")
	}
	file := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := undo.Take(file, undo.Options{}); err == nil {
		t.Fatal("Take accepted a regular file as root")
	}
}

// --- 原子性 ---------------------------------------------------------------

// TestRevertIsAtomicOnFailure 用"目标位置被一个非空目录占住"制造第二次写入失败：
// 第一个文件的恢复必须被撤销（回到回退前的状态），并返回错误。
func TestRevertIsAtomicOnFailure(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.txt": "a-old\n", "x.txt": "x-old\n"})
	snapshot := take(t, root, undo.Options{})

	// 第一处改动（会被成功写回，然后必须回滚）。
	writeTree(t, root, map[string]string{"a.txt": "a-new\n"})
	// 第二处改动：把 x.txt 换成目录并在里面放文件，使"把 x.txt 写回文件"必然失败。
	if err := os.Remove(filepath.Join(root, "x.txt")); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	writeTree(t, root, map[string]string{"x.txt/child.txt": "child\n"})

	changes, err := snapshot.Revert()
	if err == nil {
		t.Fatalf("Revert succeeded despite an impossible write; changes = %+v", changes)
	}
	if got := read(t, filepath.Join(root, "a.txt")); got != "a-new\n" {
		t.Fatalf("a.txt = %q, want the pre-revert content restored by rollback", got)
	}
	if info, statErr := os.Stat(filepath.Join(root, "x.txt")); statErr != nil || !info.IsDir() {
		t.Fatalf("x.txt = %v (err=%v), want the directory left in place", info, statErr)
	}
}

// TestRevertPathFiltersSingleFile 证明可以只回退一个路径。
func TestRevertPathFiltersSingleFile(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.txt": "a\n", "b.txt": "b\n"})
	snapshot := take(t, root, undo.Options{})
	writeTree(t, root, map[string]string{"a.txt": "a2\n", "b.txt": "b2\n"})

	changes, err := snapshot.RevertPath("a.txt")
	if err != nil {
		t.Fatalf("RevertPath: %v", err)
	}
	if len(changes) != 1 || changes[0].Path != "a.txt" || !changes[0].Reverted {
		t.Fatalf("changes = %+v, want only a.txt reverted", changes)
	}
	if got := read(t, filepath.Join(root, "a.txt")); got != "a\n" {
		t.Fatalf("a.txt = %q, want reverted", got)
	}
	if got := read(t, filepath.Join(root, "b.txt")); got != "b2\n" {
		t.Fatalf("b.txt = %q, want untouched", got)
	}
}

// TestSnapshotRejectsEscapingPaths 证明快照级路径过滤同样拒绝越界（绝对路径与 ".."）。
func TestSnapshotRejectsEscapingPaths(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.txt": "a\n"})
	snapshot := take(t, root, undo.Options{})
	for _, path := range []string{"/etc/passwd", "../outside.txt", `C:\Windows\system32`} {
		if _, err := snapshot.RevertPath(path); err == nil {
			t.Fatalf("RevertPath(%q) accepted an escaping path", path)
		}
	}
}

// TestRevertPathUnknownIsError 证明过滤一个既不在快照也不在当前树里的路径会报错。
func TestRevertPathUnknownIsError(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.txt": "a\n"})
	snapshot := take(t, root, undo.Options{})
	if _, err := snapshot.RevertPath("missing.txt"); err == nil {
		t.Fatal("RevertPath accepted a path that is neither tracked nor present")
	}
}

// TestRevertDeletedFileInDeletedDirectory 证明整个目录被删掉后仍能重建父目录并写回文件。
func TestRevertDeletedFileInDeletedDirectory(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"sub/deep/gone.txt": "gone\n"})
	snapshot := take(t, root, undo.Options{})
	if err := os.RemoveAll(filepath.Join(root, "sub")); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}

	changes, err := snapshot.Changes()
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes) != 1 || changes[0].Path != "sub/deep/gone.txt" || changes[0].Op != "delete" {
		t.Fatalf("changes = %+v, want a single delete", changes)
	}
	revert(t, snapshot)
	if got := read(t, filepath.Join(root, "sub", "deep", "gone.txt")); got != "gone\n" {
		t.Fatalf("content = %q, want the restored file", got)
	}
}

// TestRevertPathDirectoryIsRejected 证明把目录当作 path 传入会报错，而不是静默地
// "撤销成功但没有变化"。
func TestRevertPathDirectoryIsRejected(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"sub/a.txt": "a\n"})
	snapshot := take(t, root, undo.Options{})
	if _, err := snapshot.RevertPath("sub"); err == nil {
		t.Fatal("RevertPath accepted a directory")
	}
}

// TestSnapshotIgnoresItsOwnTempFiles 证明回退过程中可能残留的临时文件不会被当成
// 工作区内容跟踪。
func TestSnapshotIgnoresItsOwnTempFiles(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{".rig-undo-abc123.tmp": "junk\n", "a.txt": "a\n"})
	snapshot := take(t, root, undo.Options{})
	if got := snapshot.Files(); got != 1 {
		t.Fatalf("Files() = %d, want only a.txt", got)
	}
}
