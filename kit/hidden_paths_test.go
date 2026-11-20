package kit_test

// kit 零配置装配也必须能表达"对模型隐藏某些路径"：宿主不该为了这个能力去自建
// 沙箱策略。本用例钉住 Config → Policy 的接线（工具侧的行为由
// tools/builtin 与 tools/full 的用例覆盖）。

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/kit"
)

func TestKitHiddenPathsReachThePolicy(t *testing.T) {
	root := t.TempDir()
	for name, content := range map[string]string{
		".env":   "TOKEN=kit-hidden-secret\n",
		"ok.txt": "visible\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile %q: %v", name, err)
		}
	}

	agent, err := kit.New(kit.Config{
		Model:       "test-model",
		Root:        root,
		HiddenPaths: []string{"**/.env"},
	})
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	defer func() {
		_ = agent.Shutdown(context.Background())
	}()

	policy := agent.Policy()
	if policy == nil {
		t.Fatal("kit.New 没有产出策略")
	}
	if !slices.Contains(policy.HiddenPatterns(), "**/.env") {
		t.Fatalf("HiddenPaths 没进策略，实际模式: %v", policy.HiddenPatterns())
	}
	if _, err := policy.ResolvePath(".env"); err == nil {
		t.Fatal("策略应拒绝解析被隐藏的 .env")
	} else if !strings.Contains(err.Error(), "hidden") {
		t.Fatalf("拒绝理由应是隐藏命中，实际: %v", err)
	}
	if _, err := policy.ResolvePath("ok.txt"); err != nil {
		t.Fatalf("可见文件不该被拒: %v", err)
	}
}
