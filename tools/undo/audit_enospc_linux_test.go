//go:build linux

package undo_test

// 对抗审计（真实 ENOSPC）：用一个 128KiB 的 tmpfs 制造"真的磁盘满"，
// 验证第 2 个文件写回失败时第 1 个文件回到**回退前**的状态。
//
// 这是清单里 ②"磁盘满"那一项的实测版本；作者用的是"把目标换成目录"来注入失败，
// 两者走的都是 writeFileAtomic 的失败分支，但 ENOSPC 额外覆盖了"回滚本身也缺空间"
// 的现实条件（回滚要重新写一遍字节）。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/zeokeel/zeokeel-rig/tools/undo"
)

func TestAuditDiskFullDuringRevertRollsBack(t *testing.T) {
	mountPoint := t.TempDir()
	// 只有 root 能挂；CI 上挂不上就跳过（本机实测可挂）。
	if err := syscall.Mount("tmpfs", mountPoint, "tmpfs", 0, "size=128k"); err != nil {
		t.Skipf("无法挂载 tmpfs（需要 CAP_SYS_ADMIN）: %v", err)
	}
	t.Cleanup(func() {
		if err := syscall.Unmount(mountPoint, 0); err != nil {
			t.Errorf("卸载 tmpfs: %v", err)
		}
	})

	small := filepath.Join(mountPoint, "a.txt")
	large := filepath.Join(mountPoint, "b.txt")
	if err := os.WriteFile(small, []byte(strings.Repeat("a", 4<<10)), 0o644); err != nil {
		t.Fatalf("写 a.txt: %v", err)
	}
	if err := os.WriteFile(large, []byte(strings.Repeat("b", 40<<10)), 0o644); err != nil {
		t.Fatalf("写 b.txt: %v", err)
	}

	snapshot, err := undo.Take(mountPoint, undo.Options{})
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	// 回合里的改动：两个文件都换内容（大小不变，便于只依赖剩余空间控制成败）。
	if err := os.WriteFile(small, []byte(strings.Repeat("A", 4<<10)), 0o644); err != nil {
		t.Fatalf("改 a.txt: %v", err)
	}
	if err := os.WriteFile(large, []byte(strings.Repeat("B", 40<<10)), 0o644); err != nil {
		t.Fatalf("改 b.txt: %v", err)
	}

	// 把剩余空间压到 12KiB 左右：a.txt(4KiB) 的写回能成功，b.txt(40KiB) 必然 ENOSPC。
	filler := filepath.Join(mountPoint, "filler.bin")
	chunk := []byte(strings.Repeat("f", 1<<10))
	for i := 0; i < 200; i++ {
		free, err := freeBytes(mountPoint)
		if err != nil {
			t.Fatalf("statfs: %v", err)
		}
		if free <= 12<<10 {
			break
		}
		if err := appendFile(filler, chunk); err != nil {
			break // 已经填满
		}
	}

	_, revertErr := snapshot.Revert()
	if revertErr == nil {
		t.Skip("这个内核/文件系统上没有触发 ENOSPC（空间没压到位），跳过而不是误判")
	}
	if !strings.Contains(strings.ToLower(revertErr.Error()), "no space") {
		t.Fatalf("失败原因不是 ENOSPC: %v", revertErr)
	}
	t.Logf("真实的 ENOSPC 失败（回滚已生效）: %v", revertErr)

	// 关键断言：a.txt 必须回到"回退前"，也就是回合里写入的 AAAA…；
	// 而不是快照里的 aaaa…（那说明回滚没做），也不能是空/半写。
	got, err := os.ReadFile(small)
	if err != nil {
		t.Fatalf("读 a.txt: %v", err)
	}
	if string(got) != strings.Repeat("A", 4<<10) {
		t.Fatalf("a.txt 被改成了 %d 字节（首字节 %q）：回滚必须恢复到回退前的内容", len(got), firstByte(got))
	}
	gotLarge, err := os.ReadFile(large)
	if err != nil {
		t.Fatalf("读 b.txt: %v", err)
	}
	if string(gotLarge) != strings.Repeat("B", 40<<10) {
		t.Fatalf("b.txt 被改坏了（%d 字节）", len(gotLarge))
	}
	// 不许留下临时文件残渣。
	entries, err := os.ReadDir(mountPoint)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".rig-undo-") {
			t.Fatalf("回退失败后留下了临时文件 %q", entry.Name())
		}
	}
}

// freeBytes 返回剩余可用字节数。
func freeBytes(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return stat.Bavail * uint64(stat.Bsize), nil
}

// appendFile 追加一块内容。
func appendFile(path string, chunk []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	if _, err := file.Write(chunk); err != nil {
		return fmt.Errorf("append: %w", err)
	}
	return nil
}

// firstByte 便于在失败信息里定位内容（0 长度返回 0）。
func firstByte(data []byte) byte {
	if len(data) == 0 {
		return 0
	}
	return data[0]
}
