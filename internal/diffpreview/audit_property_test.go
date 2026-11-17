package diffpreview

// 本文件是审计用的**属性测试**（只新增测试、不改实现）：随机生成 old/new，
// 用"标准库语法 + 独立实现的应用器"（本文件里的 applyUnified，不复用 diffpreview
// 的任何代码）把 diff 应用回 old，要求逐字节等于 new。
//
// 判据：diffpreview 生成的 diff 必须是**可执行**的，而不是"看起来像 diff"。
// 因为审批弹窗把它展示给用户、apply_patch 与 git apply 也要能消费它。

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"testing"
)

// auditLine 是独立应用器自己的一行（与 diffpreview 的内部类型无关）。
type auditLine struct {
	text  string
	hadNL bool
}

// splitAudit 按换行切行，**保留行内的 \r**（CRLF 文件的 \r 属于内容）。
func splitAudit(content []byte) []auditLine {
	if len(content) == 0 {
		return nil
	}
	var out []auditLine
	start := 0
	for index := 0; index < len(content); index++ {
		if content[index] != '\n' {
			continue
		}
		out = append(out, auditLine{text: string(content[start:index]), hadNL: true})
		start = index + 1
	}
	if start < len(content) {
		out = append(out, auditLine{text: string(content[start:]), hadNL: false})
	}
	return out
}

// parseHunkStart 解析 hunk 头里某一侧的起始行号并换算成 0 基下标。
//
// 统一 diff 的记法：`-1,2` 表示从第 1 行起 2 行；`-1` 等价 `-1,1`；
// `-0,0`（新增文件）表示 0 基的 0。因此 count==0 时起始值本身就是 0 基下标。
func parseHunkStart(t *testing.T, header, sign string) int {
	t.Helper()
	marker := strings.Index(header, sign)
	if marker < 0 {
		t.Fatalf("hunk 头缺少 %q: %q", sign, header)
	}
	rest := header[marker+1:]
	end := strings.IndexAny(rest, " @") // 注意：逗号属于 spec 本身
	if end < 0 {
		t.Fatalf("无法解析 hunk 头: %q", header)
	}
	spec := rest[:end]
	startText, countText := spec, ""
	if comma := strings.IndexByte(spec, ','); comma >= 0 {
		startText, countText = spec[:comma], spec[comma+1:]
	}
	var start, count int
	if _, err := fmt.Sscanf(startText, "%d", &start); err != nil {
		t.Fatalf("hunk 头起始行号无法解析: %q", header)
	}
	if countText != "" {
		if _, err := fmt.Sscanf(countText, "%d", &count); err != nil {
			t.Fatalf("hunk 头行数无法解析: %q", header)
		}
	} else {
		count = 1
	}
	if count == 0 {
		return start
	}
	return start - 1
}

// applyUnified 是一个**独立实现**的统一 diff 应用器：把 diff 应用到 old 上重建 new。
//
// 它刻意不复用 diffpreview 的任何函数（否则就是自己验证自己），并且必须处理
// "hunk 之外的内容不在 diff 里"这一点——只凭 diff 正文是重建不出完整文件的。
func applyUnified(t *testing.T, old []byte, diff string) []byte {
	t.Helper()
	if strings.TrimSpace(diff) == "" {
		return old
	}
	oldLines := splitAudit(old)
	lines := strings.Split(diff, "\n")

	var out []auditLine
	oldIdx := 0
	index := 0
	for index < len(lines) && !strings.HasPrefix(lines[index], "@@") {
		index++ // 跳过 ---/+++ 头部
	}
	for index < len(lines) {
		header := lines[index]
		index++
		oldStart := parseHunkStart(t, header, "-")
		if oldStart < oldIdx {
			t.Fatalf("hunk 起始行倒退: %d < %d (%q)", oldStart, oldIdx, header)
		}
		out = append(out, oldLines[oldIdx:oldStart]...)
		oldIdx = oldStart

		for index < len(lines) && !strings.HasPrefix(lines[index], "@@") {
			raw := lines[index]
			if raw == "" {
				index++
				continue
			}
			noNewline := index+1 < len(lines) && strings.HasPrefix(lines[index+1], `\ No newline`)
			switch raw[0] {
			case ' ':
				if oldIdx >= len(oldLines) || oldLines[oldIdx].text != raw[1:] {
					t.Fatalf("上下文行与旧内容不匹配: diff=%q old[%d]=%q", raw, oldIdx, safeLine(oldLines, oldIdx))
				}
				out = append(out, auditLine{text: raw[1:], hadNL: !noNewline})
				oldIdx++
			case '-':
				if oldIdx >= len(oldLines) || oldLines[oldIdx].text != raw[1:] {
					t.Fatalf("删除行与旧内容不匹配: diff=%q old[%d]=%q", raw, oldIdx, safeLine(oldLines, oldIdx))
				}
				oldIdx++
			case '+':
				out = append(out, auditLine{text: raw[1:], hadNL: !noNewline})
			default:
				t.Fatalf("diff 里有无法识别的行: %q", raw)
			}
			if noNewline {
				index++
			}
			index++
		}
	}
	out = append(out, oldLines[oldIdx:]...)

	var buf []byte
	for _, line := range out {
		buf = append(buf, line.text...)
		if line.hadNL {
			buf = append(buf, '\n')
		}
	}
	return buf
}

