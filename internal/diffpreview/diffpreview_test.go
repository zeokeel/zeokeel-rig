package diffpreview_test

// diffpreview 的行为测试：统一 diff 的字节级快照、降级规则、Summary 文案，
// 以及最重要的**往返性质**——把我生成的 diff 反应用一遍，必须逐字节得到新内容。

import (
	"bytes"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/internal/diffpreview"
)

// golden 比较输出与 testdata 下的快照；-update 重新生成。
var update = flag.Bool("update", false, "重新生成 golden")

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("写 golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 golden %s（首次运行请加 -update）: %v", path, err)
	}
	if got != string(want) {
		t.Fatalf("%s 与 golden 不一致\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

// P1：修改文件的统一 diff（含 3 行上下文、hunk 头省略 ",1"）。
func TestPreviewModifyGolden(t *testing.T) {
	old := []byte("line1\nline2\nline3\nline4\nline5\nline6\nline7\n")
	new := []byte("line1\nline2\nline3\nCHANGED\nline5\nline6\nline7\n")
	got, err := diffpreview.Preview("src/a.txt", old, new)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	golden(t, "modify.diff", got)
	if !strings.HasPrefix(got, "--- a/src/a.txt\n+++ b/src/a.txt\n@@ ") {
		t.Fatalf("头部格式不对:\n%s", got)
	}
}

// P2：新增文件（--- /dev/null）。
func TestPreviewAddGolden(t *testing.T) {
	got, err := diffpreview.Preview("docs/new.md", nil, []byte("# New\n\nbody\n"))
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	golden(t, "add.diff", got)
	if !strings.HasPrefix(got, "--- /dev/null\n+++ b/docs/new.md\n@@ -0,0 +1,3 @@\n") {
		t.Fatalf("新增文件的头不对:\n%s", got)
	}
}

// P3：删除文件（+++ /dev/null）。
func TestPreviewDeleteGolden(t *testing.T) {
	got, err := diffpreview.Preview("old.txt", []byte("a\nb\n"), nil)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	golden(t, "delete.diff", got)
	if !strings.HasPrefix(got, "--- a/old.txt\n+++ /dev/null\n@@ -1,2 +0,0 @@\n") {
		t.Fatalf("删除文件的头不对:\n%s", got)
	}
}

// P4：两处改动离得远 → 两个 hunk；离得近（中间只隔 ≤6 行）→ 合成一个。
func TestPreviewHunkGrouping(t *testing.T) {
	var oldLines, newLines []string
	for i := 1; i <= 40; i++ {
		oldLines = append(oldLines, fmt.Sprintf("line%02d", i))
		newLines = append(newLines, fmt.Sprintf("line%02d", i))
	}
	newLines[4] = "changed04"  // 第 5 行
	newLines[34] = "changed34" // 第 35 行
	got, err := diffpreview.Preview("f.txt", []byte(strings.Join(oldLines, "\n")+"\n"), []byte(strings.Join(newLines, "\n")+"\n"))
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if count := strings.Count(got, "@@ -"); count != 2 {
		t.Fatalf("相隔 30 行的两处改动应产生 2 个 hunk，实际 %d 个:\n%s", count, got)
	}

	// 相邻改动（中间只隔 3 行 = 2*context 以内）→ 一个 hunk。
	near := append([]string(nil), oldLines...)
	near[4] = "changed04"
	near[10] = "changed10"
	got, err = diffpreview.Preview("f.txt", []byte(strings.Join(oldLines, "\n")+"\n"), []byte(strings.Join(near, "\n")+"\n"))
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if count := strings.Count(got, "@@ -"); count != 1 {
		t.Fatalf("相隔 5 行的两处改动应合成 1 个 hunk，实际 %d 个:\n%s", count, got)
	}
}

// P5：二进制（含 NUL）降级为摘要，不产生 diff。
func TestPreviewBinaryDegrades(t *testing.T) {
	old := []byte{0x89, 'P', 'N', 'G', 0x00, 0x01}
	new := []byte{0x89, 'P', 'N', 'G', 0x00, 0x01, 0x02, 0x03}
	got, err := diffpreview.Preview("logo.png", old, new)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	want := "binary file changed (6 bytes → 8 bytes)"
	if got != want {
		t.Fatalf("二进制降级 = %q, want %q", got, want)
	}
}

// P6：超过 1MiB 降级为摘要（只看大小，不尝试逐行）。
func TestPreviewOversizeDegrades(t *testing.T) {
	old := bytes.Repeat([]byte("a"), diffpreview.MaxBytes+1)
	new := bytes.Repeat([]byte("b"), diffpreview.MaxBytes+2)
	got, err := diffpreview.Preview("big.txt", old, new)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	want := fmt.Sprintf("binary file changed (%d bytes → %d bytes)", len(old), len(new))
	if got != want {
		t.Fatalf("超长降级 = %q, want %q", got, want)
	}

	// 边界：两边都正好 MaxBytes 时仍然生成 diff（不降级）。
	exact := bytes.Repeat([]byte("a\n"), diffpreview.MaxBytes/2)
	if len(exact) != diffpreview.MaxBytes {
		t.Fatalf("构造错误：%d 字节", len(exact))
	}
	modified := append([]byte("x\n"), exact[2:]...)
	if len(modified) != diffpreview.MaxBytes {
		t.Fatalf("构造错误：%d 字节", len(modified))
	}
	got, err = diffpreview.Preview("edge.txt", exact, modified)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if strings.HasPrefix(got, "binary file changed") {
		t.Fatalf("正好 %d 字节不应降级: %q", len(exact), got)
	}
}

// P7：内容相同 → 空 diff（而不是伪造一个空 hunk）。
func TestPreviewIdenticalIsEmpty(t *testing.T) {
	same := []byte("a\nb\nc\n")
	got, err := diffpreview.Preview("f.txt", same, same)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if got != "" {
		t.Fatalf("相同内容应返回空串，得到 %q", got)
	}
	if got, err := diffpreview.Preview("f.txt", nil, nil); err != nil || got != "" {
		t.Fatalf("两边都空: got=%q err=%v", got, err)
	}
}

// P8：无尾换行必须带 `\ No newline at end of file` 标记（git 兼容）。
func TestPreviewNoTrailingNewline(t *testing.T) {
	got, err := diffpreview.Preview("f.txt", []byte("a\nb"), []byte("a\nc"))
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if strings.Count(got, "\\ No newline at end of file") != 2 {
		t.Fatalf("两侧都没有尾换行，应有 2 个标记:\n%s", got)
	}
	golden(t, "no-newline.diff", got)

	// 只在新侧缺尾换行 → 只有 1 个标记。
	got, err = diffpreview.Preview("f.txt", []byte("a\nb\n"), []byte("a\nc"))
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if strings.Count(got, "\\ No newline at end of file") != 1 {
		t.Fatalf("只有新侧缺尾换行，应有 1 个标记:\n%s", got)
	}
}

// P9【核心性质】：把我生成的 diff 反应用一遍，必须逐字节得到新内容。
func TestPreviewRoundTrips(t *testing.T) {
	cases := []struct {
		name     string
		old, new string
	}{
		{"纯插入开头", "b\nc\n", "a\nb\nc\n"},
		{"纯插入中间", "a\nc\n", "a\nb\nc\n"},
		{"纯插入结尾", "a\nb\n", "a\nb\nc\n"},
		{"纯删除开头", "a\nb\nc\n", "b\nc\n"},
		{"纯删除中间", "a\nb\nc\n", "a\nc\n"},
		{"纯删除结尾", "a\nb\nc\n", "a\nb\n"},
		{"整文件替换", "a\nb\n", "x\ny\n"},
		{"从空文件创建", "", "a\nb\n"},
		{"清空文件", "a\nb\n", ""},
		{"重复行", "x\nx\nx\n", "x\nx\n"},
		{"交换两行", "a\nb\n", "b\na\n"},
		{"CRLF 内容", "a\r\nb\r\nc\r\n", "a\r\nB\r\nc\r\n"},
		{"无尾换行到有尾换行", "a\nb", "a\nb\n"},
		{"大量改动", strings.Repeat("same\n", 50) + "old\n" + strings.Repeat("tail\n", 50),
			strings.Repeat("same\n", 50) + "new\n" + strings.Repeat("tail\n", 50)},
		// 超过 MaxEditDistance 的退化路径（600 行全变 → Myers 放弃 → 整段替换）
		// 也必须产生可往返的 diff。
		{"整段替换_退化路径", repeatLines("old", 600), repeatLines("new", 600)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := diffpreview.Preview("f.txt", []byte(tc.old), []byte(tc.new))
			if err != nil {
				t.Fatalf("Preview: %v", err)
			}
			if got == "" {
				if tc.old != tc.new {
					t.Fatalf("内容不同却没有 diff")
				}
				return
			}
			applied, err := applyUnified(tc.old, got)
			if err != nil {
				t.Fatalf("把自己生成的 diff 应用回去失败: %v\n%s", err, got)
			}
			if applied != tc.new {
				t.Fatalf("往返不一致:\n got %q\nwant %q\ndiff:\n%s", applied, tc.new, got)
			}
		})
	}
}

// P10【随机性质】：随机编辑下也必须往返成立（覆盖 hunk 分组、行号边界）。
func TestPreviewRandomizedRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(20261118))
	for iteration := 0; iteration < 200; iteration++ {
		oldLines := randomLines(rng, rng.Intn(30))
		newLines := editLines(rng, oldLines)
		oldText := joinLines(oldLines)
		newText := joinLines(newLines)

		got, err := diffpreview.Preview("f.txt", []byte(oldText), []byte(newText))
		if err != nil {
			t.Fatalf("第 %d 轮 Preview: %v", iteration, err)
		}
		if oldText == newText {
			if got != "" {
				t.Fatalf("第 %d 轮：内容相同却生成了 diff:\n%s", iteration, got)
			}
			continue
		}
		applied, err := applyUnified(oldText, got)
		if err != nil {
			t.Fatalf("第 %d 轮应用失败: %v\n--- old ---\n%q\n--- new ---\n%q\n--- diff ---\n%s",
				iteration, err, oldText, newText, got)
		}
		if applied != newText {
			t.Fatalf("第 %d 轮往返不一致\n--- old ---\n%q\n--- new ---\n%q\n--- got ---\n%q\n--- diff ---\n%s",
				iteration, oldText, newText, applied, got)
		}
	}
}

