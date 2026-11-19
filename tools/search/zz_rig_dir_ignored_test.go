package search

// review-1 M6：kit 把 spill 默认写到 <Root>/.rig/spill，而索引侧的忽略名单里没有
// .rig → 每次自动落盘的溢出内容都会进检索索引，并且出现在 git status / gitsnap 的
// `git add -A` 快照里。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRigDirIsNotIndexed(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "notes.md"),
		[]byte("needle in a normal file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spill := filepath.Join(root, ".rig", "spill")
	if err := os.MkdirAll(spill, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spill, "spill.default-x.txt"),
		[]byte("needle inside spilled tool output\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tool := New(root)
	result, err := tool.Invoke(context.Background(), toolCallFor(`{"query":"needle","mode":"text"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %s", result.Content)
	}
	if strings.Contains(result.Content, ".rig") {
		t.Fatalf(".rig 下的文件被检索到了：\n%s", result.Content)
	}
	if !strings.Contains(result.Content, "notes.md") {
		t.Fatalf("普通文件没有命中：\n%s", result.Content)
	}
}