// safeLine 取一行用于错误信息（越界时返回占位）。
func safeLine(lines []auditLine, index int) string {
	if index < 0 || index >= len(lines) {
		return "<越界>"
	}
	return lines[index].text
}

// countDiffLines 独立统计 diff 里的增删行数（不含头部与标记行）。
func countDiffLines(diff string) (added, removed int) {
	inBody := false
	for _, raw := range strings.Split(diff, "\n") {
		if strings.HasPrefix(raw, "@@") {
			inBody = true
			continue
		}
		if !inBody || raw == "" || strings.HasPrefix(raw, `\ No newline`) {
			continue
		}
		switch raw[0] {
		case '+':
			added++
		case '-':
			removed++
		}
	}
	return added, removed
}

// linePool 是生成随机内容用的行池（含空行与需要转义的边界字符）。
var linePool = []string{
	"a", "b", "c", "package main", "func main() {", "}", "",
	"\tindented", "trailing space ", "unicode 中文", "+looks-like-diff", "-also", "@@ fake hunk",
}

// randomContent 生成一段随机内容；eol 决定行尾（"\n" 或 "\r\n"）。
func randomContent(rng *rand.Rand, eol string, maxLines int) []byte {
	lines := rng.Intn(maxLines + 1)
	var builder strings.Builder
	for index := 0; index < lines; index++ {
		builder.WriteString(linePool[rng.Intn(len(linePool))])
		builder.WriteString(eol)
	}
	content := builder.String()
	// 30% 概率去掉尾换行（"no newline at end of file" 路径）。
	if content != "" && rng.Intn(10) < 3 {
		content = strings.TrimSuffix(strings.TrimSuffix(content, eol), "\n")
		content = strings.TrimSuffix(content, "\r")
	}
	return []byte(content)
}

// mutateContent 在内容上随机做若干处编辑（插/删/改），产出一份"看起来像人改的"新内容。
func mutateContent(rng *rand.Rand, content []byte, eol string) []byte {
	lines := splitLinesForAudit(content)
	edits := rng.Intn(6)
	for index := 0; index < edits; index++ {
		switch rng.Intn(3) {
		case 0: // 插入
			at := rng.Intn(len(lines) + 1)
			inserted := linePool[rng.Intn(len(linePool))]
			lines = append(lines[:at], append([]string{inserted}, lines[at:]...)...)
		case 1: // 删除
			if len(lines) > 0 {
				at := rng.Intn(len(lines))
				lines = append(lines[:at], lines[at+1:]...)
			}
		case 2: // 修改
			if len(lines) > 0 {
				at := rng.Intn(len(lines))
				lines[at] = linePool[rng.Intn(len(linePool))]
			}
		}
	}
	joined := strings.Join(lines, eol)
	if joined == "" {
		return nil
	}
	return []byte(joined + eol)
}

// splitLinesForAudit 按换行切行（去掉行尾的 \r 与 \n），只用于造数据。
func splitLinesForAudit(content []byte) []string {
	text := strings.ReplaceAll(string(content), "\r\n", "\n")
	if text == "" {
		return nil
	}
	text = strings.TrimSuffix(text, "\n")
	if text == "" {
		return []string{""}
	}
	return strings.Split(text, "\n")
}

