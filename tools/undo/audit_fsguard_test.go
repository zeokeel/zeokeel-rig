package undo_test

// 符号链接 / 硬链接语义（经 internal/fsguard 的 traversal-safe 句柄）。
//
// 语义矩阵（快照时 × 现在 → 回退怎么做）：
//
//	file    × file(内容/权限变)  → 原子写回文件
//	file    × symlink            → 删除链接，原子写回**普通文件**（绝不写穿到链接目标）
//	file    × 缺失               → 原子写回文件
//	symlink × symlink(目标变)    → 删除链接，按快照里的目标重建
//	symlink × file               → 删除文件，按快照里的目标重建链接
//	symlink × 缺失               → 按快照里的目标重建链接
//	缺失    × file / symlink     → 删除该路径（删链接本身，不删目标）
//
// 硬链接：同一 inode 的多个路径在快照里被记录为"同组"；回退某一条路径会把该路径
// 换成新 inode（rename 语义），因此**未被回退的同组路径**会保留回合里的内容 ——
// 必须以 Change.HardlinkOf 明确报告，不能静默。

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/tools/undo"
)

// symlinkOrSkip 创建一个符号链接，平台不支持时跳过。
func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("无法创建符号链接: %v", err)
	}
}

// linkTargetOf 返回链接的原始目标文本。
func linkTargetOf(t *testing.T, path string) string {
	t.Helper()
	target, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("Readlink(%q): %v", path, err)
	}
	return target
}

// TestSymlinksAreTrackedInSnapshot 证明符号链接被跟踪（记录目标）但不读目标内容。
func TestSymlinksAreTrackedInSnapshot(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"real.txt": "hello\n"})
	symlinkOrSkip(t, "real.txt", filepath.Join(root, "link.txt"))

	snapshot := take(t, root, undo.Options{})
	if got := snapshot.Files(); got != 2 {
		t.Fatalf("Files() = %d, want 2 (real file + symlink)", got)
	}
	if got := snapshot.Bytes(); got != 6 {
		t.Fatalf("Bytes() = %d, want 6 (only the regular file's content)", got)
	}
	changes, err := snapshot.Changes()
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("changes = %+v, want none when nothing changed", changes)
	}
}

// TestSymlinkToOutsideRootIsNotRead 证明指向根外的链接不会被读出内容（只记录目标文本）。
func TestSymlinkToOutsideRootIsNotRead(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	// 根外目标是一个很大的二进制文件：如果 Take 跟随了链接，Bytes() 会非零或报错。
	big := make([]byte, 1<<20)
	if err := os.WriteFile(filepath.Join(outside, "big.bin"), big, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	symlinkOrSkip(t, filepath.Join(outside, "big.bin"), filepath.Join(root, "link.bin"))

	snapshot := take(t, root, undo.Options{})
	if got := snapshot.Bytes(); got != 0 {
		t.Fatalf("Bytes() = %d, want 0: 指向根外的链接目标不能进入备份", got)
	}
	if got := snapshot.Files(); got != 1 {
		t.Fatalf("Files() = %d, want 1 (the link itself)", got)
	}
	changes, err := snapshot.Changes()
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("changes = %+v, want none", changes)
	}
}

// TestSymlinkTargetChangeIsDetectedAndReverted 证明改链接目标被识别为变更并回退。
func TestSymlinkTargetChangeIsDetectedAndReverted(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.txt": "a\n", "b.txt": "b\n"})
	link := filepath.Join(root, "current")
	symlinkOrSkip(t, "a.txt", link)

	snapshot := take(t, root, undo.Options{})
	// 回合里把链接改指向 b.txt。
	if err := os.Remove(link); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	symlinkOrSkip(t, "b.txt", link)

	changes, err := snapshot.Changes()
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	change := changesByPath(changes)["current"]
	if change.Op != "modify" || change.TypeChange != "symlink->symlink" {
		t.Fatalf("change = %+v, want modify with type symlink->symlink", change)
	}
	revert(t, snapshot)
	if got := linkTargetOf(t, link); got != "a.txt" {
		t.Fatalf("link target = %q, want a.txt", got)
	}
}

