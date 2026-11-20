package full_test

// 技能默认库的接线契约：full.All 必须把 AllOptions.Root 传给 skill 工具。
//
// 守护进程 / GUI 的工作区根与进程 cwd 通常不同。旧的接线让 skill 工具回落到
// os.Getwd()，于是 <Root>/.zeokeel/skills 下的项目级技能静默消失——工具还能用，
// 只是永远列不出项目自己的技能。这里在 full 的接线上钉住它（skill 包内部另有
// ToolsWithRoot 的单元测试）。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/full"
)

// TestSkillToolUsesAllOptionsRoot：Root 指向临时工作区、进程 cwd 在别处时，
// 仍然能发现 <Root>/.zeokeel/skills 下的技能。
func TestSkillToolUsesAllOptionsRoot(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, ".zeokeel", "skills", "wired-skill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("# Wired Skill\ndescription: comes from AllOptions.Root\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// 隔离默认库的其它来源，避免用例受开发机上的技能影响。
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZEOKEEL_SKILLS", "")
	// 进程 cwd 换到别处：这就是守护进程的真实形态。
	t.Chdir(t.TempDir())

	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	var skillTool rig.Tool
	for _, tool := range full.All(full.AllOptions{Root: root, Policy: policy}) {
		if tool.Name() == "skill" {
			skillTool = tool
			break
		}
	}
	if skillTool == nil {
		t.Fatal("full.All 没有构造 skill 工具")
	}
	result, err := skillTool.Invoke(context.Background(), rig.ToolCall{
		ID: "c1", Name: "skill", Arguments: `{"action":"list"}`,
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.IsError {
		t.Fatalf("list 报错: %s", result.Content)
	}
	if !strings.Contains(result.Content, "wired-skill") {
		t.Fatalf("AllOptions.Root 下的技能没有被发现:\n%s", result.Content)
	}
}