// TestAuditProperty_DiffAppliesBackToNew 是核心属性：500 轮随机 diff → 独立应用器
// 应用回去 → 逐字节等于新内容。
func TestAuditProperty_DiffAppliesBackToNew(t *testing.T) {
	rng := rand.New(rand.NewSource(0x5EED))
	eols := []string{"\n", "\r\n"}
	rounds := 500
	// 需要更大规模的临时烟测时：AUDIT_DIFF_ROUNDS=5000 go test ./internal/diffpreview -run TestAuditProperty
	if raw := os.Getenv("AUDIT_DIFF_ROUNDS"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			rounds = parsed
		}
	}
	for round := 0; round < rounds; round++ {
		eol := eols[rng.Intn(len(eols))]
		old := randomContent(rng, eol, 20)
		var new []byte
		switch rng.Intn(4) {
		case 0:
			new = mutateContent(rng, old, eol)
		case 1:
			new = nil // 变成空文件
		case 2:
			new = []byte(linePool[rng.Intn(len(linePool))] + eol) // 变成单行
		default:
			new = randomContent(rng, eol, 20) // 完全无关的新内容
		}
		if bytes.Equal(old, new) {
			continue
		}

		diff, err := Preview("src/a.txt", old, new)
		if err != nil {
			t.Fatalf("轮 %d: Preview: %v (old=%q new=%q)", round, err, old, new)
		}
		if diff == "" {
			t.Fatalf("轮 %d: old != new 但 diff 为空 (old=%q new=%q)", round, old, new)
		}
		if strings.Contains(diff, "binary file changed") {
			t.Fatalf("轮 %d: 纯文本被当成二进制: %q / %q", round, old, new)
		}

		got := applyUnified(t, old, diff)
		if !bytes.Equal(got, new) {
			t.Fatalf("轮 %d: 应用 diff 后不等于新内容\n old  = %q\n new  = %q\n got  = %q\n diff =\n%s",
				round, old, new, got, diff)
		}

		// Stats 必须与 diff 里的增删行数一致（Summary 的 +N/-M 就靠它）。
		wantAdded, wantRemoved := countDiffLines(diff)
		added, removed := Stats(Change{Path: "src/a.txt", Op: OpModify, Old: old, New: new})
		if added != wantAdded || removed != wantRemoved {
			t.Fatalf("轮 %d: Stats() = +%d -%d, diff 里是 +%d -%d", round, added, removed, wantAdded, wantRemoved)
		}

		// 应用后的行数关系自洽：newLines = oldLines - removed + added。
		if gotLines, wantLines := LineCount(new), LineCount(old)-removed+added; gotLines != wantLines {
			t.Fatalf("轮 %d: 行数关系不自洽: LineCount(new)=%d vs old(%d)-%d+%d=%d",
				round, gotLines, LineCount(old), removed, added, wantLines)
		}
	}
}

// TestAuditEdge_EmptyAndNoTrailingNewline 覆盖空文件/无尾换行/CRLF/超长单行这四类
// 最容易出错的边界（也是用户在个人仓库里最常见的形态）。
func TestAuditEdge_EmptyAndNoTrailingNewline(t *testing.T) {
	cases := []struct {
		name string
		old  []byte
		new  []byte
	}{
		{"两边都空", nil, nil},
		{"空 → 有内容", nil, []byte("a\n")},
		{"有内容 → 空", []byte("a\n"), nil},
		{"无尾换行 → 有尾换行", []byte("a\nb"), []byte("a\nb\n")},
		{"有尾换行 → 无尾换行", []byte("a\nb\n"), []byte("a\nb")},
		{"纯空行文件", []byte("\n"), []byte("\n\n")},
		{"CRLF 未变", []byte("a\r\nb\r\n"), []byte("a\r\nb\r\n")},
		{"CRLF → LF", []byte("a\r\nb\r\n"), []byte("a\nb\n")},
		{"单行无尾换行改内容", []byte("x"), []byte("y")},
		{"超长单行", append(bytes.Repeat([]byte("z"), 100<<10), '\n'), []byte("short\n")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			diff, err := Preview("f.txt", tc.old, tc.new)
			if err != nil {
				t.Fatalf("Preview: %v", err)
			}
			if bytes.Equal(tc.old, tc.new) {
				if diff != "" {
					t.Fatalf("内容相同却产生了 diff: %q", diff)
				}
				return
			}
			if diff == "" {
				t.Fatalf("内容不同却没有 diff")
			}
			if got := applyUnified(t, tc.old, diff); !bytes.Equal(got, tc.new) {
				t.Fatalf("应用后不匹配:\n new = %q\n got = %q\n diff = %q", tc.new, got, diff)
			}
			// 二进制/超大上限内仍走 diff；NUL 才走摘要。
			if IsBinary(tc.old) || IsBinary(tc.new) {
				if !strings.Contains(diff, "binary file changed") {
					t.Fatalf("含 NUL 却没走二进制摘要")
				}
			}
		})
	}
}

// TestAuditEdge_BinaryAndOversize 覆盖二进制与超 MaxBytes 的降级（不能尝试逐行比较）。
func TestAuditEdge_BinaryAndOversize(t *testing.T) {
	binaryOld := []byte("a\x00b")
	diff, err := Preview("bin.dat", binaryOld, []byte("c\x00d"))
	if err != nil {
		t.Fatalf("Preview(binary): %v", err)
	}
	if !strings.Contains(diff, "binary file changed") {
		t.Fatalf("二进制没有降级成摘要: %q", diff)
	}
	if strings.Contains(diff, "@@") {
		t.Fatalf("二进制不该产生 hunk: %q", diff)
	}

	huge := bytes.Repeat([]byte("x"), MaxBytes+1)
	diff, err = Preview("huge.txt", huge, []byte("small"))
	if err != nil {
		t.Fatalf("Preview(huge): %v", err)
	}
	if !strings.Contains(diff, "binary file changed") {
		t.Fatalf("超 MaxBytes 没有降级成摘要: %q", diff)
	}
	if added, removed := Stats(Change{Old: huge, New: []byte("small")}); added != 0 || removed != 0 {
		t.Fatalf("超限文件的 Stats = +%d -%d, want 0/0（与 git 的 Binary files differ 一致）", added, removed)
	}
}

