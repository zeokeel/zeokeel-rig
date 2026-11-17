package search

// 本文件是性能基准：构造 2000 个小文件，测量一次 BM25 内容检索（含索引构建）的
// 耗时，并报告 IndexFiles/sec。它不进默认测试路径（go test 不会跑 Benchmark*）。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
)

// benchmarkCorpus 在临时目录里构造 count 个文件，每个文件 lines 行、含固定词表。
func benchmarkCorpus(b *testing.B, count, lines int) string {
	b.Helper()
	root := b.TempDir()
	for index := 0; index < count; index++ {
		dir := filepath.Join(root, "pkg", fmt.Sprintf("mod%02d", index%20))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			b.Fatalf("mkdir: %v", err)
		}
		var builder strings.Builder
		for line := 0; line < lines; line++ {
			// token 按文件下标取模，保证查询词能命中几百个文档（BM25 才有排序可做）。
			fmt.Fprintf(&builder, "func helper%d_%d(ctx context.Context) error { return nil } // searchable token%d\n", index, line, index%7)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("file%04d.go", index)), []byte(builder.String()), 0o644); err != nil {
			b.Fatalf("write: %v", err)
		}
	}
	return root
}

func BenchmarkTextQuery2000Files(b *testing.B) {
	const count = 2000
	root := benchmarkCorpus(b, count, 40)
	tool := New(root, WithMaxResults(20))
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		result, err := tool.Invoke(context.Background(), toolCallFor(`{"query":"searchable token3","mode":"text"}`))
		if err != nil {
			b.Fatalf("Invoke: %v", err)
		}
		if result.IsError {
			b.Fatalf("tool error: %s", result.Content)
		}
		if result.Content == "" {
			b.Fatal("no matches in the benchmark corpus")
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(b.N)*count/elapsedSeconds(b), "IndexFiles/sec")
}

func BenchmarkFilesQuery2000Files(b *testing.B) {
	const count = 2000
	root := benchmarkCorpus(b, count, 5)
	tool := New(root, WithMaxResults(20))
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		result, err := tool.Invoke(context.Background(), toolCallFor(`{"query":"file17","mode":"files"}`))
		if err != nil {
			b.Fatalf("Invoke: %v", err)
		}
		if result.IsError {
			b.Fatalf("tool error: %s", result.Content)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(b.N)*count/elapsedSeconds(b), "ScanFiles/sec")
}

// toolCallFor 构造一次基准用的工具调用。
func toolCallFor(arguments string) rig.ToolCall {
	return rig.ToolCall{Name: ToolName, Arguments: arguments}
}

// elapsedSeconds 返回本次基准的墙钟秒数（至少 1ns，避免除零）。
func elapsedSeconds(b *testing.B) float64 {
	elapsed := b.Elapsed()
	if elapsed <= 0 {
		elapsed = 1
	}
	return elapsed.Seconds()
}
