package costmeter

// 架构守卫：costmeter 必须是**纯计算**包，只依赖 rig.Usage。
// 一旦有人在这里 import provider/agent/tools，本包的可用范围（UI 线程、回放脚本、
// 测试）立刻塌掉，还会把成本计量和网络/工具耦合在一起。

import (
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// allowedImport 报告该 import 是否被允许。
func allowedImport(path string) bool {
	// 标准库：没有第一个路径段之外的点（"context"、"math"、"sync/atomic"…）。
	first := path
	if slash := strings.IndexByte(path, '/'); slash >= 0 {
		first = path[:slash]
	}
	if !strings.Contains(first, ".") {
		return true
	}
	switch path {
	case "github.com/zeokeel/zeokeel-rig": // 只为了 rig.Usage
		return true
	case "github.com/zeokeel/zeokeel-rig/modelprice": // 内置价目表
		return true
	default:
		return false
	}
}

func TestImportsStayPure(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("读目录: %v", err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("解析 %s: %v", name, err)
		}
		checked++
		for _, spec := range file.Imports {
			path := strings.Trim(spec.Path.Value, `"`)
			if !allowedImport(path) {
				t.Errorf("%s import 了 %q：costmeter 必须保持纯计算（只依赖 rig.Usage 与内置价目表）", name, path)
			}
		}
	}
	if checked == 0 {
		t.Fatal("没有检查到任何非测试源文件")
	}
}
