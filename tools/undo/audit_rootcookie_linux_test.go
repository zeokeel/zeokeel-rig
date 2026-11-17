//go:build linux

package undo_test

// 根实例 cookie（xattr）的定向回归。
//
// 背景：只比较 (dev, ino) 挡不住"同 inode 复用"——删除目录后在**同一父目录**重建时，
// ext4 会把刚释放的 inode 分配给新目录，旧快照会误判"还是同一个工作区"，把内容倒进
// 冒名目录。cookie（写在根 inode 上的 user.rig-undo-root）是唯一能识别这种替换的信号。

import (
	"errors"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/zeokeel/zeokeel-rig/tools/undo"
)

const rootCookieAttr = "user.rig-undo-root"

// TestRootCookieIsWrittenAndInvisible 证明 cookie 落到了根的 inode 上，且不产生目录项。
func TestRootCookieIsWrittenAndInvisible(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.txt": "a\n", "b.txt": "b\n"})
	before := readDirNames(t, root)

	snapshot := take(t, root, undo.Options{})
	buffer := make([]byte, 1024)
	size, err := syscall.Getxattr(root, rootCookieAttr, buffer)
	if err != nil || size == 0 {
		t.Skipf("文件系统不支持 user xattr（cookie 降级为 (dev,ino) 校验）: %v", err)
	}
	if got := string(buffer[:size]); len(got) < 16 {
		t.Fatalf("cookie 太短: %q", got)
	}
	// cookie 不能变成工作区里的条目。
	if after := readDirNames(t, root); after != before {
		t.Fatalf("Take 之后目录条目变了: %q → %q", before, after)
	}
	if got := snapshot.Files(); got != 2 {
		t.Fatalf("Files() = %d, want 2（cookie 不能被当成文件）", got)
	}
	changes, err := snapshot.Changes()
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("changes = %+v, want none（cookie 不能被当成变更）", changes)
	}
}

// TestRootCookieDetectsReplacementThatReusesTheInode 是本轮失败用例的确定性版本：
// 保持**同一个目录对象**（dev/ino 不变）但抹掉 cookie —— 这正是 ext4 上
// "删除后原地重建、新目录复用旧 inode" 观察到的状态。四个入口都必须报 ErrRootReplaced，
// 且不能写盘。
func TestRootCookieDetectsReplacementThatReusesTheInode(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.txt": "snapshot\n"})
	snapshot := take(t, root, undo.Options{})
	writeTree(t, root, map[string]string{"a.txt": "modified\n"})

	if err := syscall.Removexattr(root, rootCookieAttr); err != nil {
		t.Skipf("文件系统不支持 user xattr（cookie 降级为 (dev,ino) 校验）: %v", err)
	}

	if _, err := snapshot.Changes(); !errors.Is(err, undo.ErrRootReplaced) {
		t.Fatalf("Changes = %v, want ErrRootReplaced（同 inode、无 cookie 必须视为根被换掉）", err)
	}
	if _, err := snapshot.ChangesForPath("a.txt"); !errors.Is(err, undo.ErrRootReplaced) {
		t.Fatalf("ChangesForPath = %v, want ErrRootReplaced", err)
	}
	if _, err := snapshot.Revert(); !errors.Is(err, undo.ErrRootReplaced) {
		t.Fatalf("Revert = %v, want ErrRootReplaced", err)
	}
	if _, err := snapshot.RevertPath("a.txt"); !errors.Is(err, undo.ErrRootReplaced) {
		t.Fatalf("RevertPath = %v, want ErrRootReplaced", err)
	}
	if got := read(t, filepath.Join(root, "a.txt")); got != "modified\n" {
		t.Fatalf("a.txt = %q, want untouched（根不可信时绝不能写盘）", got)
	}
}

// TestSecondTakeAdoptsTheExistingCookie 证明同一根上的多次 Take 沿用同一个 token：
// 否则"下一回合拍的新快照"会让旧快照误判根被换掉（多回合回退全坏）。
func TestSecondTakeAdoptsTheExistingCookie(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.txt": "v1\n"})
	first := take(t, root, undo.Options{})

	buffer := make([]byte, 1024)
	size, err := syscall.Getxattr(root, rootCookieAttr, buffer)
	if err != nil || size == 0 {
		t.Skipf("文件系统不支持 user xattr: %v", err)
	}
	firstToken := string(buffer[:size])

	writeTree(t, root, map[string]string{"a.txt": "v2\n"})
	second := take(t, root, undo.Options{})
	if size, err := syscall.Getxattr(root, rootCookieAttr, buffer); err != nil {
		t.Fatalf("Getxattr: %v", err)
	} else if got := string(buffer[:size]); got != firstToken {
		t.Fatalf("第二次 Take 覆盖了 cookie（%q → %q）：旧快照会被误判成根被换掉", firstToken, got)
	}

	// 旧快照仍然可用，并且能正常回退。
	if _, err := first.Revert(); err != nil {
		t.Fatalf("first.Revert: %v", err)
	}
	if got := read(t, filepath.Join(root, "a.txt")); got != "v1\n" {
		t.Fatalf("a.txt = %q, want v1", got)
	}
	if _, err := second.Changes(); err != nil {
		t.Fatalf("second.Changes: %v", err)
	}
}