// P11：输出必须稳定（同输入两次逐字节相同）。
func TestPreviewIsStable(t *testing.T) {
	old := []byte("a\nb\nc\n")
	new := []byte("a\nB\nc\nd\n")
	first, err := diffpreview.Preview("f.txt", old, new)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	for i := 0; i < 5; i++ {
		again, err := diffpreview.Preview("f.txt", old, new)
		if err != nil {
			t.Fatalf("Preview: %v", err)
		}
		if again != first {
			t.Fatalf("同一输入得到不同输出:\n%q\n%q", first, again)
		}
	}
}

// P12：Summary 文案（0 / 1 / n 个文件）。
func TestSummary(t *testing.T) {
	cases := []struct {
		name    string
		changes []diffpreview.Change
		want    string
	}{
		{"没有变更", nil, "no changes"},
		{"一个文件", []diffpreview.Change{
			{Path: "a.txt", Op: diffpreview.OpModify, Old: []byte("a\n"), New: []byte("b\nc\n")},
		}, "1 file changed, +2 -1"},
		{"三个文件", []diffpreview.Change{
			{Path: "a.txt", Op: diffpreview.OpModify, Old: []byte("a\n"), New: []byte("b\nc\n")},
			{Path: "b.txt", Op: diffpreview.OpAdd, New: []byte("x\ny\nz\n")},
			{Path: "c.txt", Op: diffpreview.OpDelete, Old: []byte("q\n")},
		}, "3 files changed, +5 -2"},
		{"使用显式计数", []diffpreview.Change{
			{Path: "a.txt", Op: diffpreview.OpModify, Added: 7, Removed: 3},
		}, "1 file changed, +7 -3"},
		{"二进制不计数", []diffpreview.Change{
			{Path: "logo.png", Op: diffpreview.OpModify, Old: []byte{0}, New: []byte{0, 1}},
		}, "1 file changed, +0 -0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := diffpreview.Summary(tc.changes); got != tc.want {
				t.Fatalf("Summary = %q, want %q", got, tc.want)
			}
		})
	}
}

