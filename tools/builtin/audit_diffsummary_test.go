package builtin

// 本文件是审计用例（只新增测试、不改实现）：write_file / edit_file 结果里的
// 一行摘要是否"说人话"——尤其"内容没变"时会不会给出误导性的 +0 -0——以及
// 1 MiB 文件上 200 次的耗时（每条写文件都花 10ms 是不可接受的）。

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/internal/diffpreview"
)

// TestAuditWriteSummaryNewFile 覆盖"新建"。
func TestAuditWriteSummaryNewFile(t *testing.T) {
	got := writeFileSummary("src/a.txt", nil, 0, false, true, []byte("a\nb\nc\n"))
	if want := "write_file: src/a.txt (created, 3 lines)"; got != want {
		t.Fatalf("新建摘要 = %q, want %q", got, want)
	}
	// 空文件也要说清楚（不能是 "created, " 这种断尾）。
	empty := writeFileSummary("empty.txt", nil, 0, false, true, nil)
	if !strings.Contains(empty, "created") || !strings.Contains(empty, "0 lines") {
		t.Fatalf("空文件新建摘要 = %q", empty)
	}
}

// TestAuditWriteSummaryOverwrite 覆盖"覆盖已有文件"且有内容变化。
func TestAuditWriteSummaryOverwrite(t *testing.T) {
	old := []byte("a\nb\nc\n")
	new := []byte("a\nB\nc\nd\n")
	got := writeFileSummary("src/a.txt", old, int64(len(old)), true, true, new)
	if want := "write_file: src/a.txt (3 lines → 4 lines, +2 -1)"; got != want {
		t.Fatalf("覆盖摘要 = %q, want %q", got, want)
	}
}

// TestAuditWriteSummaryUnchanged 覆盖"内容相同（0 变化）"：必须明确说 unchanged，
// 而不是让模型去自行推理 `+0 -0`（修复前是 `+0 -0`，见审计报告 §5 / §7-F6）。
func TestAuditWriteSummaryUnchanged(t *testing.T) {
	content := []byte("a\nb\nc\n")
	got := writeFileSummary("src/a.txt", content, int64(len(content)), true, true, content)
	if want := "write_file: src/a.txt (unchanged, 3 lines)"; got != want {
		t.Fatalf("内容相同时摘要 = %q, want %q", got, want)
	}
	if strings.Contains(got, "+0") || strings.Contains(got, "-0") {
		t.Fatalf("不该再出现 +0 -0: %q", got)
	}
	// 空文件也要能说清楚。
	empty := writeFileSummary("empty.txt", nil, 0, true, true, nil)
	if !strings.Contains(empty, "unchanged") || !strings.Contains(empty, "0 lines") {
		t.Fatalf("空文件 unchanged 摘要 = %q", empty)
	}
}

// TestAuditEditSummaryUnchanged 覆盖 edit_file 的"替换前后完全一致"（例如把 A 换成 A）。
func TestAuditEditSummaryUnchanged(t *testing.T) {
	content := []byte("a\nb\n")
	got := editFileSummary("src/a.txt", content, content, 1)
	if want := "edit_file: src/a.txt (unchanged, 2 lines)"; got != want {
		t.Fatalf("edit 内容相同时摘要 = %q, want %q", got, want)
	}
}

// TestAuditWriteSummaryBinary 覆盖"二进制/超限"降级成字节数。
func TestAuditWriteSummaryBinary(t *testing.T) {
	old := []byte("a\x00b")
	new := []byte("a\x00c")
	got := writeFileSummary("bin.dat", old, int64(len(old)), true, true, new)
	if want := "write_file: bin.dat (3 bytes → 3 bytes)"; got != want {
		t.Fatalf("二进制摘要 = %q, want %q", got, want)
	}
	if strings.Contains(got, "+") || strings.Contains(got, "-") {
		t.Fatalf("二进制摘要不该出现行数: %q", got)
	}

	// 超过 MaxBytes：即使不是二进制也只报字节数（避免审批时卡住）。
	huge := make([]byte, diffpreview.MaxBytes+1)
	for index := range huge {
		huge[index] = 'x'
	}
	got = writeFileSummary("huge.txt", huge, int64(len(huge)), true, true, []byte("small\n"))
	if !strings.Contains(got, "bytes →") {
		t.Fatalf("超 MaxBytes 应降级为字节数: %q", got)
	}
}

// TestAuditWriteSummaryOldNotKnown 覆盖"旧内容没读进内存"（文件过大）的降级。
func TestAuditWriteSummaryOldNotKnown(t *testing.T) {
	got := writeFileSummary("big.json", nil, 5<<20, true, false, []byte("{}\n"))
	if !strings.Contains(got, "5242880 bytes → 3 bytes") {
		t.Fatalf("旧内容未知时应报字节数: %q", got)
	}
}

