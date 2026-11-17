package builtin_test

// 保护路径的落盘语义：策略声明为"只读"的路径（.git、宿主状态目录、主目录凭据
// 位置）必须让 write_file / edit_file 失败，并给出可读、可判定的错误；读工具不受
// 影响（读 .git/HEAD 仍然合法）。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/builtin"
)

// newProtectedTools 构造一个开启了 .git 保护的 workspace-write 工具集。
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

// TestWriteFileRefusesProtectedPath 断言 write_file 不写保护路径，且原文完好。
func TestWriteFileRefusesProtectedPath(t *testing.T) {
	tools, root := newProtectedTools(t)
	for _, target := range []string{".git/config", ".git/hooks/pre-commit", ".git/objects/new"} {
		result := callTool(t, tools, builtin.ToolWriteFile,
			`{"path":"`+target+`","content":"hijacked\n"}`)
		if !result.IsError {
			t.Fatalf("write_file(%q) 没有被拒绝: %s", target, result.Content)
		}
		if !strings.Contains(result.Content, "protected") {
			t.Fatalf("错误文案没有说明是保护路径: %q", result.Content)
		}
	}
	if data, err := os.ReadFile(filepath.Join(root, ".git", "config")); err != nil || string(data) != "orig = 1\n" {
		t.Fatalf(".git/config 被改写了: %q err=%v", data, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".git", "hooks")); err == nil {
		t.Fatal(".git/hooks 被创建了")
	}
}

// TestEditFileRefusesProtectedPath 断言 edit_file 同样拒绝。
func TestEditFileRefusesProtectedPath(t *testing.T) {
	tools, root := newProtectedTools(t)
	result := callTool(t, tools, builtin.ToolEditFile,
		`{"path":".git/config","old_string":"orig = 1","new_string":"hijacked = 1"}`)
	if !result.IsError {
		t.Fatalf("edit_file 没有被拒绝: %s", result.Content)
	}
	if data, err := os.ReadFile(filepath.Join(root, ".git", "config")); err != nil || !strings.Contains(string(data), "orig = 1") {
		t.Fatalf(".git/config 被改写了: %q err=%v", data, err)
	}
}

// TestWriteFileStillWorksOutsideProtection 是对照组：同一策略下普通文件照常可写。
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

// TestReadFileStillReadsProtectedPath 断言读没有被误伤：保护是"只读"，不是"不可见"。
func TestReadFileStillReadsProtectedPath(t *testing.T) {
	tools, _ := newProtectedTools(t)
	result := callTool(t, tools, builtin.ToolReadFile, `{"path":".git/config"}`)
	if result.IsError {
		t.Fatalf("读取保护路径失败（保护应当是只读而不是禁止读）: %s", result.Content)
	}
	if !strings.Contains(result.Content, "orig = 1") {
		t.Fatalf("读取内容 = %q", result.Content)
	}
}

// TestProtectedWriteDenialIsIdentifiable 断言错误可通过 errors.Is 判定（工具错误
// 文本会进模型上下文，宿主也可能据此统计）。
func TestProtectedWriteDenialIsIdentifiable(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	policy.AddProtectedPaths(filepath.Join(root, ".git"))
	err := policy.CheckWritablePath(filepath.Join(root, ".git", "config"))
	if err == nil || !errors.Is(err, sandbox.ErrDenied) {
		t.Fatalf("err = %v, want sandbox.ErrDenied", err)
	}
}
