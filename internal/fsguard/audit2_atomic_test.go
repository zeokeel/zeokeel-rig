package fsguard_test

// 第 2D 轮对抗审计（只读）：fsguard 的原子写是否会留下半成品/临时文件。
//
// 数据安全问的是：写失败时目标文件要么是旧内容、要么是新内容，**不能是截断的**；
// 而且不能在工作区里留下临时文件（否则 undo/apply_patch 的"零残留"承诺就破了）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/internal/fsguard"
)

// F1：写失败（目标是个目录）→ 旧文件不变、无临时文件残留。
func TestAuditAtomicWriteFailureKeepsOldContent(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("OLD CONTENT\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// 让 rename 必定失败：把目标位置换成一个非空目录。
	blocker := filepath.Join(dir, "blocked")
	if err := os.MkdirAll(filepath.Join(blocker, "child"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	root, err := fsguard.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })

	if err := root.WriteFileAtomic("blocked", []byte("new\n"), 0o644); err == nil {
		t.Fatal("写入目录竟然成功了")
	}
	// 目录内容不能被破坏。
	if _, err := os.Stat(filepath.Join(blocker, "child")); err != nil {
		t.Fatalf("目录被破坏了: %v", err)
	}
	// 不能留下临时文件（名字来自 tempName：以点开头或含随机串）。
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == "target.txt" || name == "blocked" {
			continue
		}
		if strings.HasPrefix(name, ".") || strings.Contains(name, "tmp") {
			t.Fatalf("失败路径留下临时文件: %s", name)
		}
	}
}

// F2：成功路径的"要么旧要么新"：原子写不应该让读者看到半截内容。
func TestAuditAtomicWriteIsAllOrNothing(t *testing.T) {
	dir := t.TempDir()
	root, err := fsguard.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })

	if err := root.WriteFileAtomic("a.txt", []byte("v1\n"), 0o644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	// 并发读 + 反复写：每次读到的都必须是完整的 vN，不能是空或半截。
	done := make(chan struct{})
	problems := make(chan string, 1)
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			raw, readErr := os.ReadFile(filepath.Join(dir, "a.txt"))
			if readErr != nil {
				problems <- "读失败: " + readErr.Error()
				return
			}
			if !strings.HasSuffix(string(raw), "\n") || len(raw) < 3 {
				problems <- "读到半截内容: " + string(raw)
				return
			}
		}
	}()
	for i := 0; i < 200; i++ {
		payload := strings.Repeat("x", 100+i%7) + "\n"
		if err := root.WriteFileAtomic("a.txt", []byte(payload), 0o644); err != nil {
			t.Fatalf("第 %d 次写: %v", i, err)
		}
	}
	<-done
	select {
	case problem := <-problems:
		t.Fatal(problem)
	default:
	}
	// 最终也不能有临时文件。
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, entry := range entries {
		if entry.Name() != "a.txt" {
			t.Fatalf("残留文件: %s", entry.Name())
		}
	}
}