// TestAuditEditSummaryReplaceAll 覆盖 edit_file 的 replace_all（多次替换）。
func TestAuditEditSummaryReplaceAll(t *testing.T) {
	old := []byte("foo\nfoo\nfoo\n")
	new := []byte("bar\nbar\nbar\n")
	got := editFileSummary("src/a.txt", old, new, 3)
	if want := "edit_file: src/a.txt (replace 3 occurrences, +3 -3)"; got != want {
		t.Fatalf("replace_all 摘要 = %q, want %q", got, want)
	}
	// 单次替换的复数形态。
	one := editFileSummary("src/a.txt", []byte("foo\n"), []byte("bar\n"), 1)
	if !strings.Contains(one, "replace 1 occurrence,") {
		t.Fatalf("单次替换摘要 = %q", one)
	}
	// 二进制文件的 edit：走字节数，且仍然说明替换次数。
	bin := editFileSummary("bin.dat", []byte("a\x00b"), []byte("a\x00c"), 1)
	if !strings.Contains(bin, "replace 1 occurrence") || !strings.Contains(bin, "bytes →") {
		t.Fatalf("二进制 edit 摘要 = %q", bin)
	}
}

// TestAuditSummaryPerformance 覆盖"每条写文件都花 10ms"的性能风险：
// 1 MiB 文件 × 200 次，分别测"只改一行"（常见）与"整篇重写"（最坏）。
func TestAuditSummaryPerformance(t *testing.T) {
	if raceEnabled {
		t.Skip("-race 构建下性能断言不成立（插桩会让摘要慢 5~20 倍）")
	}
	// 精确造到"刚好不到 MaxBytes"的 1 MiB 级内容（超过上限会走二进制降级，测不到 Myers）。
	var builder strings.Builder
	for index := 0; builder.Len() < diffpreview.MaxBytes-200; index++ {
		fmt.Fprintf(&builder, "line %06d: the quick brown fox jumps over the lazy dog\n", index)
	}
	old := []byte(builder.String())
	if len(old) > diffpreview.MaxBytes {
		t.Fatalf("构造的内容超过 MaxBytes: %d", len(old))
	}
	t.Logf("基线内容 %d 字节（MaxBytes=%d）", len(old), diffpreview.MaxBytes)
	// 只改一行（编辑距离很小）。
	oneChange := append([]byte(nil), old...)
	copy(oneChange, []byte("LINE 000001: the quick brown fox jumps over the lazy dog\n"))
	// 整篇重写（编辑距离巨大 → 应当走"整段替换"退化路径）。
	rewritten := []byte(strings.ReplaceAll(string(old), "quick", "slow"))

	const iterations = 200
	for _, tc := range []struct {
		name string
		new  []byte
	}{
		{"改一行", oneChange},
		{"整篇重写", rewritten},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			var summary string
			for index := 0; index < iterations; index++ {
				summary = writeFileSummary("big.txt", old, int64(len(old)), true, true, tc.new)
			}
			elapsed := time.Since(start)
			perCall := elapsed / iterations
			t.Logf("%s：%d 次共 %v，单次 %v，摘要 %q", tc.name, iterations, elapsed, perCall, summary)
			// 修复目标：1 MiB 上单次摘要 ≤5ms（修复前"改一行"是 14.7ms、"整篇重写"21.7ms）。
			if perCall > 5*time.Millisecond {
				t.Fatalf("%s：单次 %v 超过 5ms 目标", tc.name, perCall)
			}
		})
	}
}

// TestAuditSummaryLineCountWords 覆盖 "0/1/N lines" 的复数形态（模型会读这句话）。
func TestAuditSummaryLineCountWords(t *testing.T) {
	cases := map[string]int{
		"0 lines": 0,
		"1 line":  1,
		"2 lines": 2,
	}
	for want, count := range cases {
		content := make([]byte, 0, count*2)
		for index := 0; index < count; index++ {
			content = append(content, 'a', '\n')
		}
		if got := lineCountWords(content); got != want {
			t.Fatalf("lineCountWords(%d 行) = %q, want %q", count, got, want)
		}
	}
}

// TestAuditPerformance_SizeCurve 记录耗时随文件大小的变化，用来给"从哪里开始变慢"
// 一个可引用的曲线（都是 1 MiB/200 次之外的补充数据）。
func TestAuditPerformance_SizeCurve(t *testing.T) {
	if raceEnabled {
		t.Skip("-race 构建下尺寸曲线没意义（插桩开销主导）")
	}
	sizes := []int{64 << 10, 256 << 10, 512 << 10, 1 << 20}
	for _, size := range sizes {
		var builder strings.Builder
		for index := 0; builder.Len() < size-64; index++ {
			fmt.Fprintf(&builder, "line %06d: the quick brown fox jumps over the lazy dog\n", index)
		}
		old := []byte(builder.String())
		new := append([]byte(nil), old...)
		copy(new, []byte("LINE 000001: the quick brown fox jumps over the lazy dog\n"))

		const iterations = 20
		start := time.Now()
		for index := 0; index < iterations; index++ {
			_ = writeFileSummary("big.txt", old, int64(len(old)), true, true, new)
		}
		perCall := time.Since(start) / iterations
		t.Logf("%7d 字节（%5d 行）→ 单次 %v", len(old), diffpreview.LineCount(old), perCall)
	}
}
