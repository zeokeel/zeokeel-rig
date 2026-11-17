package undo_test

// Tracker 与 undo 工具的语义测试（回合号、保留、工具入参/文案）。

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/undo"
)

// invokeTool 调用 undo 工具并返回文本。
func invokeTool(t *testing.T, tool rig.Tool, arguments string) string {
	t.Helper()
	result, err := tool.Invoke(context.Background(), rig.ToolCall{Name: tool.Name(), Arguments: arguments})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if strings.TrimSpace(result.Content) == "" {
		t.Fatalf("tool returned no content (result=%+v)", result)
	}
	return result.Content
}

// TestTrackerTurnsAndRetention 证明回合号递增、保留上限生效、Resolve 语义正确。
func TestTrackerTurnsAndRetention(t *testing.T) {
	repo := newRepo(t)
	writeTree(t, repo, map[string]string{"a.txt": "a\n"})
	commitAll(t, repo)

	tracker := undo.NewTracker(repo, undo.Options{Retain: 2})
	for turn := 1; turn <= 3; turn++ {
		got, err := tracker.Begin()
		if err != nil {
			t.Fatalf("Begin #%d: %v", turn, err)
		}
		if got != turn {
			t.Fatalf("Begin 返回回合号 %d, want %d", got, turn)
		}
	}
	turns := tracker.Turns()
	if len(turns) != 2 || turns[0] != 2 || turns[1] != 3 {
		t.Fatalf("Turns() = %v, want [2 3]（Retain=2，最早的被挤掉）", turns)
	}
	if resolved, err := tracker.Resolve(0); err != nil || resolved != 3 {
		t.Fatalf("Resolve(0) = %d, %v; want 3", resolved, err)
	}
	if _, err := tracker.Resolve(1); err == nil {
		t.Fatal("Resolve(1) 应当报错（回合已被挤掉）")
	}
	if _, err := tracker.Resolve(99); err == nil {
		t.Fatal("Resolve(99) 应当报错")
	}
}

// TestTrackerWithoutSnapshot 证明没有快照时各入口给出可读错误。
func TestTrackerWithoutSnapshot(t *testing.T) {
	repo := newRepo(t)
	tracker := undo.NewTracker(repo, undo.Options{})
	if _, err := tracker.Changes(0, ""); err == nil {
		t.Fatal("没有快照时 Changes 应当报错")
	} else if !strings.Contains(err.Error(), "no snapshot") {
		t.Fatalf("错误文案 = %v", err)
	}
	if _, err := tracker.Revert(0); err == nil {
		t.Fatal("没有快照时 Revert 应当报错")
	}
	var nilTracker *undo.Tracker
	if _, err := nilTracker.Begin(); err == nil {
		t.Fatal("nil tracker 应当报错而不是 panic")
	}
}

// TestTrackerBeginWithoutGitRepo 证明非 git 仓库的 Begin 返回 ErrNotGitRepo，且不消耗回合。
func TestTrackerBeginWithoutGitRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git 不可用: %v", err)
	}
	plain := t.TempDir()
	tracker := undo.NewTracker(plain, undo.Options{})
	if _, err := tracker.Begin(); err == nil {
		t.Fatal("非 git 仓库的 Begin 应当失败")
	} else if !strings.Contains(err.Error(), "not a git repository") {
		t.Fatalf("错误 = %v, want ErrNotGitRepo", err)
	}
	if turns := tracker.Turns(); len(turns) != 0 {
		t.Fatalf("失败时不应留下快照: %v", turns)
	}
}

