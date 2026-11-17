package instructions

// 内部实现细节的单元测试：@import 指令解析、行切分、截断、路径显示与 HEAD 解析。
// 这些函数的行为会直接决定模型可见文本的形状，因此单独钉住，避免只靠端到端用例
// 间接覆盖。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestParseImportDirective 覆盖指令识别的全部边界。
func TestParseImportDirective(t *testing.T) {
	cases := []struct {
		name   string
		line   string
		want   string
		wantOK bool
	}{
		{"bare path", "@a.md", "a.md", true},
		{"relative path", "@./dir/a.md", "./dir/a.md", true},
		{"trailing spaces", "  @a.md  ", "a.md", true},
		{"double quoted", `@"./x y.md"`, "./x y.md", true},
		{"single quoted", `@'./x y.md'`, "./x y.md", true},
		{"quoted empty", `@""`, "", false},
		{"at alone", "@", "", false},
		{"at with spaces only", "@   ", "", false},
		{"unquoted spaces", "@not a path", "", false},
		{"inline mention", "see @a.md here", "", false},
		{"no at", "a.md", "", false},
		{"empty line", "", "", false},
		{"email-like", "@user@example.com", "user@example.com", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseImport(tc.line)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("parseImport(%q) = (%q, %v), want (%q, %v)", tc.line, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestSplitLinesKeepingEnds 证明切分保留行尾换行，且末尾空行不会被丢失。
func TestSplitLinesKeepingEnds(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    []string
	}{
		{"empty", "", nil},
		{"no newline", "abc", []string{"abc"}},
		{"trailing newline", "a\nb\n", []string{"a\n", "b\n"}},
		{"blank line", "a\n\nb", []string{"a\n", "\n", "b"}},
		{"crlf kept", "a\r\nb\r\n", []string{"a\r\n", "b\r\n"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitLinesKeepingEnds(tc.content)
			if len(got) != len(tc.want) {
				t.Fatalf("splitLinesKeepingEnds(%q) = %q, want %q", tc.content, got, tc.want)
			}
			for index := range got {
				if got[index] != tc.want[index] {
					t.Fatalf("splitLinesKeepingEnds(%q) = %q, want %q", tc.content, got, tc.want)
				}
			}
		})
	}
}

// TestTruncateBoundaries 覆盖"零上限不截断""恰好等长""多字节安全"与尾注字节数。
func TestTruncateBoundaries(t *testing.T) {
	cases := []struct {
		name    string
		content string
		max     int
		want    string
	}{
		{"zero max is no-op", "abcdef", 0, "abcdef"},
		{"exact fit", "abcdef", 6, "abcdef"},
		{"cut ascii", "abcdef", 3, "abc\n…[truncated: 3 bytes omitted]"},
		{"cut multibyte", "aé", 2, "a\n…[truncated: 2 bytes omitted]"},
		{"empty content", "", 1, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := truncate(tc.content, tc.max); got != tc.want {
				t.Fatalf("truncate(%q, %d) = %q, want %q", tc.content, tc.max, got, tc.want)
			}
		})
	}
}

// TestDisplayPath 覆盖相对/越界/空工作区三种情况。
func TestDisplayPath(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "ws")
	cases := []struct {
		name          string
		path          string
		workspaceRoot string
		want          string
	}{
		{"inside root", filepath.Join(root, "sub", "AGENTS.md"), root, "sub/AGENTS.md"},
		{"root itself", filepath.Join(root, "AGENTS.md"), root, "AGENTS.md"},
		{"outside root", filepath.Join(string(filepath.Separator), "home", "AGENTS.md"), root, "/home/AGENTS.md"},
		{"empty root", filepath.Join(root, "AGENTS.md"), "", "/ws/AGENTS.md"},
		{"sibling prefix", filepath.Join(root, "..", "ws2", "AGENTS.md"), root, "/ws2/AGENTS.md"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := displayPath(tc.path, tc.workspaceRoot); got != tc.want {
				t.Fatalf("displayPath(%q, %q) = %q, want %q", tc.path, tc.workspaceRoot, got, tc.want)
			}
		})
	}
}

