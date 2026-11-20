package undo_test

// doc.go 的目录布局守护：包文档是用户读到的第一手说明，写错布局比没有说明更糟。

import (
	"os"
	"strings"
	"testing"
)

// TestDocDescribesActualSnapshotLayout 断言包文档写的是真实的快照布局。
//
// 实现（file_backend.go）是 `<Dir>/turns/<turn>/{manifest.json,files/<rel>}`，清单与
// 镜像分两个命名空间；文档若还写旧布局 `<Dir>/<turn>/`，用户会去翻一个不存在的目录，
// 也会以为清单会和自己的 `manifest.json` 撞名。
func TestDocDescribesActualSnapshotLayout(t *testing.T) {
	data, err := os.ReadFile("doc.go")
	if err != nil {
		t.Fatalf("read doc.go: %v", err)
	}
	doc := string(data)

	if strings.Contains(doc, "<Dir>/<turn>/") {
		t.Error("doc.go 仍写着旧布局 `<Dir>/<turn>/`；实现是 `<Dir>/turns/<turn>/files/<rel>`")
	}
	for _, want := range []string{
		"<Dir>/turns/<turn>/files/<rel>",
		"<Dir>/turns/<turn>/manifest.json",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("doc.go 没有写明真实布局 %q", want)
		}
	}
}
