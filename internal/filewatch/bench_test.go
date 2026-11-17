package filewatch

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// 本文件是扫描成本的基准：轮询方案的关键指标是"一次全量扫描要多久"，
// 因为它直接决定 Interval 能开多小。
//
//	go test ./internal/filewatch/ -bench . -benchmem -run '^$'

// buildBenchTree 在临时目录里铺 files 个文件（10 个目录 × files/10 个文件）。
func buildBenchTree(b *testing.B, files int) string {
	b.Helper()
	root := b.TempDir()
	const dirs = 10
	for d := 0; d < dirs; d++ {
		dir := filepath.Join(root, fmt.Sprintf("pkg%02d", d))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			b.Fatalf("建目录: %v", err)
		}
		for i := 0; i < files/dirs; i++ {
			name := filepath.Join(dir, fmt.Sprintf("file%04d.go", i))
			if err := os.WriteFile(name, []byte("package p\n"), 0o644); err != nil {
				b.Fatalf("写文件: %v", err)
			}
		}
	}
	return root
}

// BenchmarkScan10k 测 1 万文件的一次扫描（不带摘要）。
func BenchmarkScan10k(b *testing.B) {
	root := buildBenchTree(b, 10000)
	s := &scanner{root: root, ignore: defaultIgnore(), maxFiles: DefaultMaxFiles}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		files, truncated, err := s.scan()
		if err != nil {
			b.Fatalf("scan: %v", err)
		}
		if truncated || len(files) != 10000 {
			b.Fatalf("scan 结果异常: truncated=%v files=%d", truncated, len(files))
		}
	}
}

// BenchmarkScan10kDigest 测 1 万文件的扫描 + sha256（每个文件 10 字节）。
func BenchmarkScan10kDigest(b *testing.B) {
	root := buildBenchTree(b, 10000)
	s := &scanner{root: root, ignore: defaultIgnore(), maxFiles: DefaultMaxFiles, digest: true}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := s.scan(); err != nil {
			b.Fatalf("scan: %v", err)
		}
	}
}

// BenchmarkDiff10k 测"无变化"的差异比较（每个轮询周期都要做一次）。
func BenchmarkDiff10k(b *testing.B) {
	root := buildBenchTree(b, 10000)
	s := &scanner{root: root, ignore: defaultIgnore(), maxFiles: DefaultMaxFiles}
	cur, _, err := s.scan()
	if err != nil {
		b.Fatalf("scan: %v", err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if pending := diffPaths(cur, cur, false); len(pending) != 0 {
			b.Fatalf("无变化却产生了 %d 条 pending", len(pending))
		}
	}
}

// BenchmarkScan1k 是小型仓库的参照点。
func BenchmarkScan1k(b *testing.B) {
	root := buildBenchTree(b, 1000)
	s := &scanner{root: root, ignore: defaultIgnore(), maxFiles: DefaultMaxFiles}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := s.scan(); err != nil {
			b.Fatalf("scan: %v", err)
		}
	}
}
