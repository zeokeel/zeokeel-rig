package undo_test

// Tracker 的回合管理契约：回合号单调、turn<=0 表示最近一次、保留上限丢最老、
// 并发安全，以及工具的 schema/结果文案。

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/undo"
)

// --- Tracker --------------------------------------------------------------

// TestTrackerBeginSequentialTurns 证明回合号从 1 开始单调递增。
func TestTrackerBeginSequentialTurns(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.txt": "a\n"})
	tracker := undo.NewTracker(root, undo.Options{})
	for want := 1; want <= 3; want++ {
		got, err := tracker.Begin()
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if got != want {
			t.Fatalf("Begin() = %d, want %d", got, want)
		}
	}
	if got := tracker.Turns(); len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Fatalf("Turns() = %v, want [1 2 3]", got)
	}
}

// TestTrackerRevertLatestWhenTurnIsZero 证明 turn<=0 回退最近一次快照。
func TestTrackerRevertLatestWhenTurnIsZero(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.txt": "a\n"})
	tracker := undo.NewTracker(root, undo.Options{})
	if _, err := tracker.Begin(); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	writeTree(t, root, map[string]string{"a.txt": "a2\n"})
	if _, err := tracker.Begin(); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	writeTree(t, root, map[string]string{"a.txt": "a3\n"})

	for _, turn := range []int{0, -5} {
		changes, err := tracker.Revert(turn)
		if err != nil {
			t.Fatalf("Revert(%d): %v", turn, err)
		}
		if len(changes) != 1 || changes[0].Path != "a.txt" {
			t.Fatalf("Revert(%d) = %+v, want a.txt reverted to turn 2", turn, changes)
		}
		if got := read(t, filepath.Join(root, "a.txt")); got != "a2\n" {
			t.Fatalf("content = %q, want the turn-2 state %q", got, "a2\n")
		}
		writeTree(t, root, map[string]string{"a.txt": "a3\n"})
	}
}

// TestTrackerRevertSpecificTurn 证明可以回到更早的回合：两个回合的改动一起回退。
func TestTrackerRevertSpecificTurn(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.txt": "a\n", "b.txt": "b\n"})
	tracker := undo.NewTracker(root, undo.Options{})
	if _, err := tracker.Begin(); err != nil { // turn 1
		t.Fatalf("Begin: %v", err)
	}
	writeTree(t, root, map[string]string{"a.txt": "a2\n"})
	if _, err := tracker.Begin(); err != nil { // turn 2
		t.Fatalf("Begin: %v", err)
	}
	writeTree(t, root, map[string]string{"b.txt": "b2\n"})

	changes, err := tracker.Revert(1)
	if err != nil {
		t.Fatalf("Revert(1): %v", err)
	}
	paths := changePaths(changes)
	if len(paths) != 2 || paths[0] != "a.txt" || paths[1] != "b.txt" {
		t.Fatalf("paths = %v, want [a.txt b.txt]", paths)
	}
	if got := read(t, filepath.Join(root, "a.txt")); got != "a\n" {
		t.Fatalf("a.txt = %q, want the turn-1 state", got)
	}
	if got := read(t, filepath.Join(root, "b.txt")); got != "b\n" {
		t.Fatalf("b.txt = %q, want the turn-1 state", got)
	}
}

// TestTrackerRetentionDropsOldest 证明超过保留上限时丢弃最老快照。
func TestTrackerRetentionDropsOldest(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.txt": "a\n"})
	tracker := undo.NewTracker(root, undo.Options{Retain: 2})
	for turn := 1; turn <= 4; turn++ {
		got, err := tracker.Begin()
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if got != turn {
			t.Fatalf("Begin() = %d, want %d", got, turn)
		}
	}
	if got := tracker.Turns(); len(got) != 2 || got[0] != 3 || got[1] != 4 {
		t.Fatalf("Turns() = %v, want [3 4] after retention", got)
	}
	if _, err := tracker.Revert(1); err == nil {
		t.Fatal("Revert accepted a dropped turn")
	}
}

// TestTrackerTurnsIsCopy 证明 Turns 返回副本（调用方改动不影响内部状态）。
func TestTrackerTurnsIsCopy(t *testing.T) {
	root := t.TempDir()
	tracker := undo.NewTracker(root, undo.Options{})
	if _, err := tracker.Begin(); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	turns := tracker.Turns()
	turns[0] = 99
	if got := tracker.Turns(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("Turns() = %v, want [1] after mutating a copy", got)
	}
}

// TestTrackerUnknownTurnIsError 证明未知回合号报错并列出可用回合。
func TestTrackerUnknownTurnIsError(t *testing.T) {
	root := t.TempDir()
	tracker := undo.NewTracker(root, undo.Options{})
	if _, err := tracker.Begin(); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	_, err := tracker.Revert(7)
	if err == nil {
		t.Fatal("Revert accepted an unknown turn")
	}
	if !strings.Contains(err.Error(), "7") {
		t.Fatalf("error = %v, want it to name the requested turn", err)
	}
}

