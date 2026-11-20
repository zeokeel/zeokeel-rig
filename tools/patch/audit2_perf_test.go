package patch_test

// 第 2D 轮审计：性能（1000 行文件 + 20 个小补丁）。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/tools/patch"
)

// 1000 行文件上打 20 个小补丁的总耗时。
func TestAuditPerf20SmallPatchesOn1000Lines(t *testing.T) {
	root := t.TempDir()
	var builder strings.Builder
	for i := 0; i < 1000; i++ {
		fmt.Fprintf(&builder, "line %04d: some content here\n", i)
	}
	writeFile(t, root, "big.txt", builder.String())

	tool := patch.New(root, patch.WithPolicy(workspacePolicy(root)))
	start := time.Now()
	for round := 0; round < 20; round++ {
		// 每轮改一行（行号分散，避免总是命中缓存）。
		target := (round * 47) % 1000
		patchText := fmt.Sprintf("*** Begin Patch\n*** Update File: big.txt\n@@\n-line %04d: some content here\n+line %04d: PATCHED round %d\n*** End Patch\n", target, target, round)
		content, isError := invokePatch(t, tool, patchText)
		if isError {
			t.Fatalf("第 %d 个补丁失败: %s", round, content)
		}
	}
	elapsed := time.Since(start)
	t.Logf("1000 行文件 × 20 个小补丁：总计 %v（平均 %v/补丁）", elapsed, elapsed/20)
	if elapsed > 10*time.Second {
		t.Fatalf("20 个小补丁耗时 %v，明显不正常", elapsed)
	}

	// 结果正确性抽查：最后一次编辑必须生效，且文件行数不变。
	rawBytes, err := os.ReadFile(filepath.Join(root, "big.txt"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	raw := string(rawBytes)
	if got := strings.Count(raw, "\n"); got != 1000 {
		t.Fatalf("行数变成了 %d", got)
	}
	if !strings.Contains(raw, "PATTERN-DOES-NOT-EXIST") && !strings.Contains(raw, "PATCHED round 19") {
		t.Fatalf("最后一轮补丁没有生效")
	}
}
