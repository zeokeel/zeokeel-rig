package patch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
)

// invokeWith 用给定工具执行一次补丁。
func invokeWith(t *testing.T, tool rig.Tool, patchText string) (string, bool) {
	t.Helper()
	result, err := tool.Invoke(context.Background(), rig.ToolCall{
		ID: "c", Name: ToolName, Arguments: `{"patch":` + mustJSONString(t, patchText) + `}`,
	})
	if err != nil {
		return "GO ERROR: " + err.Error(), true
	}
	return result.Content, result.IsError
}

// TestAuditMaxFileBytes 覆盖目标文件读取上限：默认 16 MiB，超限必须给出明确错误，
// 并且**必须在整读之前**判定（用"超大且含 NUL 的稀疏文件"区分两种错误）。
func TestAuditMaxFileBytes(t *testing.T) {
	t.Run("默认上限", func(t *testing.T) {
		root := t.TempDir()
		concrete, ok := newForTest(root).(*tool)
		if !ok {
			t.Fatalf("New 返回的类型 = %T", newForTest(root))
		}
		if got, want := concrete.effectiveMaxFileBytes(), DefaultMaxFileBytes; got != want {
			t.Fatalf("默认上限 = %d, want %d", got, want)
		}
		if DefaultMaxFileBytes != 16<<20 {
			t.Fatalf("DefaultMaxFileBytes = %d, want 16 MiB", DefaultMaxFileBytes)
		}
		if got := newForTest(root, WithMaxFileBytes(0), WithMaxFileBytes(-1)); got.(*tool).effectiveMaxFileBytes() != DefaultMaxFileBytes {
			t.Fatal("非正上限应回落到默认值")
		}
	})

	t.Run("自定义上限拒绝超大目标", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "big.txt")
		if err := os.WriteFile(path, []byte("0123456789"), 0o644); err != nil {
			t.Fatal(err)
		}
		tool := newForTest(root, WithMaxFileBytes(8))
		out, isErr := invokeWith(t, tool, "*** Begin Patch\n*** Update File: big.txt\n@@\n-0123456789\n+small\n*** End Patch\n")
		if !isErr {
			t.Fatalf("超过文件大小上限仍然成功: %q", out)
		}
		for _, fragment := range []string{"file too large to patch", "> limit", "rewrite it with write_file"} {
			if !strings.Contains(out, fragment) {
				t.Fatalf("错误文案缺少 %q: %q", fragment, out)
			}
		}
		if got := readFileString(t, path); got != "0123456789" {
			t.Fatalf("被拒绝的补丁改动了文件: %q", got)
		}
	})

	t.Run("删除路径同样受限", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "big.txt"), []byte("0123456789"), 0o644); err != nil {
			t.Fatal(err)
		}
		tool := newForTest(root, WithMaxFileBytes(8))
		out, isErr := invokeWith(t, tool, "*** Begin Patch\n*** Delete File: big.txt\n*** End Patch\n")
		if !isErr {
			t.Fatalf("超限文件被删除: %q", out)
		}
		if _, err := os.Stat(filepath.Join(root, "big.txt")); err != nil {
			t.Fatalf("文件被删掉了: %v", err)
		}
	})

	t.Run("先判大小再整读", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "huge.bin")
		// 稀疏文件：不占内存，但内容全是 NUL 字节（若实现先读再查二进制，会得到
		// "binary file"；先判大小才得到 "too large"）。
		if err := os.Truncate(path, int64(DefaultMaxFileBytes)+4096); err != nil {
			t.Skipf("无法创建稀疏文件: %v", err)
		}
		// 需要一个真实内容让补丁有上下文可用（否则会先报上下文缺失）。
		out, isErr := invokeWith(t, newForTest(root), "*** Begin Patch\n*** Update File: huge.bin\n@@\n-x\n+y\n*** End Patch\n")
		if !isErr {
			t.Fatalf("超大文件未被拒绝: %q", out)
		}
		if !strings.Contains(out, "file too large to patch") {
			t.Fatalf("应当在整读之前按大小拒绝（而不是二进制/上下文错误）: %q", out)
		}
	})
}

// TestAuditMaxFiles 覆盖单补丁文件数上限：默认 200，超限必须拒绝并且零改动。
func TestAuditMaxFiles(t *testing.T) {
	t.Run("默认上限", func(t *testing.T) {
		root := t.TempDir()
		concrete := newForTest(root).(*tool)
		if got, want := concrete.effectiveMaxFiles(), DefaultMaxFiles; got != want {
			t.Fatalf("默认文件数上限 = %d, want %d", got, want)
		}
		if DefaultMaxFiles != 200 {
			t.Fatalf("DefaultMaxFiles = %d, want 200", DefaultMaxFiles)
		}
		if newForTest(root, WithMaxFiles(0), WithMaxFiles(-3)).(*tool).effectiveMaxFiles() != DefaultMaxFiles {
			t.Fatal("非正上限应回落到默认值")
		}
	})

	t.Run("超限拒绝且零改动", func(t *testing.T) {
		root := t.TempDir()
		tool := newForTest(root, WithMaxFiles(2))
		var builder strings.Builder
		builder.WriteString("*** Begin Patch\n")
		for _, name := range []string{"a.md", "b.md", "c.md"} {
			builder.WriteString("*** Add File: " + name + "\n+x\n")
		}
		builder.WriteString("*** End Patch\n")
		out, isErr := invokeWith(t, tool, builder.String())
		if !isErr {
			t.Fatalf("超过文件数上限仍然成功: %q", out)
		}
		if !strings.Contains(out, "touches 3 files") || !strings.Contains(out, "limit 2") || !strings.Contains(out, "split it") {
			t.Fatalf("错误文案不符合约定: %q", out)
		}
		entries, _ := os.ReadDir(root)
		if len(entries) != 0 {
			t.Fatalf("被拒绝的补丁留下了 %d 个条目", len(entries))
		}
	})

	t.Run("恰好等于上限时成功", func(t *testing.T) {
		root := t.TempDir()
		tool := newForTest(root, WithMaxFiles(2))
		out, isErr := invokeWith(t, tool, "*** Begin Patch\n*** Add File: a.md\n+x\n*** Add File: b.md\n+y\n*** End Patch\n")
		if isErr {
			t.Fatalf("恰好等于上限应当成功: %s", out)
		}
	})

	t.Run("dry-run 也受限", func(t *testing.T) {
		root := t.TempDir()
		tool := newForTest(root, WithMaxFiles(1), WithDryRun(true))
		out, isErr := invokeWith(t, tool, "*** Begin Patch\n*** Add File: a.md\n+x\n*** Add File: b.md\n+y\n*** End Patch\n")
		if !isErr {
			t.Fatalf("dry-run 也应当拒绝超限补丁: %q", out)
		}
	})
}

// TestAuditLimitsDoNotChangeDefaults 守门：没有显式选项时，正常大小的补丁行为不变。
func TestAuditLimitsDoNotChangeDefaults(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a.md")
	if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, isErr := invokeWith(t, newForTest(root), "*** Begin Patch\n*** Update File: a.md\n@@\n-old\n+new\n*** End Patch\n")
	if isErr {
		t.Fatalf("正常补丁被拒绝: %s", out)
	}
	if got := readFileString(t, path); got != "new\n" {
		t.Fatalf("内容 = %q", got)
	}
}
