package builtin_test

// 隐藏路径必须对模型真的不可见：不只是 read_file 被拒，list_dir / glob / grep 的结果
// 里也不能出现它。v0.24.0 之前这靠 WithDeniedGlobs（后来被删除），这一层
// 完全消失。本文件是"隐藏"这条产品承诺的行为级回归。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/builtin"
)

// hiddenFixture 建一棵含隐藏文件的小树，并装配带隐藏规则的文件工具。
type hiddenFixture struct {
	root  string
	tools map[string]rig.Tool
}

func newHiddenFixture(t *testing.T) *hiddenFixture {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		".env":            "SECRET=42\n",
		"ok.txt":          "hello\n",
		"src/main.go":     "package main\n",
		"secrets/key.pem": "TOP-SECRET-PEM\n",
	}
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll %q: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile %q: %v", name, err)
		}
	}
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithHidden("**/.env", "secrets/**"),
	)
	byName := make(map[string]rig.Tool)
	for _, tool := range builtin.NewWithPolicy(policy) {
		byName[tool.Name()] = tool
	}
	return &hiddenFixture{root: root, tools: byName}
}

// invokeHidden 调一次工具并把 Go error 当失败（被拒是 ToolResult.IsError）。
func (f *hiddenFixture) invokeHidden(t *testing.T, name string, args map[string]any) rig.ToolResult {
	t.Helper()
	payload, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	tool, ok := f.tools[name]
	if !ok {
		t.Fatalf("没有工具 %q", name)
	}
	result, invokeErr := tool.Invoke(context.Background(), rig.ToolCall{ID: "hidden", Name: name, Arguments: string(payload)})
	if invokeErr != nil {
		t.Fatalf("%s.Invoke 返回 Go error: %v", name, invokeErr)
	}
	return result
}

// TestHiddenPathsStayInvisibleToFileTools 覆盖 read_file / list_dir / glob / grep 四条路。
func TestHiddenPathsStayInvisibleToFileTools(t *testing.T) {
	fixture := newHiddenFixture(t)

	t.Run("read_file", func(t *testing.T) {
		result := fixture.invokeHidden(t, builtin.ToolReadFile, map[string]any{"path": ".env"})
		if !result.IsError {
			t.Fatalf("read_file(.env) 应当失败，实际成功并返回: %q", result.Content)
		}
		if !strings.Contains(strings.ToLower(result.Content), "hidden") {
			t.Fatalf("拒绝理由应说明是隐藏命中，实际: %q", result.Content)
		}
		ok := fixture.invokeHidden(t, builtin.ToolReadFile, map[string]any{"path": "ok.txt"})
		if ok.IsError {
			t.Fatalf("read_file(ok.txt) 不该失败: %q", ok.Content)
		}
	})

	t.Run("list_dir", func(t *testing.T) {
		result := fixture.invokeHidden(t, builtin.ToolListDir, map[string]any{"path": "."})
		if result.IsError {
			t.Fatalf("list_dir(.) 不该失败: %q", result.Content)
		}
		if strings.Contains(result.Content, ".env") {
			t.Fatalf("list_dir 结果里不该出现 .env:\n%s", result.Content)
		}
		if strings.Contains(result.Content, "secrets") {
			t.Fatalf("list_dir 结果里不该出现被隐藏的目录 secrets:\n%s", result.Content)
		}
		if !strings.Contains(result.Content, "ok.txt") {
			t.Fatalf("list_dir 结果里应有 ok.txt:\n%s", result.Content)
		}
	})

	t.Run("glob", func(t *testing.T) {
		result := fixture.invokeHidden(t, builtin.ToolGlob, map[string]any{"pattern": "**/*"})
		if result.IsError {
			t.Fatalf("glob 不该失败: %q", result.Content)
		}
		if strings.Contains(result.Content, ".env") || strings.Contains(result.Content, "key.pem") {
			t.Fatalf("glob 结果里不该出现隐藏项:\n%s", result.Content)
		}
		if !strings.Contains(result.Content, "src/main.go") {
			t.Fatalf("glob 结果里应有 src/main.go:\n%s", result.Content)
		}
	})

	t.Run("write_file", func(t *testing.T) {
		// 隐藏是"模型看不到"，写也得挡住：否则模型仍能把内容覆盖进一个它读不回来
		// 的文件（对用户来说是更难查的破坏）。
		result := fixture.invokeHidden(t, builtin.ToolWriteFile, map[string]any{"path": ".env", "content": "OVERWRITTEN=1\n"})
		if !result.IsError {
			t.Fatalf("write_file(.env) 应被隐藏拒绝，实际: %q", result.Content)
		}
		content, err := os.ReadFile(filepath.Join(fixture.root, ".env"))
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if string(content) != "SECRET=42\n" {
			t.Fatalf("隐藏文件被改写了: %q", content)
		}
		ok := fixture.invokeHidden(t, builtin.ToolWriteFile, map[string]any{"path": "ok.txt", "content": "updated\n"})
		if ok.IsError {
			t.Fatalf("write_file(ok.txt) 不该失败: %q", ok.Content)
		}
	})

	t.Run("grep", func(t *testing.T) {
		result := fixture.invokeHidden(t, builtin.ToolGrep, map[string]any{"pattern": "SECRET|TOP-SECRET"})
		if result.IsError {
			t.Fatalf("grep 不该失败: %q", result.Content)
		}
		if strings.Contains(result.Content, ".env") || strings.Contains(result.Content, "key.pem") {
			t.Fatalf("grep 命中了隐藏文件:\n%s", result.Content)
		}
	})
}