// TestParseHead 覆盖普通分支、路径形式分支、detached 与垃圾输入。
func TestParseHead(t *testing.T) {
	cases := []struct {
		name   string
		head   string
		want   string
		wantOK bool
	}{
		{"branch", "ref: refs/heads/main\n", "main", true},
		{"nested branch", "ref: refs/heads/feature/x\n", "feature/x", true},
		{"detached", "0123456789abcdef0123456789abcdef01234567\n", "detached@01234567", true},
		{"short sha", "abc123\n", "", false},
		{"empty", "  \n", "", false},
		{"ref without name", "ref: refs/heads/\n", "", false},
		// 只剥 refs/heads/：非分支引用整段保留，避免把标签伪装成分支名。
		{"tag ref stays qualified", "ref: refs/tags/v1\n", "refs/tags/v1", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseHead(tc.head)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("parseHead(%q) = (%q, %v), want (%q, %v)", tc.head, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestReadGitBranchEdgeCases 覆盖 HEAD 缺失、.git 文件不是 gitdir、空目录。
func TestReadGitBranchEdgeCases(t *testing.T) {
	t.Run("no repository", func(t *testing.T) {
		if branch, ok := readGitBranch(t.TempDir()); ok || branch != "" {
			t.Fatalf("readGitBranch = (%q, %v), want no branch", branch, ok)
		}
	})
	t.Run("empty dir argument", func(t *testing.T) {
		if branch, ok := readGitBranch(""); ok || branch != "" {
			t.Fatalf("readGitBranch(\"\") = (%q, %v), want no branch", branch, ok)
		}
	})
	t.Run("missing HEAD file", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if branch, ok := readGitBranch(root); ok || branch != "" {
			t.Fatalf("readGitBranch = (%q, %v), want no branch without HEAD", branch, ok)
		}
	})
	t.Run("git file without gitdir", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, ".git"), []byte("nonsense\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if branch, ok := readGitBranch(root); ok || branch != "" {
			t.Fatalf("readGitBranch = (%q, %v), want no branch", branch, ok)
		}
	})
	t.Run("relative gitdir", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "realgit"), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		writeHeadFixture(t, filepath.Join(root, "realgit", "HEAD"), "ref: refs/heads/relative\n")
		if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: realgit\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if branch, ok := readGitBranch(root); !ok || branch != "relative" {
			t.Fatalf("readGitBranch = (%q, %v), want relative", branch, ok)
		}
	})
}

// TestExpandSelfImportIsCycle 证明文档 import 自己不会无限递归。
func TestExpandSelfImportIsCycle(t *testing.T) {
	root := t.TempDir()
	document := filepath.Join(root, "AGENTS.md")
	if err := os.WriteFile(document, []byte("x\n@AGENTS.md\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	expanded, imports := expandDocument(document, "x\n@AGENTS.md\n", DefaultMaxImportDepth)
	want := "x\n<!-- import skipped: AGENTS.md (cycle) -->\n"
	if expanded != want {
		t.Fatalf("expanded = %q, want %q", expanded, want)
	}
	if len(imports) != 1 || imports[0] != "AGENTS.md" {
		t.Fatalf("imports = %v, want [AGENTS.md]", imports)
	}
}

// TestExpandDedupesImportRecords 证明重复 import 目标只记录一次，但内容各展开一次。
func TestExpandDedupesImportRecords(t *testing.T) {
	root := t.TempDir()
	document := filepath.Join(root, "AGENTS.md")
	child := filepath.Join(root, "a.md")
	if err := os.WriteFile(child, []byte("A\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	content := "@a.md\n@a.md\n"
	if err := os.WriteFile(document, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	expanded, imports := expandDocument(document, content, DefaultMaxImportDepth)
	if want := "A\nA\n"; expanded != want {
		t.Fatalf("expanded = %q, want %q", expanded, want)
	}
	if len(imports) != 1 {
		t.Fatalf("imports = %v, want a single record", imports)
	}
}

// TestExpandImportDirectoryIsNotFound 证明 import 指向目录时按"找不到"处理，
// 不会把目录内容当文档读入。
func TestExpandImportDirectoryIsNotFound(t *testing.T) {
	root := t.TempDir()
	document := filepath.Join(root, "AGENTS.md")
	if err := os.MkdirAll(filepath.Join(root, "parts"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	expanded, _ := expandDocument(document, "@parts\n", DefaultMaxImportDepth)
	if want := "<!-- import not found: parts -->\n"; expanded != want {
		t.Fatalf("expanded = %q, want %q", expanded, want)
	}
}

// TestDisplayPathIsSlashNormalized 证明标题路径用 "/" 分隔（Windows 上不出现 "\\"）。
func TestDisplayPathIsSlashNormalized(t *testing.T) {
	root := t.TempDir()
	got := displayPath(filepath.Join(root, "a", "AGENTS.md"), root)
	if strings.Contains(got, `\`) {
		t.Fatalf("displayPath = %q, want forward slashes", got)
	}
	if got != "a/AGENTS.md" {
		t.Fatalf("displayPath = %q, want a/AGENTS.md", got)
	}
}

// TestDetectEnvironmentKeepsCallerTime 证明 DetectEnvironment 不改写入参时间，
// Render 才做 UTC 归一化。
func TestDetectEnvironmentKeepsCallerTime(t *testing.T) {
	zone := time.FixedZone("UTC-5", -5*3600)
	now := time.Date(2026, 11, 17, 22, 0, 0, 0, zone)
	environment := DetectEnvironment(t.TempDir(), t.TempDir(), now)
	if !environment.Time.Equal(now) {
		t.Fatalf("Time = %v, want the caller value %v", environment.Time, now)
	}
}

// writeHeadFixture 写入一个 HEAD 夹具。
func writeHeadFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}
