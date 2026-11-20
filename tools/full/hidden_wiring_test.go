package full_test

// 装配层的回归：`full.All` 必须把策略里的隐藏规则接到 search 上。
//
// search 走自己的文件遍历（不经过 Policy.OpenPath），所以它是唯一一个"策略改了
// 但工具不知道"的漏点。这条用例钉住接线，避免以后有人重排 AllOptions 时把它丢掉。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/full"
)

func TestFullWiringAppliesHiddenPathsToSearch(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		".env":        "TOKEN=full-wiring-secret\n",
		"src/main.go": "package main\n\n// full-wiring-secret appears here too\n",
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

	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite), sandbox.WithHidden("**/.env"))
	tools := full.All(full.AllOptions{Root: root, Policy: policy})

	var searchTool rig.Tool
	for _, tool := range tools {
		if tool.Name() == "search" {
			searchTool = tool
			break
		}
	}
	if searchTool == nil {
		t.Fatal("full.All 没有装配 search 工具")
	}

	payload, err := json.Marshal(map[string]any{"mode": "text", "query": "full-wiring-secret"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	result, err := searchTool.Invoke(context.Background(), rig.ToolCall{Name: "search", Arguments: string(payload)})
	if err != nil {
		t.Fatalf("search.Invoke 返回 Go error: %v", err)
	}
	if strings.Contains(result.Content, ".env") {
		t.Fatalf("装配后的 search 命中了被隐藏的 .env：\n%s", result.Content)
	}
	if !strings.Contains(result.Content, "src/main.go") {
		t.Fatalf("可见文件应仍被命中（否则用例可能只是没搜到）：\n%s", result.Content)
	}
}
