package kit_test

// C4 审计（.audit/audit/c4-undo.md）里 kit 接线的守护用例：
//   - C4-3 忽略表按实际 UndoDir 生成（不能只写死 `.rig`）；
//   - C4-4 NewFileTracker 的配置错误不能被静默吞掉。

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

// TestKitGitUndoIgnoresActualUndoDirState 守护 C4-3：宿主把 UndoDir 设成 Root 内的
// 非 `.rig` 目录时，下一回合 Begin 改写 state.json 不能被上一回合当成"回合改动"。
func TestKitGitUndoIgnoresActualUndoDirState(t *testing.T) {
	root := gitRepoRoot(t)
	undoDir := filepath.Join(root, "snapshots")

	// 一份初始提交，让 git 快照的基线稳定。
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-qm", "init"}} {
		command := exec.Command("git", append([]string{"-C", root}, args...)...)
		command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}

	provider := &scriptProvider{scripts: [][]kit.Chunk{textScript("one"), textScript("two")}}
	agent := mustNew(t, kit.Config{
		Model: "m", Provider: provider, Root: root, GitUndo: true, UndoDir: undoDir,
	})
	if agent.UndoTracker() == nil {
		t.Fatal("git 仓库 + GitUndo 应当有 tracker")
	}
	if _, _, err := agent.RunText(context.Background(), "s1", "go", nil); err != nil {
		t.Fatalf("RunText #1: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("a2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile a2: %v", err)
	}
	// 第二个回合开始时会改写 <UndoDir>/state.json。
	if _, _, err := agent.RunText(context.Background(), "s1", "go", nil); err != nil {
		t.Fatalf("RunText #2: %v", err)
	}
	if _, err := os.Stat(filepath.Join(undoDir, "state.json")); err != nil {
		t.Fatalf("回合号没有落在 UndoDir/state.json: %v", err)
	}

	changes, err := agent.UndoTracker().Changes(1, "")
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes) != 1 || changes[0].Path != "a.txt" {
		t.Fatalf("Changes(1) = %+v, want 只有 a.txt（UndoDir 里的 state.json 必须被忽略）", changes)
	}
	for _, change := range changes {
		if strings.HasPrefix(change.Path, "snapshots/") {
			t.Fatalf("UndoDir 里的状态文件被当成回合改动: %+v", change)
		}
	}
}

// TestKitUndoDirConfigurationErrorIsReported 守护 C4-4：`UndoDir == Root` 这类配置
// 错误必须从 kit.New 返回，而不是静默把 undo 摘掉。
func TestKitUndoDirConfigurationErrorIsReported(t *testing.T) {
	root := t.TempDir()
	if isRepo, err := undo.IsGitRepo(context.Background(), root); err != nil || isRepo {
		t.Skipf("临时目录不是非 git 工作区: isRepo=%v err=%v", isRepo, err)
	}
	provider := &scriptProvider{scripts: [][]kit.Chunk{textScript("done")}}
	_, err := kit.New(kit.Config{Model: "m", Provider: provider, Root: root, UndoDir: root})
	if err == nil {
		t.Fatal("UndoDir 与 Root 相同时 kit.New 必须报错（不能静默吞掉）")
	}
	message := err.Error()
	if !strings.Contains(message, "undo") || !strings.Contains(message, "UndoDir") {
		t.Fatalf("错误文案 = %q, want 同时提到 undo 与 UndoDir", message)
	}
}
