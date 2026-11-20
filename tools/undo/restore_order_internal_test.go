package undo

// 包内测试：restoreFile 的失败路径必须与工作区的真实状态自洽。

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRestoreFileReadsCopyBeforeRemovingTarget 钉住 restoreFile 的顺序：
// **先读副本、再删目标**。
//
// 旧实现是先 `Remove(target)` 再 `ReadFile(source)`：副本恰好读不出来（并发删除、
// I/O 错误）时用户的文件已经被删了，调用方却把它报成 "...left as-is"。这里用
// readSnapshotCopy 这个替身点模拟"检查通过之后读失败"，断言原文件一个字节没动。
func TestRestoreFileReadsCopyBeforeRemovingTarget(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "a.txt")
	if err := os.WriteFile(target, []byte("v2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	sourceDir := t.TempDir()
	source := filepath.Join(sourceDir, "copy.txt")
	if err := os.WriteFile(source, []byte("v1\n"), 0o644); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}

	original := readSnapshotCopy
	t.Cleanup(func() { readSnapshotCopy = original })
	readSnapshotCopy = func(string) ([]byte, error) {
		return nil, errors.New("injected read failure")
	}

	err := restoreFile(root, source, target, 0o644)
	if err == nil {
		t.Fatal("副本读失败时 restoreFile 没有报错")
	}
	if !strings.Contains(err.Error(), "left as-is") {
		t.Errorf("错误文案 = %q；副本读失败时工作区没被改动，文案必须说明 left as-is", err)
	}
	data, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatalf("副本读失败却把工作区原文件删了（旧实现的 bug）: %v", readErr)
	}
	if string(data) != "v2\n" {
		t.Fatalf("目标内容 = %q, want %q（副本读失败时不能改动工作区）", data, "v2\n")
	}
}

// TestRestoreFileSaysTargetWasRemovedWhenWriteFails 钉住另一半失败路径的文案：
// 副本读成功之后写失败时，目标确实已经被删掉，文案必须明说，不能再报 left as-is。
func TestRestoreFileSaysTargetWasRemovedWhenWriteFails(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "a.txt")
	if err := os.WriteFile(target, []byte("v2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	sourceDir := t.TempDir()
	source := filepath.Join(sourceDir, "copy.txt")
	if err := os.WriteFile(source, []byte("v1\n"), 0o644); err != nil {
		t.Fatalf("WriteFile source: %v", err)
	}

	originalWrite := writeRestoredFile
	t.Cleanup(func() { writeRestoredFile = originalWrite })
	writeRestoredFile = func(string, []byte, os.FileMode) error {
		return errors.New("injected write failure")
	}

	err := restoreFile(root, source, target, 0o644)
	if err == nil {
		t.Fatal("写失败时 restoreFile 没有报错")
	}
	if !strings.Contains(err.Error(), "was removed but could not be restored") {
		t.Errorf("错误文案 = %q；写失败时原文件已经被删，文案必须说明", err)
	}
	if _, statErr := os.Lstat(target); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("目标仍在（statErr=%v）：替身点没有走到「先删再建」的路径", statErr)
	}
}