// P13：Change 级预览带文件头，且与 Preview 的单文件输出一致。
func TestPreviewChangeMatchesPreview(t *testing.T) {
	change := diffpreview.Change{
		Path: "src/a.txt", Op: diffpreview.OpModify,
		Old: []byte("a\nb\n"), New: []byte("a\nc\n"),
	}
	direct, err := diffpreview.Preview("src/a.txt", change.Old, change.New)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	viaChange, err := diffpreview.PreviewChange(change)
	if err != nil {
		t.Fatalf("PreviewChange: %v", err)
	}
	if viaChange != direct {
		t.Fatalf("PreviewChange 与 Preview 不一致:\n%q\n%q", viaChange, direct)
	}
}

// P14：辅助函数（IsBinary / LineCount）的边界。
func TestHelpers(t *testing.T) {
	if diffpreview.IsBinary([]byte("plain text\n")) {
		t.Fatal("纯文本被判成二进制")
	}
	if !diffpreview.IsBinary([]byte("a\x00b")) {
		t.Fatal("含 NUL 应判成二进制")
	}
	cases := map[string]int{
		"":       0,
		"a":      1,
		"a\n":    1,
		"a\nb":   2,
		"a\nb\n": 2,
		"\n":     1,
	}
	for input, want := range cases {
		if got := diffpreview.LineCount([]byte(input)); got != want {
			t.Fatalf("LineCount(%q) = %d, want %d", input, got, want)
		}
	}
}

// --- 测试辅助 -------------------------------------------------------------

// repeatLines 生成 n 行 "<prefix><i>" 的文本。
func repeatLines(prefix string, n int) string {
	var builder strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&builder, "%s-%d\n", prefix, i)
	}
	return builder.String()
}

// randomLines 生成 n 行可重复的文本行。
func randomLines(rng *rand.Rand, n int) []string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("l%d", rng.Intn(5))
	}
	return lines
}

