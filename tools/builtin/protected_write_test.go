package builtin_test

// 保护路径的落盘语义（**已停用强制，DSH 对齐**）。
//
// 旧语义：策略声明为"只读"的路径（.git、宿主状态目录、主目录凭据位置）必须让
// write_file / edit_file 失败。DSH 对齐后策略层只做可写根围栏，AddProtectedPaths
// 是记录型 no-op，因此这些路径现在**照常可写**——本文件改成断言"允许写"。
//
// 仍然拒绝的写侧情形（根外、只读模式、`..`、符号链接逃逸）在
// audit_protection_test.go 与包里的其它用例里覆盖；读路径行为不变。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/builtin"
)

// newProtectedTools 构造一个**声明了** .git 保护（已停用）的 workspace-write 工具集。
func newProtectedTools(t *testing.T) (map[string]rig.Tool, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "config"), []byte("orig = 1\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "app.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write app: %v", err)
	}
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
	)
	policy.AddProtectedPaths(sandbox.WorkspaceMetadataPaths(root)...)
	byName := make(map[string]rig.Tool)
	for _, tool := range builtin.NewWithPolicy(policy) {
		byName[tool.Name()] = tool
	}
	return byName, root
}

// callTool 调用一个工具并返回结果。
func callTool(t *testing.T, tools map[string]rig.Tool, name, arguments string) rig.ToolResult {
	t.Helper()
	tool, ok := tools[name]
	if !ok {
		t.Fatalf("工具 %q 不存在", name)
	}
	result, err := tool.Invoke(context.Background(), rig.ToolCall{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatalf("%s Invoke: %v", name, err)
	}
	return result
}

// TestWriteFileWritesFormerlyProtectedPath：以前拒绝的三个落点现在都必须写成功。
//
// 这是 DSH 对齐的**有意**变更（旧用例名 TestWriteFileRefusesProtectedPath）。
func TestWriteFileWritesFormerlyProtectedPath(t *testing.T) {
	tools, root := newProtectedTools(t)
	for _, target := range []string{".git/config", ".git/hooks/pre-commit", ".git/objects/new"} {
		result := callTool(t, tools, builtin.ToolWriteFile,
			`{"path":"`+target+`","content":"legit\n"}`)
		if result.IsError {
			t.Fatalf("write_file(%q) 被拒绝（保护路径已停用）: %s", target, result.Content)
		}
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(target)))
		if err != nil || string(raw) != "legit\n" {
			t.Fatalf("%s 内容 = %q (%v), want 写入生效", target, raw, err)
		}
	}
	// .git/hooks 目录也应当被建出来（路径在可写根内）。
	if _, err := os.Stat(filepath.Join(root, ".git", "hooks")); err != nil {
		t.Fatalf(".git/hooks 没有被创建: %v", err)
	}
}

// TestEditFileEditsFormerlyProtectedPath：旧用例 TestEditFileRefusesProtectedPath
// 的反向版本。
func TestEditFileEditsFormerlyProtectedPath(t *testing.T) {
	tools, root := newProtectedTools(t)
	result := callTool(t, tools, builtin.ToolEditFile,
		`{"path":".git/config","old_string":"orig = 1","new_string":"hijacked = 1"}`)
	if result.IsError {
		t.Fatalf("edit_file 被拒绝（保护路径已停用）: %s", result.Content)
	}
	if data, err := os.ReadFile(filepath.Join(root, ".git", "config")); err != nil || !strings.Contains(string(data), "hijacked = 1") {
		t.Fatalf(".git/config = %q err=%v, want 编辑生效", data, err)
	}
}

// TestWriteFileStillWorksOutsideProtection 是对照组：普通文件照常可写。
func TestWriteFileStillWorksOutsideProtection(t *testing.T) {
	tools, root := newProtectedTools(t)
	result := callTool(t, tools, builtin.ToolWriteFile, `{"path":"src/new.txt","content":"ok\n"}`)
	if result.IsError {
		t.Fatalf("普通文件写入失败: %s", result.Content)
	}
	if data, err := os.ReadFile(filepath.Join(root, "src", "new.txt")); err != nil || string(data) != "ok\n" {
		t.Fatalf("普通文件内容 = %q err=%v", data, err)
	}
	// edit_file 也一样。
	result = callTool(t, tools, builtin.ToolEditFile,
		`{"path":"app.txt","old_string":"hello","new_string":"bye"}`)
	if result.IsError {
		t.Fatalf("普通文件编辑失败: %s", result.Content)
	}
}

// TestReadFileStillReadsProtectedPath 断言读没有被误伤（读本来就不受保护路径影响）。
func TestReadFileStillReadsProtectedPath(t *testing.T) {
	tools, _ := newProtectedTools(t)
	result := callTool(t, tools, builtin.ToolReadFile, `{"path":".git/config"}`)
	if result.IsError {
		t.Fatalf("读取失败: %s", result.Content)
	}
	if !strings.Contains(result.Content, "orig = 1") {
		t.Fatalf("读取内容 = %q", result.Content)
	}
}

// 旧用例 TestProtectedWriteDenialIsIdentifiable 已删除：它断言
// policy.CheckWritablePath(.git/config) 返回包装 ErrDenied 的错误，而 DSH 对齐后
// 该调用必须返回 nil。这条策略层语义现在由 sandbox 包的 dsh_parity_test.go 钉住，
// 工具层不再重复。