// TestTrackerConcurrentBeginAndRevert 证明并发 Begin/Revert 不 panic、不返回错误
// （配合 -race 检查数据竞争）。
func TestTrackerConcurrentBeginAndRevert(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.txt": "a\n"})
	tracker := undo.NewTracker(root, undo.Options{Retain: 3})

	const workers = 4
	const rounds = 5
	var wait sync.WaitGroup
	errs := make(chan error, workers*rounds*2)
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func(id int) {
			defer wait.Done()
			for round := 0; round < rounds; round++ {
				if _, err := tracker.Begin(); err != nil {
					errs <- err
					return
				}
				if _, err := tracker.Revert(0); err != nil {
					errs <- err
					return
				}
				_ = tracker.Turns()
			}
		}(worker)
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent tracker operation failed: %v", err)
	}
}

// --- 工具 -----------------------------------------------------------------

// invoke 调用工具并要求没有 Go 错误。
func invoke(t *testing.T, tool rig.Tool, arguments string) rig.ToolResult {
	t.Helper()
	result, err := tool.Invoke(t.Context(), rig.ToolCall{ID: "call-1", Name: undo.ToolName, Arguments: arguments})
	if err != nil {
		t.Fatalf("undo.Invoke returned Go error: %v", err)
	}
	return result
}

// TestToolNameAndSchema 钉住工具名与 schema。
func TestToolNameAndSchema(t *testing.T) {
	tool := undo.New(undo.NewTracker(t.TempDir(), undo.Options{}))
	if got := tool.Name(); got != "undo" {
		t.Fatalf("Name() = %q, want undo", got)
	}
	schema := tool.Schema()
	if schema["type"] != "object" {
		t.Fatalf("schema type = %v, want object", schema["type"])
	}
	if got := schema["additionalProperties"]; got != false {
		t.Fatalf("additionalProperties = %v, want false", got)
	}
	if _, ok := schema["required"]; ok {
		t.Fatalf("schema requires %v, want no required fields", schema["required"])
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties = %#v, want an object", schema["properties"])
	}
	for name, wantType := range map[string]string{"turn": "integer", "dry_run": "boolean", "path": "string"} {
		property, ok := properties[name].(map[string]any)
		if !ok {
			t.Fatalf("property %q = %#v, want an object", name, properties[name])
		}
		if property["type"] != wantType {
			t.Fatalf("property %q type = %v, want %s", name, property["type"], wantType)
		}
		if description, _ := property["description"].(string); description == "" {
			t.Fatalf("property %q has no description", name)
		}
	}
	if len(properties) != 3 {
		t.Fatalf("properties = %v, want exactly turn/dry_run/path", properties)
	}
}

// TestToolNilTracker 证明未配置 Tracker 时返回可读错误而不是 panic。
func TestToolNilTracker(t *testing.T) {
	result := invoke(t, undo.New(nil), `{}`)
	if !result.IsError {
		t.Fatalf("result = %q, want IsError", result.Content)
	}
	if !strings.Contains(result.Content, "undo:") {
		t.Fatalf("result = %q, want an undo: error", result.Content)
	}
}

// TestToolRevertsLatestTurn 钉住默认（turn=0）回退最近回合的结果文案。
func TestToolRevertsLatestTurn(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.txt": "a\n"})
	tracker := undo.NewTracker(root, undo.Options{})
	if _, err := tracker.Begin(); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	writeTree(t, root, map[string]string{"a.txt": "a2\n"})

	result := invoke(t, undo.New(tracker), `{}`)
	if result.IsError {
		t.Fatalf("result = %q, want success", result.Content)
	}
	if want := "undo: reverted 1 change(s) from turn 1\n  M a.txt (+1/-1)\n"; result.Content != want {
		t.Fatalf("content = %q, want %q", result.Content, want)
	}
	if got := read(t, filepath.Join(root, "a.txt")); got != "a\n" {
		t.Fatalf("a.txt = %q, want reverted", got)
	}
}

// TestToolDryRunDoesNotWrite 证明 dry_run 只报告不落盘。
func TestToolDryRunDoesNotWrite(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.txt": "a\n"})
	tracker := undo.NewTracker(root, undo.Options{})
	if _, err := tracker.Begin(); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	writeTree(t, root, map[string]string{"a.txt": "a2\n"})

	result := invoke(t, undo.New(tracker), `{"dry_run":true}`)
	if result.IsError {
		t.Fatalf("result = %q, want success", result.Content)
	}
	want := "undo: would revert 1 change(s) from turn 1 (dry run; nothing was written)\n  M a.txt (+1/-1)\n"
	if result.Content != want {
		t.Fatalf("content = %q, want %q", result.Content, want)
	}
	if got := read(t, filepath.Join(root, "a.txt")); got != "a2\n" {
		t.Fatalf("a.txt = %q, want dry_run to leave it modified", got)
	}
}

// TestToolNoChanges 证明没有变更时的结果文案。
func TestToolNoChanges(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.txt": "a\n"})
	tracker := undo.NewTracker(root, undo.Options{})
	if _, err := tracker.Begin(); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if result := invoke(t, undo.New(tracker), `{}`); result.Content != "undo: no changes from turn 1\n" {
		t.Fatalf("content = %q, want the no-changes line", result.Content)
	}
	if result := invoke(t, undo.New(tracker), `{"dry_run":true}`); result.Content != "undo: no changes from turn 1 (dry run; nothing was written)\n" {
		t.Fatalf("content = %q, want the dry-run no-changes line", result.Content)
	}
}

