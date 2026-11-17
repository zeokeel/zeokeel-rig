package builtin_test

// write_file / edit_file 结果里的紧凑摘要行：
//
//	write_file: src/a.txt (2 lines → 3 lines, +2 -1)
//	edit_file: src/a.txt (replace 1 occurrence, +1 -1)
//
// 这是**模型可见文案**，所以断言精确到整行；同时钉住失败路径逐字节不变、以及
// 二进制/超大文件只给字节数。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// summaryLine 取结果里的最后一行（摘要行）。
func summaryLine(t *testing.T, content string) string {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	return lines[len(lines)-1]
}

// W1：覆盖已有文件 → 行数变化 + 增删行数。
func TestWriteFileSummaryOverwrite(t *testing.T) {
	fixture := newFSFixture(t)
	fixture.write(t, "src/a.txt", "one\ntwo\n")

	result := fixture.invoke(t, "write_file", `{"path":"src/a.txt","content":"one\nTWO\nthree\n"}`)
	requireContains(t, result.Content, "wrote 14 bytes to src/a.txt")
	if got, want := summaryLine(t, result.Content), "write_file: src/a.txt (2 lines → 3 lines, +2 -1)"; got != want {
		t.Fatalf("摘要行 = %q, want %q（完整结果 %q）", got, want, result.Content)
	}
}

// W2：新建文件 → created + 行数。
func TestWriteFileSummaryCreate(t *testing.T) {
	fixture := newFSFixture(t)
	result := fixture.invoke(t, "write_file", `{"path":"docs/new.md","content":"# New\n\nbody\n"}`)
	if got, want := summaryLine(t, result.Content), "write_file: docs/new.md (created, 3 lines)"; got != want {
		t.Fatalf("摘要行 = %q, want %q", got, want)
	}

	// 单数形式。
	result = fixture.invoke(t, "write_file", `{"path":"one.txt","content":"single\n"}`)
	if got, want := summaryLine(t, result.Content), "write_file: one.txt (created, 1 line)"; got != want {
		t.Fatalf("摘要行 = %q, want %q", got, want)
	}
}

// W3：内容未变的重写 → +0 -0（不谎报改动）。
func TestWriteFileSummaryNoChange(t *testing.T) {
	fixture := newFSFixture(t)
	fixture.write(t, "same.txt", "a\nb\n")
	result := fixture.invoke(t, "write_file", `{"path":"same.txt","content":"a\nb\n"}`)
	if got, want := summaryLine(t, result.Content), "write_file: same.txt (2 lines → 2 lines, +0 -0)"; got != want {
		t.Fatalf("摘要行 = %q, want %q", got, want)
	}
}

// W4：覆盖二进制文件 → 不给行数，只给字节数。
func TestWriteFileSummaryBinary(t *testing.T) {
	fixture := newFSFixture(t)
	path := filepath.Join(fixture.root, "logo.png")
	if err := os.WriteFile(path, []byte{0x89, 'P', 'N', 'G', 0x00, 0x01}, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	result := fixture.invoke(t, "write_file", `{"path":"logo.png","content":"plain text\n"}`)
	if got, want := summaryLine(t, result.Content), "write_file: logo.png (6 bytes → 11 bytes)"; got != want {
		t.Fatalf("摘要行 = %q, want %q", got, want)
	}
}

// W5：覆盖超大文件（>1 MiB）→ 不比较，只给字节数（且不会把整个文件读进内存）。
func TestWriteFileSummaryOversize(t *testing.T) {
	fixture := newFSFixture(t)
	big := strings.Repeat("x", (1<<20)+10)
	fixture.write(t, "big.txt", big)

	result := fixture.invoke(t, "write_file", `{"path":"big.txt","content":"small\n"}`)
	want := "write_file: big.txt (1048586 bytes → 6 bytes)"
	if got := summaryLine(t, result.Content); got != want {
		t.Fatalf("摘要行 = %q, want %q", got, want)
	}
}

// E1：edit_file 单处替换 → replace 1 occurrence + 行数变化。
func TestEditFileSummarySingle(t *testing.T) {
	fixture := newFSFixture(t)
	fixture.write(t, "src/a.go", "package a\n\nfunc foo() {}\n")
	result := fixture.invoke(t, "edit_file",
		`{"path":"src/a.go","old_string":"func foo() {}","new_string":"func bar() {}\nfunc baz() {}"}`)
	requireContains(t, result.Content, "edited src/a.go: replaced 1 occurrence(s)")
	if got, want := summaryLine(t, result.Content), "edit_file: src/a.go (replace 1 occurrence, +2 -1)"; got != want {
		t.Fatalf("摘要行 = %q, want %q（完整 %q）", got, want, result.Content)
	}
}

// E2：replace_all → 复数形式 + 累计增删。
func TestEditFileSummaryReplaceAll(t *testing.T) {
	fixture := newFSFixture(t)
	fixture.write(t, "dup.txt", "x\nx\n")
	result := fixture.invoke(t, "edit_file",
		`{"path":"dup.txt","old_string":"x","new_string":"y","replace_all":true}`)
	if got, want := summaryLine(t, result.Content), "edit_file: dup.txt (replace 2 occurrences, +2 -2)"; got != want {
		t.Fatalf("摘要行 = %q, want %q", got, want)
	}
}

// E3：二进制目标 → 只给字节数。
func TestEditFileSummaryBinary(t *testing.T) {
	fixture := newFSFixture(t)
	path := filepath.Join(fixture.root, "data.bin")
	if err := os.WriteFile(path, []byte("abc\x00def"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	result := fixture.invoke(t, "edit_file", `{"path":"data.bin","old_string":"abc","new_string":"xyz"}`)
	if got, want := summaryLine(t, result.Content), "edit_file: data.bin (replace 1 occurrence, 7 bytes → 7 bytes)"; got != want {
		t.Fatalf("摘要行 = %q, want %q", got, want)
	}
}

// F1【逐字节不变】：失败路径的结果文本里不应出现摘要行。
func TestFailurePathsHaveNoSummaryLine(t *testing.T) {
	fixture := newFSFixture(t)
	fixture.write(t, "a.txt", "one\n")

	cases := []struct {
		name string
		args string
		want string
	}{
		{"write_file 缺 path", `{"content":"x"}`, "write_file: path is required"},
		{"edit_file 空 old_string", `{"path":"a.txt","old_string":"","new_string":"x"}`, "edit_file: old_string must not be empty"},
		{"edit_file 目标不存在", `{"path":"a.txt","old_string":"nope","new_string":"x"}`, "edit_file: old_string not found in a.txt"},
		{"edit_file 匹配不唯一", `{"path":"dup.txt","old_string":"x","new_string":"y"}`,
			"edit_file: old_string occurs 2 times in dup.txt; set replace_all=true or make old_string unique"},
	}
	fixture.write(t, "dup.txt", "x\nx\n")

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text := fixture.invokeError(t, strings.Split(tc.name, " ")[0], tc.args)
			if text != tc.want {
				t.Fatalf("失败文案 = %q, want %q（失败路径必须逐字节不变）", text, tc.want)
			}
			requireNotContains(t, text, "\n", "lines →", "occurrence,")
		})
	}
}
