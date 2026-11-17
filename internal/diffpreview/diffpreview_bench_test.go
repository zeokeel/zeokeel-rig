package diffpreview_test

// 预览的性能基准：审批弹窗必须"立刻"出现，所以这里钉住 1 MiB 量级的耗时。

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/internal/diffpreview"
)

// oneMiB 生成约 1 MiB 的文本（每行 40 字节）。
func oneMiB() []byte {
	line := "0123456789abcdefghijklmnopqrstuvwxyzABCD\n"
	repeats := diffpreview.MaxBytes / len(line)
	return []byte(strings.Repeat(line, repeats))
}

// BenchmarkPreview1MiBSmallChange 是典型场景：1 MiB 文件里改 10 行。
func BenchmarkPreview1MiBSmallChange(b *testing.B) {
	old := oneMiB()
	lines := strings.Split(string(old), "\n")
	for i := 100; i < 110; i++ {
		lines[i] = "CHANGED " + lines[i]
	}
	updated := []byte(strings.Join(lines, "\n"))

	b.SetBytes(int64(len(old)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := diffpreview.Preview("big.txt", old, updated); err != nil {
			b.Fatalf("Preview: %v", err)
		}
	}
}

// BenchmarkPreview1MiBFullRewrite 是最坏情况：整个文件都不一样（退化路径）。
func BenchmarkPreview1MiBFullRewrite(b *testing.B) {
	old := oneMiB()
	updated := bytes.Repeat([]byte("x"), len(old))

	b.SetBytes(int64(len(old)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := diffpreview.Preview("big.txt", old, updated); err != nil {
			b.Fatalf("Preview: %v", err)
		}
	}
}

// BenchmarkPreviewScatteredEdits 是"改动散布在两端"的病态形状：前后缀裁剪帮不上忙。
func BenchmarkPreviewScatteredEdits(b *testing.B) {
	old := oneMiB()
	lines := strings.Split(string(old), "\n")
	for i := 5; i < len(lines)-5; i += 500 {
		lines[i] = fmt.Sprintf("EDIT-%d", i)
	}
	updated := []byte(strings.Join(lines, "\n"))

	b.SetBytes(int64(len(old)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := diffpreview.Preview("big.txt", old, updated); err != nil {
			b.Fatalf("Preview: %v", err)
		}
	}
}

// BenchmarkSummary100Changes 是 apply_patch 多文件场景的汇总开销。
func BenchmarkSummary100Changes(b *testing.B) {
	changes := make([]diffpreview.Change, 0, 100)
	for i := 0; i < 100; i++ {
		changes = append(changes, diffpreview.Change{
			Path: fmt.Sprintf("f%d.txt", i),
			Op:   diffpreview.OpModify,
			Old:  []byte("a\nb\nc\n"),
			New:  []byte("a\nB\nc\nd\n"),
		})
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := diffpreview.Summary(changes); !strings.Contains(got, "100 files changed") {
			b.Fatalf("Summary = %q", got)
		}
	}
}

// BenchmarkStats1MiB 是"写文件摘要"的热路径：1 MiB、只改一行。
//
// 修复前（逐行 string 物化 + 整份切两侧）约 14.7ms/op；修复后（字节级剥前后缀 +
// []byte 行切片）降到 ~2ms/op。这条基准用来防止回退。
func BenchmarkStats1MiB(b *testing.B) {
	var builder bytes.Buffer
	for index := 0; builder.Len() < diffpreview.MaxBytes-200; index++ {
		fmt.Fprintf(&builder, "line %06d: the quick brown fox jumps over the lazy dog\n", index)
	}
	old := builder.Bytes()
	new := append([]byte(nil), old...)
	copy(new, []byte("LINE 000001: the quick brown fox jumps over the lazy dog\n"))
	change := diffpreview.Change{Path: "big.txt", Op: diffpreview.OpModify, Old: old, New: new}

	b.SetBytes(int64(len(old)))
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if added, removed := diffpreview.Stats(change); added != 1 || removed != 1 {
			b.Fatalf("Stats = +%d -%d, want +1 -1", added, removed)
		}
	}
}