// TestToolTurnSelection 证明 turn 参数选择具体回合。
func TestToolTurnSelection(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.txt": "a\n"})
	tracker := undo.NewTracker(root, undo.Options{})
	if _, err := tracker.Begin(); err != nil { // turn 1
		t.Fatalf("Begin: %v", err)
	}
	writeTree(t, root, map[string]string{"a.txt": "a2\n"})
	if _, err := tracker.Begin(); err != nil { // turn 2
		t.Fatalf("Begin: %v", err)
	}
	writeTree(t, root, map[string]string{"a.txt": "a3\n"})

	result := invoke(t, undo.New(tracker), `{"turn":1}`)
	if result.IsError {
		t.Fatalf("result = %q, want success", result.Content)
	}
	if got := read(t, filepath.Join(root, "a.txt")); got != "a\n" {
		t.Fatalf("a.txt = %q, want the turn-1 state", got)
	}
	if !strings.Contains(result.Content, "from turn 1") {
		t.Fatalf("content = %q, want it to name turn 1", result.Content)
	}
}

// TestToolPathFilter 证明 path 只回退指定路径，并在文案里注明。
func TestToolPathFilter(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.txt": "a\n", "b.txt": "b\n"})
	tracker := undo.NewTracker(root, undo.Options{})
	if _, err := tracker.Begin(); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	writeTree(t, root, map[string]string{"a.txt": "a2\n", "b.txt": "b2\n"})

	result := invoke(t, undo.New(tracker), `{"path":"a.txt"}`)
	if result.IsError {
		t.Fatalf("result = %q, want success", result.Content)
	}
	want := "undo: reverted 1 change(s) from turn 1 for path a.txt\n  M a.txt (+1/-1)\n"
	if result.Content != want {
		t.Fatalf("content = %q, want %q", result.Content, want)
	}
	if got := read(t, filepath.Join(root, "a.txt")); got != "a\n" {
		t.Fatalf("a.txt = %q, want reverted", got)
	}
	if got := read(t, filepath.Join(root, "b.txt")); got != "b2\n" {
		t.Fatalf("b.txt = %q, want untouched", got)
	}
}

// TestToolRejectsEscapingPaths 证明绝对路径、".." 与符号链接逃逸都被拒绝。
func TestToolRejectsEscapingPaths(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeTree(t, root, map[string]string{"a.txt": "a\n"})
	writeTree(t, outside, map[string]string{"secret.txt": "s\n"})
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	tracker := undo.NewTracker(root, undo.Options{})
	if _, err := tracker.Begin(); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	tool := undo.New(tracker)

	cases := map[string]string{
		"absolute":         `{"path":"/etc/passwd"}`,
		"windows-absolute": `{"path":"C:\\Windows\\system32\\drivers\\etc\\hosts"}`,
		"dotdot":           `{"path":"../outside.txt"}`,
		"symlink-escape":   `{"path":"escape/secret.txt"}`,
	}
	for name, arguments := range cases {
		t.Run(name, func(t *testing.T) {
			result := invoke(t, tool, arguments)
			if !result.IsError {
				t.Fatalf("result = %q, want IsError for an escaping path", result.Content)
			}
			if !strings.Contains(result.Content, "undo:") {
				t.Fatalf("result = %q, want an undo: error", result.Content)
			}
		})
	}
}

// TestToolUnknownPathIsError 证明过滤不存在的路径会报错（而不是静默成功）。
func TestToolUnknownPathIsError(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.txt": "a\n"})
	tracker := undo.NewTracker(root, undo.Options{})
	if _, err := tracker.Begin(); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	result := invoke(t, undo.New(tracker), `{"path":"missing.txt"}`)
	if !result.IsError {
		t.Fatalf("result = %q, want IsError", result.Content)
	}
}

// TestToolReportsSkippedChangesWithWarning 证明不可回退的变更会在结果里明确汇总，
// 而不是静默掠过。
func TestToolReportsSkippedChangesWithWarning(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"blob.bin": "head\x00tail"})
	tracker := undo.NewTracker(root, undo.Options{})
	if _, err := tracker.Begin(); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	writeTree(t, root, map[string]string{"blob.bin": "head\x00tail2"})

	result := invoke(t, undo.New(tracker), `{}`)
	if result.IsError {
		t.Fatalf("result = %q, want a successful result carrying a warning", result.Content)
	}
	want := "undo: reverted 0 change(s) from turn 1, skipped 1\n" +
		"warning: 1 change(s) were not backed up and were left as-is:\n" +
		"  ! blob.bin: not-backed-up: binary file (contains NUL)\n"
	if result.Content != want {
		t.Fatalf("content = %q, want %q", result.Content, want)
	}
	if got := read(t, filepath.Join(root, "blob.bin")); got != "head\x00tail2" {
		t.Fatalf("blob.bin = %q, want it left as-is", got)
	}
}
