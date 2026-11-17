package undo

// 对抗审计（包内）：用未导出入口精确覆盖"扫描预算"与"回滚语义"这两处，
// 外部用例只能间接观察。

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// F-U16（major，资源）：scanCurrent 不设总量预算，Changes/Revert 会把每个
// ≤MaxFileBytes 的文件都读进内存 —— 内存随**当前**工作区线性增长。
//
// 快照有 MaxTotalBytes（默认 64MiB），扫描却没有；于是"回合里生成了大量文件"
// 之后，连"撤销"这一步都可能 OOM。OOM 发生在回退途中是最坏时机。
func TestAuditScanCurrentRespectsBudget(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 20; i++ {
		path := filepath.Join(root, "f"+string(rune('a'+i))+".txt")
		if err := os.WriteFile(path, []byte(strings.Repeat("x", 4096)), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	snapshot, err := Take(root, Options{MaxTotalBytes: 8 << 10}) // 预算只够 2 个 4KiB 文件
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	current, err := snapshot.scanCurrent()
	if err != nil {
		t.Fatalf("scanCurrent: %v", err)
	}
	buffered := 0
	for _, state := range current {
		if state.hasContent {
			buffered++
		}
	}
	if buffered > 2 {
		t.Fatalf("scanCurrent 缓冲了 %d 个文件的内容（8KiB 预算只够 2 个）："+
			"扫描没有总量上限，工作区一大就会 OOM", buffered)
	}
	// 未缓冲的文件仍必须能被准确比较（否则会把没改动的文件报成变更）。
	for _, state := range current {
		if state.hasContent {
			continue
		}
		if !state.hasHash {
			t.Fatalf("未缓冲的文件 %q 既没有内容也没有哈希：无法判断是否改动", state.relative)
		}
	}
}

// F-U17（minor）：回滚"新增文件"时，如果该文件在 execute 看到它之前就消失了，
// 回滚步骤会凭空造出一个空文件。
//
// 触发路径：changes() 看到文件存在 → 另一个进程/另一个 undo 调用删掉它 →
// execute 的 os.Remove 得到 ErrNotExist（被忽略）→ 回滚写回 previous(nil) →
// 磁盘上多出一个 0 字节文件。同类错误在"写回"分支里已经有 exists 判断，
// 只有"删除"分支漏了。
func TestAuditExecuteRollbackDoesNotResurrectVanishedFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "keep.txt"), []byte("keep\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	snapshot, err := Take(root, Options{})
	if err != nil {
		t.Fatalf("Take: %v", err)
	}

	// 动作 2 的写回必然失败：把目标路径换成非空目录。
	if err := os.MkdirAll(filepath.Join(root, "z.txt", "child"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	handle, err := snapshot.openRoot()
	if err != nil {
		t.Fatalf("openRoot: %v", err)
	}
	defer func() { _ = handle.Close() }()
	actions := []action{
		// 动作 1：删除一个"新增文件"，但它此刻已经不存在（模拟 scan 与 execute 之间被删）。
		{change: Change{Path: "gone.txt", Op: "add"}, kind: actionRemovePath},
		// 动作 2：写回 z.txt（失败），用来触发对动作 1 的回滚。
		{change: Change{Path: "z.txt", Op: "modify"}, kind: actionWriteFile, content: []byte("snapshot\n"), mode: 0o644},
	}
	if _, err := snapshot.execute(handle, actions); err == nil {
		t.Fatal("必然失败的 execute 却报成功")
	}
	if _, statErr := os.Stat(filepath.Join(root, "gone.txt")); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("回滚凭空造出了 gone.txt（err=%v）：已消失文件的回滚必须是 no-op", statErr)
	}
}