// TestSymlinkReplacedByFileIsDetectedAndReverted 证明"链接被换成普通文件"被识别并还原链接。
func TestSymlinkReplacedByFileIsDetectedAndReverted(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.txt": "a\n"})
	link := filepath.Join(root, "current")
	symlinkOrSkip(t, "a.txt", link)

	snapshot := take(t, root, undo.Options{})
	if err := os.Remove(link); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	writeTree(t, root, map[string]string{"current": "now a regular file\n"})

	changes, err := snapshot.Changes()
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	change := changesByPath(changes)["current"]
	if change.Op != "modify" || change.TypeChange != "symlink->file" {
		t.Fatalf("change = %+v, want modify with type symlink->file", change)
	}
	revert(t, snapshot)
	if got := linkTargetOf(t, link); got != "a.txt" {
		t.Fatalf("link target = %q, want a.txt (文件必须被换回链接)", got)
	}
	if got := read(t, filepath.Join(root, "a.txt")); got != "a\n" {
		t.Fatalf("a.txt = %q, want untouched", got)
	}
}

// TestFileReplacedBySymlinkRestoresFileAndLeavesTargetAlone 证明"文件被换成链接"时回退
// 还原普通文件，且**不写穿**到链接目标（哪怕目标在根外）。
func TestFileReplacedBySymlinkRestoresFileAndLeavesTargetAlone(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim.txt")
	if err := os.WriteFile(victim, []byte("outside-untouched\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	writeTree(t, root, map[string]string{"a.txt": "snapshot-content\n"})

	snapshot := take(t, root, undo.Options{})
	if err := os.Remove(filepath.Join(root, "a.txt")); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	symlinkOrSkip(t, victim, filepath.Join(root, "a.txt"))

	changes, err := snapshot.Changes()
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	change := changesByPath(changes)["a.txt"]
	if change.Op != "modify" || change.TypeChange != "file->symlink" {
		t.Fatalf("change = %+v, want modify with type file->symlink", change)
	}
	revert(t, snapshot)

	if got := read(t, victim); got != "outside-untouched\n" {
		t.Fatalf("根外文件被写穿改坏: %q", got)
	}
	info, err := os.Lstat(filepath.Join(root, "a.txt"))
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("a.txt mode = %v, want a regular file", info.Mode())
	}
	if got := read(t, filepath.Join(root, "a.txt")); got != "snapshot-content\n" {
		t.Fatalf("a.txt = %q, want the snapshot content", got)
	}
}

// TestDeletedSymlinkIsRestored 证明回合里删掉的链接会被重建（目标是原始文本）。
func TestDeletedSymlinkIsRestored(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.txt": "a\n"})
	link := filepath.Join(root, "current")
	symlinkOrSkip(t, "a.txt", link)

	snapshot := take(t, root, undo.Options{})
	if err := os.Remove(link); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	changes, err := snapshot.Changes()
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	change := changesByPath(changes)["current"]
	if change.Op != "delete" {
		t.Fatalf("change = %+v, want delete", change)
	}
	revert(t, snapshot)
	if got := linkTargetOf(t, link); got != "a.txt" {
		t.Fatalf("link target = %q, want a.txt", got)
	}
}

// TestAddedSymlinkIsRemovedOnRevert 证明回合里新建的链接被删除，且不触碰其目标。
func TestAddedSymlinkIsRemovedOnRevert(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.txt": "a\n"})
	snapshot := take(t, root, undo.Options{})
	link := filepath.Join(root, "new-link")
	symlinkOrSkip(t, "a.txt", link)

	changes, err := snapshot.Changes()
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	change := changesByPath(changes)["new-link"]
	if change.Op != "add" {
		t.Fatalf("change = %+v, want add", change)
	}
	revert(t, snapshot)
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("new-link still exists (err=%v)", err)
	}
	if got := read(t, filepath.Join(root, "a.txt")); got != "a\n" {
		t.Fatalf("a.txt = %q, want its target untouched", got)
	}
}