// TestToolRevertsLatestTurn 覆盖工具端到端：改 → undo → 恢复。
func TestToolRevertsLatestTurn(t *testing.T) {
	repo := newRepo(t)
	writeTree(t, repo, map[string]string{"a.txt": "one\n"})
	commitAll(t, repo)
	tracker := undo.NewTracker(repo, undo.Options{})
	if _, err := tracker.Begin(); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	writeTree(t, repo, map[string]string{"a.txt": "two\n", "b.txt": "new\n"})

	dry := invokeTool(t, undo.New(tracker), `{"dry_run":true}`)
	if !strings.Contains(dry, "would revert 2 change(s)") {
		t.Fatalf("dry-run 文案 = %q", dry)
	}
	if got := read(t, repo, "a.txt"); got != "two\n" {
		t.Fatalf("dry-run 落盘了: %q", got)
	}

	out := invokeTool(t, undo.New(tracker), `{}`)
	if !strings.Contains(out, "reverted 2 change(s)") {
		t.Fatalf("回退文案 = %q", out)
	}
	if got := read(t, repo, "a.txt"); got != "one\n" {
		t.Fatalf("a.txt = %q, want one", got)
	}
	if _, err := os.Stat(filepath.Join(repo, "b.txt")); !os.IsNotExist(err) {
		t.Fatalf("b.txt 应被删除: err=%v", err)
	}
	// 再撤一次：没有变更。
	if again := invokeTool(t, undo.New(tracker), `{}`); !strings.Contains(again, "no changes") {
		t.Fatalf("第二次撤销文案 = %q", again)
	}
}

// TestToolTurnAndPathArguments 覆盖 turn / path 两个参数。
func TestToolTurnAndPathArguments(t *testing.T) {
	repo := newRepo(t)
	writeTree(t, repo, map[string]string{"a.txt": "v1\n", "b.txt": "v1\n"})
	commitAll(t, repo)
	tracker := undo.NewTracker(repo, undo.Options{})
	first, err := tracker.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	writeTree(t, repo, map[string]string{"a.txt": "v2\n"})
	if _, err := tracker.Begin(); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	writeTree(t, repo, map[string]string{"a.txt": "v3\n", "b.txt": "v3\n"})

	// 只回退 b.txt：a.txt 保持 v3。
	out := invokeTool(t, undo.New(tracker), `{"path":"b.txt"}`)
	if !strings.Contains(out, "for path b.txt") {
		t.Fatalf("定向回退文案 = %q", out)
	}
	if got := read(t, repo, "a.txt"); got != "v3\n" {
		t.Fatalf("a.txt = %q, want untouched v3", got)
	}
	if got := read(t, repo, "b.txt"); got != "v1\n" {
		t.Fatalf("b.txt = %q, want v1", got)
	}

	// 回退到第 1 回合：a.txt 回到 v1。
	out = invokeTool(t, undo.New(tracker), `{"turn":`+itoa(first)+`}`)
	if !strings.Contains(out, "from turn "+itoa(first)) {
		t.Fatalf("回合回退文案 = %q", out)
	}
	if got := read(t, repo, "a.txt"); got != "v1\n" {
		t.Fatalf("a.txt = %q, want v1", got)
	}
}

// TestToolWithoutTracker 证明没有配置 tracker 时工具返回可读错误而不是 panic。
func TestToolWithoutTracker(t *testing.T) {
	out := invokeTool(t, undo.New(nil), `{}`)
	if !strings.Contains(out, "no snapshot tracker") {
		t.Fatalf("文案 = %q", out)
	}
}

// TestToolSchemaAndPositioning 钉住工具的 schema 与"用户面撤销按钮"的定位文案。
func TestToolSchemaAndPositioning(t *testing.T) {
	tool := undo.New(nil)
	if tool.Name() != undo.ToolName {
		t.Fatalf("Name() = %q", tool.Name())
	}
	schema := tool.Schema()
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema 没有 properties: %+v", schema)
	}
	for _, key := range []string{"turn", "dry_run", "path"} {
		if _, ok := properties[key]; !ok {
			t.Fatalf("schema 缺少 %q: %+v", key, properties)
		}
	}
	description := tool.Description()
	for _, want := range []string{"user-facing", "undo"} {
		if !strings.Contains(description, want) {
			t.Fatalf("description 缺少 %q: %q", want, description)
		}
	}
}

// itoa 是 strconv.Itoa 的小包装（避免在测试里到处 import）。
func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}
