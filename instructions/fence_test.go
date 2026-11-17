package instructions_test

// 围栏代码块（``` 与 ~~~）里的 @path 不得被当作 import 指令展开。
//
// 这是真实会咬人的坑：AGENTS.md 里经常有"如何写 import"的示例代码块，若把示例
// 里的 @path 真展开，模型会收到一份凭空多出来的文件内容。这些用例钉住状态机的
// 全部边角：两种标记、info string、围栏长度、标记不匹配、未闭合到文件尾，以及
// 被 import 进来的文件里自己的围栏。

import (
	"path/filepath"
	"testing"

	"github.com/zeokeel/zeokeel-rig/instructions"
)

// loadLocal 在临时仓库里写入 AGENTS.md 与若干 import 目标，返回展开后的内容。
func loadLocal(t *testing.T, document string, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	mkdir(t, filepath.Join(root, ".git"))
	writeFile(t, filepath.Join(root, "AGENTS.md"), document)
	for name, content := range files {
		writeFile(t, filepath.Join(root, name), content)
	}
	set, err := instructions.Load(root, instructions.WithUserPath(""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return findDoc(t, set, filepath.Join(root, "AGENTS.md")).Content
}

// TestImportInsideBacktickFenceIsNotExpanded 证明 ``` 围栏内的 @path 保持原文。
func TestImportInsideBacktickFenceIsNotExpanded(t *testing.T) {
	document := "before\n```\n@a.md\n```\nafter\n"
	got := loadLocal(t, document, map[string]string{"a.md": "imported\n"})
	if got != document {
		t.Fatalf("content = %q, want the fenced block untouched %q", got, document)
	}
}

// TestImportInsideTildeFenceIsNotExpanded 证明 ~~~ 围栏内的 @path 保持原文。
func TestImportInsideTildeFenceIsNotExpanded(t *testing.T) {
	document := "~~~\n@a.md\n~~~\n"
	got := loadLocal(t, document, map[string]string{"a.md": "imported\n"})
	if got != document {
		t.Fatalf("content = %q, want the fenced block untouched %q", got, document)
	}
}

// TestImportOutsideFenceIsExpanded 证明围栏只保护它自己内部：围栏前后的 import
// 正常展开。
func TestImportOutsideFenceIsExpanded(t *testing.T) {
	document := "@a.md\n```\n@b.md\n```\n@c.md\n"
	got := loadLocal(t, document, map[string]string{
		"a.md": "A\n",
		"b.md": "B\n",
		"c.md": "C\n",
	})
	want := "A\n```\n@b.md\n```\nC\n"
	if got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
}

// TestFenceInfoStringDoesNotCloseFence 证明带 info string 的围栏行（```go）在块内
// 仍是内容，只有"纯围栏"才关闭代码块。
func TestFenceInfoStringDoesNotCloseFence(t *testing.T) {
	document := "```go\n@a.md\n```go\n@b.md\n```\n"
	got := loadLocal(t, document, map[string]string{
		"a.md": "A\n",
		"b.md": "B\n",
	})
	if got != document {
		t.Fatalf("content = %q, want the whole fenced block untouched %q", got, document)
	}
}

// TestFenceLongerRunNeedsLongerClose 证明开启围栏比关闭围栏长时，短围栏行只是内容。
func TestFenceLongerRunNeedsLongerClose(t *testing.T) {
	document := "````\n@a.md\n```\n@b.md\n````\n@c.md\n"
	got := loadLocal(t, document, map[string]string{
		"a.md": "A\n",
		"b.md": "B\n",
		"c.md": "C\n",
	})
	want := "````\n@a.md\n```\n@b.md\n````\nC\n"
	if got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
}

// TestFenceMarkerMismatchKeepsBlockOpen 证明 ~~~ 不会关闭 ``` 围栏（反之亦然）。
func TestFenceMarkerMismatchKeepsBlockOpen(t *testing.T) {
	document := "```\n@a.md\n~~~\n@b.md\n```\n"
	got := loadLocal(t, document, map[string]string{
		"a.md": "A\n",
		"b.md": "B\n",
	})
	if got != document {
		t.Fatalf("content = %q, want the mismatched marker to stay inside the block %q", got, document)
	}
}

// TestUnclosedFenceStopsExpansion 证明未闭合的围栏一直保护到文件尾。
func TestUnclosedFenceStopsExpansion(t *testing.T) {
	document := "```\n@a.md\n"
	got := loadLocal(t, document, map[string]string{"a.md": "A\n"})
	if got != document {
		t.Fatalf("content = %q, want the unclosed block untouched %q", got, document)
	}
}

// TestImportedFileFencesAreRespected 证明被 import 的文件里自己的围栏同样生效
// （每个文件的围栏状态独立）。
func TestImportedFileFencesAreRespected(t *testing.T) {
	document := "@a.md\n@b.md\n"
	got := loadLocal(t, document, map[string]string{
		"a.md": "```\n@b.md\n```\n",
		"b.md": "B\n",
	})
	want := "```\n@b.md\n```\nB\n"
	if got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
}

// TestFenceWithIndentIsRecognized 证明最多三格缩进的围栏仍被识别（CommonMark 允许）。
func TestFenceWithIndentIsRecognized(t *testing.T) {
	document := "  ```\n@a.md\n  ```\n"
	got := loadLocal(t, document, map[string]string{"a.md": "A\n"})
	if got != document {
		t.Fatalf("content = %q, want the indented fence to protect its body %q", got, document)
	}
}