// TestDirectorySymlinkIsTrackedNotFollowed 证明指向目录的链接被当作链接跟踪（不展开）。
func TestDirectorySymlinkIsTrackedNotFollowed(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"dir/a.txt": "a\n"})
	link := filepath.Join(root, "dirlink")
	symlinkOrSkip(t, "dir", link)

	snapshot := take(t, root, undo.Options{})
	changes, err := snapshot.Changes()
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("changes = %+v, want none", changes)
	}
	// 把链接换成一个普通文件后必须被识别（链接与目录内容都没被展开）。
	if err := os.Remove(link); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	writeTree(t, root, map[string]string{"dirlink": "file now\n"})
	changes, err = snapshot.Changes()
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	change := changesByPath(changes)["dirlink"]
	if change.TypeChange != "symlink->file" {
		t.Fatalf("change = %+v, want symlink->file", change)
	}
	revert(t, snapshot)
	if got := linkTargetOf(t, link); got != "dir" {
		t.Fatalf("dirlink target = %q, want dir", got)
	}
}

// TestHardlinkSiblingReportedWhenOnlyOnePathReverted 证明只回退同 inode 的一条路径时，
// 另一条路径会被明确报告（硬链接已被 rename 断开）。
func TestHardlinkSiblingReportedWhenOnlyOnePathReverted(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.txt": "original\n"})
	if err := os.Link(filepath.Join(root, "a.txt"), filepath.Join(root, "b.txt")); err != nil {
		t.Skipf("无法创建硬链接: %v", err)
	}

	snapshot := take(t, root, undo.Options{})
	// 回合里通过 a.txt 改写（同一 inode，b.txt 一起变）。
	writeTree(t, root, map[string]string{"a.txt": "modified\n"})

	changes, err := snapshot.RevertPath("a.txt")
	if err != nil {
		t.Fatalf("RevertPath: %v", err)
	}
	byPath := changesByPath(changes)
	if applied, ok := byPath["a.txt"]; !ok || !applied.Reverted {
		t.Fatalf("a.txt change = %+v, want reverted", applied)
	}
	sibling, ok := byPath["b.txt"]
	if !ok {
		t.Fatalf("changes = %+v, want an entry informing about b.txt (same inode)", changes)
	}
	if sibling.HardlinkOf != "a.txt" {
		t.Fatalf("b.txt HardlinkOf = %q, want a.txt", sibling.HardlinkOf)
	}
	if got := read(t, filepath.Join(root, "a.txt")); got != "original\n" {
		t.Fatalf("a.txt = %q, want reverted", got)
	}
	if got := read(t, filepath.Join(root, "b.txt")); got != "modified\n" {
		t.Fatalf("b.txt = %q, want left as-is (hard link broken by the revert)", got)
	}
	// 渲染必须出现警告，不能静默（文案由 render 的单元测试与工具结果覆盖）。
	if !strings.Contains(sibling.Reason+sibling.HardlinkOf, "a.txt") {
		t.Fatalf("sibling change = %+v, want it to name the rewritten path", sibling)
	}
}

