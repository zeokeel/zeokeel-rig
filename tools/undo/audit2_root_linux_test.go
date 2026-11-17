//go:build linux

package undo_test

// 第二轮对抗审计（Linux 专属）：把"根被替换"的三种真实形态各验一遍，
// 其中 bind mount 是最容易被忽略的一种（路径没变、名字没变，只有 inode 变了）。

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/zeokeel/zeokeel-rig/tools/undo"
)

// A8：根被 bind mount 换成另一个目录 —— 路径不变、inode 变，必须被 ErrRootReplaced 拦住。
func TestAuditRootReplacedByBindMount(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "ws")
	w(t, filepath.Join(root, "a.txt"), "original\n")
	snapshot, err := undo.Take(root, undo.Options{})
	if err != nil {
		t.Fatalf("Take: %v", err)
	}

	impostor := filepath.Join(parent, "impostor")
	w(t, filepath.Join(impostor, "b.txt"), "impostor\n")
	if err := syscall.Mount(impostor, root, "", syscall.MS_BIND, ""); err != nil {
		t.Skipf("无法 bind mount（需要 CAP_SYS_ADMIN）: %v", err)
	}
	t.Cleanup(func() {
		if err := syscall.Unmount(root, syscall.MNT_DETACH); err != nil {
			t.Errorf("卸载 bind mount: %v", err)
		}
	})

	// 现在 root 这个名字下面已经是 impostor 的 inode 了。
	if _, err := snapshot.Changes(); !errors.Is(err, undo.ErrRootReplaced) {
		t.Fatalf("bind mount 换根之后 Changes = %v, want ErrRootReplaced", err)
	}
	if _, err := snapshot.Revert(); !errors.Is(err, undo.ErrRootReplaced) {
		t.Fatalf("bind mount 换根之后 Revert = %v, want ErrRootReplaced", err)
	}
	// 冒名目录里不能被写入快照内容。
	if got := r(t, filepath.Join(impostor, "b.txt")); got != "impostor\n" {
		t.Fatalf("冒名目录被改写: %q", got)
	}
	if _, statErr := os.Stat(filepath.Join(impostor, "a.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("快照内容被倒进了 bind mount 的目标: %v", statErr)
	}
}