// editLines 在 old 上做随机编辑（插入/删除/替换），返回新行集。
func editLines(rng *rand.Rand, old []string) []string {
	out := append([]string(nil), old...)
	for edits := rng.Intn(4); edits > 0; edits-- {
		switch rng.Intn(3) {
		case 0: // 插入
			at := 0
			if len(out) > 0 {
				at = rng.Intn(len(out) + 1)
			}
			line := fmt.Sprintf("n%d", rng.Intn(5))
			out = append(out[:at], append([]string{line}, out[at:]...)...)
		case 1: // 删除
			if len(out) > 0 {
				at := rng.Intn(len(out))
				out = append(out[:at], out[at+1:]...)
			}
		default: // 替换
			if len(out) > 0 {
				at := rng.Intn(len(out))
				out[at] = fmt.Sprintf("r%d", rng.Intn(5))
			}
		}
	}
	return out
}

// joinLines 把行拼成文本（统一带尾换行）。
func joinLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// applyUnified 是一个只支持本包输出格式的极简 unified diff 应用器：
// 用来验证"生成的 diff 真的能把旧内容变成新内容"。遇到不认识的格式直接报错。
func applyUnified(oldText, diff string) (string, error) {
	oldLines := splitTail(oldText)
	var out []string
	lastNoNewline := false
	pos := 0 // 下一条待消费的 old 行下标
	seenHunk := false
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "--- "), strings.HasPrefix(line, "+++ "):
			continue
		case strings.HasPrefix(line, "@@ "):
			seenHunk = true
			oldStart, _, _, _, err := parseHunkHeader(line)
			if err != nil {
				return "", err
			}
			target := oldStart - 1 // 1 基行号 → 下标
			if target < 0 {
				target = 0
			}
			if target < pos || target > len(oldLines) {
				return "", fmt.Errorf("hunk 起点 %d 不合理（当前 %d，共 %d 行）", oldStart, pos+1, len(oldLines))
			}
			if target > pos {
				out = append(out, oldLines[pos:target]...)
				pos = target
				lastNoNewline = false
			}
		case strings.HasPrefix(line, "\\"):
			// "\ No newline at end of file"：作用于刚输出的那一行。
			if len(out) > 0 {
				lastNoNewline = true
			}
		case strings.HasPrefix(line, " "), strings.HasPrefix(line, "-"), strings.HasPrefix(line, "+"):
			marker := line[0]
			text := line[1:]
			if marker != '+' {
				if pos >= len(oldLines) {
					return "", fmt.Errorf("删除/上下文行超出旧内容（pos=%d）", pos)
				}
				if oldLines[pos] != text {
					return "", fmt.Errorf("第 %d 行不匹配: 期望 %q 实际 %q", pos+1, oldLines[pos], text)
				}
				pos++
			}
			if marker != '-' {
				out = append(out, text)
				lastNoNewline = false
			}
		default:
			return "", fmt.Errorf("无法解析的 diff 行: %q", line)
		}
	}
	if !seenHunk {
		return "", fmt.Errorf("diff 里没有 hunk")
	}
	if pos < len(oldLines) {
		out = append(out, oldLines[pos:]...)
		lastNoNewline = false
	}
	if len(out) == 0 {
		return "", nil
	}
	result := strings.Join(out, "\n")
	if !lastNoNewline {
		result += "\n"
	}
	return result, nil
}

// splitTail 按换行切行（忽略末尾那个空串）。
func splitTail(text string) []string {
	if text == "" {
		return nil
	}
	trimmed := strings.TrimSuffix(text, "\n")
	if trimmed == "" {
		return []string{""}
	}
	return strings.Split(trimmed, "\n")
}

// parseHunkHeader 解析 "@@ -a,b +c,d @@"（",b" 可省略，缺省为 1）。
func parseHunkHeader(line string) (oldStart, oldCount, newStart, newCount int, err error) {
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return 0, 0, 0, 0, fmt.Errorf("hunk 头格式不对: %q", line)
	}
	if _, err := fmt.Sscanf(fields[1], "-%d,%d", &oldStart, &oldCount); err != nil {
		if _, err2 := fmt.Sscanf(fields[1], "-%d", &oldStart); err2 != nil {
			return 0, 0, 0, 0, fmt.Errorf("hunk 旧范围格式不对: %q", fields[1])
		}
		oldCount = 1
	}
	if _, err := fmt.Sscanf(fields[2], "+%d,%d", &newStart, &newCount); err != nil {
		if _, err2 := fmt.Sscanf(fields[2], "+%d", &newStart); err2 != nil {
			return 0, 0, 0, 0, fmt.Errorf("hunk 新范围格式不对: %q", fields[2])
		}
		newCount = 1
	}
	return oldStart, oldCount, newStart, newCount, nil
}