// TestHardlinkGroupFullyRevertedRestoresBothContents 证明全量回退时同 inode 的两条路径
// 内容都恢复；文档化的代价是它们变成独立 inode（硬链接关系不再重建）。
func TestHardlinkGroupFullyRevertedRestoresBothContents(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.txt": "original\n"})
	if err := os.Link(filepath.Join(root, "a.txt"), filepath.Join(root, "b.txt")); err != nil {
		t.Skipf("无法创建硬链接: %v", err)
	}
	snapshot := take(t, root, undo.Options{})
	writeTree(t, root, map[string]string{"a.txt": "modified\n"})

	changes := revert(t, snapshot)
	for _, path := range []string{"a.txt", "b.txt"} {
		if change, ok := changesByPath(changes)[path]; !ok || !change.Reverted {
			t.Fatalf("%s change = %+v, want reverted", path, changes)
		}
		if got := read(t, filepath.Join(root, path)); got != "original\n" {
			t.Fatalf("%s = %q, want original", path, got)
		}
	}
	first, err := os.Stat(filepath.Join(root, "a.txt"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	second, err := os.Stat(filepath.Join(root, "b.txt"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if os.SameFile(first, second) {
		t.Fatal("回退之后两条路径仍是同一 inode；rename 语义下这是不可能的，说明实现不符合预期")
	}
}

// TestRootReplacedBySymlinkRejectsRevert 证明工作区根被换成符号链接后拒绝回退。
func TestRootReplacedBySymlinkRejectsRevert(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "ws")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeTree(t, root, map[string]string{"a.txt": "one\n"})
	snapshot := take(t, root, undo.Options{})
	writeTree(t, root, map[string]string{"a.txt": "two\n"})

	// 把根目录搬走，并在原位置放一个指向别处的符号链接。
	other := filepath.Join(parent, "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Rename(root, filepath.Join(parent, "moved")); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	symlinkOrSkip(t, other, root)

	_, err := snapshot.Revert()
	if err == nil {
		t.Fatal("Revert succeeded after the root was replaced by a symlink")
	}
	if !errors.Is(err, undo.ErrRootReplaced) {
		t.Fatalf("error = %v, want ErrRootReplaced", err)
	}
}

// TestRootReplacedByDifferentDirectoryRejectsRevert 证明根被换成另一个真实目录同样被拒。
func TestRootReplacedByDifferentDirectoryRejectsRevert(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "ws")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeTree(t, root, map[string]string{"a.txt": "one\n"})
	snapshot := take(t, root, undo.Options{})
	writeTree(t, root, map[string]string{"a.txt": "two\n"})

	if err := os.Rename(root, filepath.Join(parent, "moved")); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("impostor\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := snapshot.Revert()
	if !errors.Is(err, undo.ErrRootReplaced) {
		t.Fatalf("error = %v, want ErrRootReplaced", err)
	}
	if got := read(t, filepath.Join(root, "a.txt")); got != "impostor\n" {
		t.Fatalf("换掉的根被改写了: %q", got)
	}
}

// TestEscapingParentIsRejectedForPathRevert 证明"父目录被换成指向根外的链接"时，
// 针对该路径的定向回退直接拒绝（fsguard 在系统调用层识别逃逸），根外文件不被改写。
//
// 这是迁移前 TOCTOU 的回归：旧实现用 filepath + os 调用，"校验通过 → 链接被换 → 写入"
// 之间可以把内容写到根外。
func TestEscapingParentIsRejectedForPathRevert(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeTree(t, root, map[string]string{"sub/a.txt": "snapshot\n"})
	if err := os.WriteFile(filepath.Join(outside, "a.txt"), []byte("outside\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	snapshot := take(t, root, undo.Options{})
	writeTree(t, root, map[string]string{"sub/a.txt": "modified\n"})

	// 回合里把 sub 换成指向根外的符号链接。
	if err := os.Rename(filepath.Join(root, "sub"), filepath.Join(root, "sub-old")); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	symlinkOrSkip(t, outside, filepath.Join(root, "sub"))

	_, err := snapshot.RevertPath("sub/a.txt")
	if err == nil {
		t.Fatal("RevertPath succeeded through a parent directory swapped for an escaping symlink")
	}
	if got := read(t, filepath.Join(outside, "a.txt")); got != "outside\n" {
		t.Fatalf("根外文件被改写: %q", got)
	}
}

// TestFullRevertNeverWritesOutsideRoot 证明全量回退在同样的"父目录换成根外链接"
// 场景下也绝不写到根外：链接本身先被当作新增项删除，随后按快照重建普通目录与文件。
//
// 两种结果（报错 / 安全重建）都可以接受，**唯一不可接受的是写到根外**，因此这里
// 断言安全不变量 + 可解释的最终状态。
func TestFullRevertNeverWritesOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeTree(t, root, map[string]string{"sub/a.txt": "snapshot\n"})
	if err := os.WriteFile(filepath.Join(outside, "a.txt"), []byte("outside\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	snapshot := take(t, root, undo.Options{})
	writeTree(t, root, map[string]string{"sub/a.txt": "modified\n"})
	if err := os.Rename(filepath.Join(root, "sub"), filepath.Join(root, "sub-old")); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	symlinkOrSkip(t, outside, filepath.Join(root, "sub"))

	changes, err := snapshot.Revert()
	if err != nil {
		// 拒绝也是一种安全结果，但必须没有写到根外。
		if got := read(t, filepath.Join(outside, "a.txt")); got != "outside\n" {
			t.Fatalf("回退失败但根外文件被改写: %q", got)
		}
		return
	}
	if got := read(t, filepath.Join(outside, "a.txt")); got != "outside\n" {
		t.Fatalf("根外文件被改写: %q", got)
	}
	// 安全重建路径：逃逸链接被删除，快照里的 sub/a.txt 被恢复。
	info, statErr := os.Lstat(filepath.Join(root, "sub", "a.txt"))
	if statErr != nil {
		t.Fatalf("sub/a.txt 未恢复（changes=%+v）: %v", changes, statErr)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("sub/a.txt mode = %v, want a regular file", info.Mode())
	}
	if got := read(t, filepath.Join(root, "sub", "a.txt")); got != "snapshot\n" {
		t.Fatalf("sub/a.txt = %q, want the snapshot content", got)
	}
}

// TestSymlinkRestoreFailureRollsBack 证明符号链接恢复失败时，之前的动作被回滚。
func TestSymlinkRestoreFailureRollsBack(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.txt": "a-old\n", "b.txt": "b\n"})
	link := filepath.Join(root, "zlink")
	symlinkOrSkip(t, "b.txt", link)
	snapshot := take(t, root, undo.Options{})

	// 1) 改 a.txt（会被成功写回，然后必须回滚）。
	writeTree(t, root, map[string]string{"a.txt": "a-new\n"})
	// 2) 把 zlink 删掉、在原位造一个**非空目录**，使"重建链接"必然失败。
	if err := os.Remove(link); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	writeTree(t, root, map[string]string{"zlink/child.txt": "child\n"})

	changes, err := snapshot.Revert()
	if err == nil {
		t.Fatalf("Revert succeeded despite an impossible symlink restore; changes = %+v", changes)
	}
	if got := read(t, filepath.Join(root, "a.txt")); got != "a-new\n" {
		t.Fatalf("a.txt = %q, want the pre-revert content restored by rollback", got)
	}
	if info, statErr := os.Stat(link); statErr != nil || !info.IsDir() {
		t.Fatalf("zlink = %v (err=%v), want the directory left in place", info, statErr)
	}
}

// TestRevertIsIdempotentWithSymlinks 证明重复回退不再产生变更（链接语义稳定）。
func TestRevertIsIdempotentWithSymlinks(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.txt": "a\n"})
	link := filepath.Join(root, "link")
	symlinkOrSkip(t, "a.txt", link)
	snapshot := take(t, root, undo.Options{})

	if err := os.Remove(link); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	writeTree(t, root, map[string]string{"link": "file\n"})
	if changes := revert(t, snapshot); len(changes) != 1 {
		t.Fatalf("first revert = %+v, want one change", changes)
	}
	if changes := revert(t, snapshot); len(changes) != 0 {
		t.Fatalf("second revert = %+v, want no changes", changes)
	}
	if got := linkTargetOf(t, link); got != "a.txt" {
		t.Fatalf("link target = %q, want a.txt", got)
	}
}
