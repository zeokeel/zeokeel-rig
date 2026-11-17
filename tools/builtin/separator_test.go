package builtin_test

// A6：非 Windows 平台上，路径参数里的 "\" 不是分隔符。
//
// 在 POSIX 上 `write_file .git\config` 会落盘成一个**名字里带反斜杠的普通文件**：
// 既没写进模型以为的 .git，也绕开了所有基于 "/" 的策略判定（保护路径、拒绝 glob）。
// 统一拒绝比"悄悄创建一个怪名字文件"更诚实。

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/builtin"
)

func newFileTools(t *testing.T) (map[string]rig.Tool, string) {
	t.Helper()
	root := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	byName := make(map[string]rig.Tool)
	for _, tool := range builtin.NewWithPolicy(policy) {
		byName[tool.Name()] = tool
	}
	return byName, root
}

// TestBackslashPathIsRejectedOnPOSIX 断言三个文件工具都拒绝反斜杠路径。
func TestBackslashPathIsRejectedOnPOSIX(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上 \\ 是合法分隔符")
	}
	tools, root := newFileTools(t)
	cases := []struct {
		tool string
		args string
	}{
		{builtin.ToolWriteFile, `{"path":".git\\config","content":"x"}`},
		{builtin.ToolWriteFile, `{"path":"dir\\file.txt","content":"x"}`},
		{builtin.ToolEditFile, `{"path":"dir\\file.txt","old_string":"a","new_string":"b"}`},
		{builtin.ToolReadFile, `{"path":"dir\\file.txt"}`},
	}
	for _, testCase := range cases {
		result := callTool(t, tools, testCase.tool, testCase.args)
		if !result.IsError {
			t.Fatalf("%s(%s) 没有被拒绝: %s", testCase.tool, testCase.args, result.Content)
		}
		lowered := strings.ToLower(result.Content)
		if !strings.Contains(lowered, "backslash") && !strings.Contains(lowered, "separator") {
			t.Fatalf("%s 的错误文案没说清分隔符问题: %q", testCase.tool, result.Content)
		}
	}
	// 关键：不能真的落盘出字面文件名 `dir\file.txt` 或 `.git\config`。
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), `\`) {
			t.Fatalf("落盘了带反斜杠的字面文件名: %q", entry.Name())
		}
	}
	// 对照组：用 "/" 的路径照常工作。
	if result := callTool(t, tools, builtin.ToolWriteFile, `{"path":"dir/file.txt","content":"ok"}`); result.IsError {
		t.Fatalf("正常路径被拒: %s", result.Content)
	}
	if data, err := os.ReadFile(filepath.Join(root, "dir", "file.txt")); err != nil || string(data) != "ok" {
		t.Fatalf("正常路径内容 = %q err=%v", data, err)
	}
}

// TestBackslashPolicyErrorMentionsSlash 断言文案给出可操作建议（用 "/"）。
func TestBackslashPolicyErrorMentionsSlash(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上 \\ 是合法分隔符")
	}
	tools, _ := newFileTools(t)
	result := callTool(t, tools, builtin.ToolWriteFile, `{"path":".git\\config","content":"x"}`)
	if !result.IsError || !strings.Contains(result.Content, "/") {
		t.Fatalf("错误文案应当建议使用 /: %q", result.Content)
	}
}

// TestWriteFileRejectsDotDot 断言写工具一律拒 ".."（apply_patch 现在与它一致）。
func TestWriteFileRejectsDotDot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("先只锁 POSIX 行为")
	}
	tools, root := newFileTools(t)
	for _, target := range []string{"a/../b.txt", "./a/../b.txt", ".git/../escape.txt"} {
		args := `{"path":"` + target + `","content":"x"}`
		result := callTool(t, tools, builtin.ToolWriteFile, args)
		if !result.IsError {
			t.Fatalf("write_file(%q) 没有被拒绝: %s", target, result.Content)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "escape.txt")); err == nil {
		t.Fatal(".. 形态被 Clean 成了 escape.txt 并创建")
	}
}
