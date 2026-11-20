package kit_test

// UndoDir 接线：非 git 工作区挂纯文件系统快照后端；git 仓库仍然优先用 git。

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/kit"
	"github.com/zeokeel/zeokeel-rig/tools/undo"
)

// hasRegisteredTool 报告 agent 是否注册了指定工具。
func hasRegisteredTool(agent *kit.Agent, name string) bool {
	for _, candidate := range agent.RegisteredTools() {
		if candidate == name {
			return true
		}
	}
	return false
}

// TestKitUndoDirWiresFileTrackerInNonGitRoot 断言：工作区不是 git 仓库、但设置了
// UndoDir 时，kit 装上文件系统快照后端，因此 undo 工具出现。
func TestKitUndoDirWiresFileTrackerInNonGitRoot(t *testing.T) {
	root := t.TempDir()
	if isRepo, err := undo.IsGitRepo(context.Background(), root); err != nil || isRepo {
		t.Skipf("临时目录不是非 git 工作区: isRepo=%v err=%v", isRepo, err)
	}
	undoDir := filepath.Join(t.TempDir(), "undo")
	provider := &scriptProvider{scripts: [][]kit.Chunk{textScript("done")}}
	agent := mustNew(t, kit.Config{Model: "m", Provider: provider, Root: root, UndoDir: undoDir})

	if agent.UndoTracker() == nil {
		t.Fatal("非 git 工作区 + UndoDir 应当挂上文件系统快照后端")
	}
	if !hasRegisteredTool(agent, "undo") {
		t.Fatalf("挂了 tracker 之后 undo 工具应当出现: %v", agent.RegisteredTools())
	}
	if _, _, err := agent.RunText(context.Background(), "s1", "go", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	// 文件后端的目录布局：<UndoDir>/turns/<turn>/{manifest.json,files/…}。
	if _, err := os.Stat(filepath.Join(undoDir, "turns", "1", "manifest.json")); err != nil {
		t.Fatalf("回合快照没有落在 UndoDir/turns/1（文件后端没生效）: %v", err)
	}
	if turns := agent.UndoTracker().Turns(); len(turns) != 1 || turns[0] != 1 {
		t.Fatalf("Turns() = %v, want [1]", turns)
	}
}

// TestKitGitUndoStillWinsInGitRepo 断言：git 仓库里即使设置了 UndoDir，也优先用 git
// 后端（GitUndo 的语义不变）。
func TestKitGitUndoStillWinsInGitRepo(t *testing.T) {
	root := gitRepoRoot(t)
	run := func(args ...string) string {
		command := exec.Command("git", append([]string{"-C", root}, args...)...)
		command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1")
		out, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
		return string(out)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	run("add", "-A")
	run("commit", "-qm", "init")

	undoDir := filepath.Join(t.TempDir(), "undo")
	provider := &scriptProvider{scripts: [][]kit.Chunk{textScript("done")}}
	agent := mustNew(t, kit.Config{
		Model: "m", Provider: provider, Root: root, GitUndo: true, UndoDir: undoDir,
	})
	if agent.UndoTracker() == nil {
		t.Fatal("git 仓库 + GitUndo 应当有 tracker")
	}
	if _, _, err := agent.RunText(context.Background(), "s1", "go", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	if strings.TrimSpace(run("for-each-ref", "refs/rig/undo")) == "" {
		t.Fatal("git 后端没有拍快照（refs/rig/undo 为空）")
	}
	if turns := agent.UndoTracker().Turns(); len(turns) != 1 || turns[0] != 1 {
		t.Fatalf("Turns() = %v, want [1]", turns)
	}
	if _, err := os.Stat(filepath.Join(undoDir, "turns", "1")); err == nil {
		t.Fatal("git 优先时不应再生成文件快照回合目录")
	}
}
