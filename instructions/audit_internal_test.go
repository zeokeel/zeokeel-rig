package instructions

// 对抗审计（内部包测试）：直接攻击未导出的解析与展开函数。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// HEAD 是文件内容，不是"一行文本"的强保证：多行内容必须只取第一行，否则分支名
// 里的换行会注入到 <environment> 块。
func TestAuditParseHeadUsesFirstLineOnly(t *testing.T) {
	cases := []struct {
		name string
		head string
		want string
		ok   bool
	}{
		{"two refs", "ref: refs/heads/main\nref: refs/heads/evil", "main", true},
		{"trailing junk line", "ref: refs/heads/main\n\nignored", "main", true},
		{"detached with second line", strings.Repeat("a", 40) + "\nevil", "detached@" + strings.Repeat("a", 8), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseHead(tc.head)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("parseHead(%q) = (%q, %v), want (%q, %v)", tc.head, got, ok, tc.want, tc.ok)
			}
			if strings.ContainsAny(got, "\r\n") {
				t.Fatalf("parseHead(%q) = %q contains a newline", tc.head, got)
			}
		})
	}
}

// 4 空格缩进的 ``` 是缩进代码块，不是围栏；包文档承诺"可最多缩进 3 格"。
func TestAuditOpenFenceRejectsDeepIndent(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{"```", true},
		{" ```", true},
		{"   ```", true},
		{"    ```", false}, // 4 空格 = 缩进代码块
		{"\t```", false},   // 制表符缩进至少 4 列
		{"    ~~~", false}, //
		{"```go", true},    //
	}
	for _, tc := range cases {
		if _, got := openFence(tc.line); got != tc.want {
			t.Fatalf("openFence(%q) ok = %v, want %v", tc.line, got, tc.want)
		}
	}
}

// 展开必须对任意输入保持 UTF-8 有效、不 panic，并且保留不认识的文本。
func FuzzExpandDocument(f *testing.F) {
	f.Add("plain text\n", "imported\n")
	f.Add("@./a.md\n", "x")
	f.Add("```\n@./a.md\n```\n", "y")
	f.Add("@\"a b.md\"\n", "z")
	f.Add("\x00\xff@\n", "\xff")
	f.Fuzz(func(t *testing.T, content, imported string) {
		dir := t.TempDir()
		doc := filepath.Join(dir, "AGENTS.md")
		if err := os.WriteFile(doc, []byte(content), 0o644); err != nil {
			t.Skipf("write doc: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte(imported), 0o644); err != nil {
			t.Skipf("write import: %v", err)
		}
		got, imports := expandDocument(doc, content, 4)
		if !utf8.ValidString(content) {
			return
		}
		if !utf8.ValidString(got) {
			t.Fatalf("expandDocument produced invalid UTF-8 for valid input %q: %q", content, got)
		}
		_ = imports
	})
}

// 截断必须是 UTF-8 安全的，且不能越界。
func FuzzTruncate(f *testing.F) {
	f.Add("hello world", 5)
	f.Add("中文内容", 4)
	f.Add("", 0)
	f.Add("a", 1)
	f.Fuzz(func(t *testing.T, content string, max int) {
		if !utf8.ValidString(content) {
			return
		}
		got := truncate(content, max)
		if !utf8.ValidString(got) {
			t.Fatalf("truncate(%q, %d) = %q is invalid UTF-8", content, max, got)
		}
	})
}

// 解析任意行都不能 panic。
func FuzzParseImport(f *testing.F) {
	f.Add("@a.md")
	f.Add("@\"./x y.md\"")
	f.Add("@")
	f.Add(strings.Repeat("@", 64))
	f.Fuzz(func(t *testing.T, line string) {
		target, ok := parseImport(line)
		if ok && strings.TrimSpace(target) == "" {
			t.Fatalf("parseImport(%q) accepted an empty target", line)
		}
	})
}
