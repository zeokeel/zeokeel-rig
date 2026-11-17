package search

// 硬链接守卫：工作区里的硬链接文件与根之外共享同一 inode，内容检索会读到
// "根之外的数据"（虽然不是通过路径逃逸）。默认不改变行为（兼容），
// WithHardlinkGuard(true) 时跳过 Nlink > 1 的文件。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeHardlinkPair 在 root 里建 a.txt 并给它建一个硬链接 b.txt。
func writeHardlinkPair(t *testing.T, root string) (string, string) {
	t.Helper()
	a := filepath.Join(root, "a.txt")
	if err := os.WriteFile(a, []byte("needle in a haystack\n"), 0o644); err != nil {
		t.Fatalf("写文件: %v", err)
	}
	b := filepath.Join(root, "b.txt")
	if err := os.Link(a, b); err != nil {
		t.Skipf("本文件系统不支持硬链接: %v", err)
	}
	return a, b
}

// 默认关闭：硬链接文件照常被索引（行为不变）。
func TestHardlinkGuardOffIndexesBothNames(t *testing.T) {
	root := t.TempDir()
	writeHardlinkPair(t, root)

	got := invoke(t, New(root), `{"query":"needle"}`)
	if got.IsError {
		t.Fatalf("Invoke 失败: %s", got.Content)
	}
	if !strings.Contains(got.Content, "a.txt") || !strings.Contains(got.Content, "b.txt") {
		t.Fatalf("默认（guard 关）应同时索引 a.txt 与 b.txt: %s", got.Content)
	}
}

// 开启守卫：Nlink > 1 的文件被跳过。
func TestHardlinkGuardSkipsMultiplyLinkedFiles(t *testing.T) {
	root := t.TempDir()
	writeHardlinkPair(t, root)
	// 对照组：一个正常文件，必须仍然被索引。
	normal := filepath.Join(root, "normal.txt")
	if err := os.WriteFile(normal, []byte("needle normal\n"), 0o644); err != nil {
		t.Fatalf("写文件: %v", err)
	}

	got := invoke(t, New(root, WithHardlinkGuard(true)), `{"query":"needle"}`)
	if got.IsError {
		t.Fatalf("Invoke 失败: %s", got.Content)
	}
	if strings.Contains(got.Content, "a.txt") || strings.Contains(got.Content, "b.txt") {
		t.Fatalf("guard 开启后不应出现硬链接文件: %s", got.Content)
	}
	if !strings.Contains(got.Content, "normal.txt") {
		t.Fatalf("guard 不应连正常文件一起跳过: %s", got.Content)
	}

	// 文件名模式（files）也走同一套遍历，必须一致。
	files := invoke(t, New(root, WithHardlinkGuard(true)), `{"query":"a.txt","mode":"files"}`)
	if strings.Contains(files.Content, "a.txt") {
		t.Fatalf("files 模式也应跳过硬链接: %s", files.Content)
	}
}