// TestAuditSummaryCounts 覆盖 Summary 的汇总口径。
func TestAuditSummaryCounts(t *testing.T) {
	changes := []Change{
		{Path: "a.txt", Op: OpModify, Old: []byte("1\n2\n3\n"), New: []byte("1\n2\n4\n")},
		{Path: "b.txt", Op: OpAdd, New: []byte("hello\nworld\n")},
	}
	summary := Summary(changes)
	// a: +1 -1；b: +2 -0 → 合计 +3 -1
	if !strings.Contains(summary, "+3 -1") {
		t.Fatalf("Summary = %q, want 含 +3 -1", summary)
	}
	if !strings.Contains(summary, "2 files changed") {
		t.Fatalf("Summary = %q, want '2 files changed'", summary)
	}
	if got := Summary(nil); got != "no changes" {
		t.Fatalf("空变更 Summary = %q", got)
	}
}

// TestAuditPreviewIsDeterministic 同一输入必须产生同一字节序列（golden 的前提）。
func TestAuditPreviewIsDeterministic(t *testing.T) {
	old := []byte("a\nb\nc\nd\ne\nf\ng\n")
	new := []byte("a\nB\nc\nd\ne\nf\nG\nh\n")
	first, err := Preview("p.txt", old, new)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	for round := 0; round < 50; round++ {
		again, err := Preview("p.txt", old, new)
		if err != nil {
			t.Fatalf("Preview: %v", err)
		}
		if again != first {
			t.Fatalf("第 %d 轮输出不一致", round)
		}
	}
}

// 防止 fmt 未使用（失败信息里用）。
var _ = fmt.Sprintf

// TestAuditStatsMatchPreviewOnLargeRewrite：退化路径（中段超过编辑距离上限）下，
// Stats 必须仍然与 Preview 产出的 diff 行数**完全一致**——这是"摘要不说谎"的底线。
func TestAuditStatsMatchPreviewOnLargeRewrite(t *testing.T) {
	var oldBuilder, newBuilder strings.Builder
	for index := 0; index < 6000; index++ {
		fmt.Fprintf(&oldBuilder, "old line %05d\n", index)
		fmt.Fprintf(&newBuilder, "NEW line %05d\n", index)
	}
	oldContent := []byte(oldBuilder.String())
	newContent := []byte(newBuilder.String())

	diff, err := Preview("big.txt", oldContent, newContent)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	wantAdded, wantRemoved := countDiffLines(diff)
	added, removed := Stats(Change{Path: "big.txt", Op: OpModify, Old: oldContent, New: newContent})
	if added != wantAdded || removed != wantRemoved {
		t.Fatalf("退化路径下 Stats(+%d -%d) != diff(+%d -%d)", added, removed, wantAdded, wantRemoved)
	}
	if added != 6000 || removed != 6000 {
		t.Fatalf("整篇重写的统计 = +%d -%d, want +6000 -6000", added, removed)
	}
	// 应用回去仍必须逐字节正确（退化路径生成的 diff 也得是合法且可应用的）。
	if got := applyUnified(t, oldContent, diff); !bytes.Equal(got, newContent) {
		t.Fatal("退化路径的 diff 应用后不等于新内容")
	}
}

// TestAuditStatsMatchPreviewNearThreshold：中段刚好在上限附近（不能因为快路径的
// 阈值判断而与 Preview 分叉）。
func TestAuditStatsMatchPreviewNearThreshold(t *testing.T) {
	for _, lines := range []int{4090, 4096, 4100, 8200} {
		var oldBuilder, newBuilder strings.Builder
		for index := 0; index < lines; index++ {
			fmt.Fprintf(&oldBuilder, "a%05d\n", index)
			fmt.Fprintf(&newBuilder, "b%05d\n", index)
		}
		oldContent := []byte(oldBuilder.String())
		newContent := []byte(newBuilder.String())
		diff, err := Preview("p.txt", oldContent, newContent)
		if err != nil {
			t.Fatalf("%d 行 Preview: %v", lines, err)
		}
		wantAdded, wantRemoved := countDiffLines(diff)
		added, removed := Stats(Change{Old: oldContent, New: newContent})
		if added != wantAdded || removed != wantRemoved {
			t.Fatalf("%d 行: Stats(+%d -%d) != diff(+%d -%d)", lines, added, removed, wantAdded, wantRemoved)
		}
	}
}
