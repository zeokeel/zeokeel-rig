package patch_test

// PreviewChanges 的行为测试：变更描述、**绝不落盘**、与 Invoke 的统计一致、
// 策略与路径安全、错误语义。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/patch"
)

// invokeApplyPatch 以 dry_run 调用 apply_patch，返回结果文本。
func invokeApplyPatch(t *testing.T, root, patchText string) (string, bool) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"patch": patchText, "dry_run": true})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	tool := patch.New(root)
	result, err := tool.Invoke(context.Background(), rig.ToolCall{
		ID: "c1", Name: patch.ToolName, Arguments: string(payload),
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	return result.Content, result.IsError
}

// writeFile 建目录并写文件。
func writeFile(t *testing.T, root, rel, content string) string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// D1：一份包含 update / add / delete / move 的补丁，PreviewChanges 必须逐项报对。
func TestPreviewChangesReportsEveryOp(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "one\ntwo\nthree\n")
	writeFile(t, root, "old.txt", "gone\n")
	writeFile(t, root, "move-me.txt", "m\n")

	patchText := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: a.txt",
		"@@",
		" one",
		"-two",
		"+TWO",
		"*** Add File: nested/new.txt",
		"+hello",
		"*** Delete File: old.txt",
		"*** Update File: move-me.txt",
		"*** Move to: moved.txt",
		"*** End Patch",
		"",
	}, "\n")

	changes, err := patch.PreviewChanges(root, patchText, nil)
	if err != nil {
		t.Fatalf("PreviewChanges: %v", err)
	}
	if len(changes) != 4 {
		t.Fatalf("变更数 = %d, want 4: %+v", len(changes), changes)
	}
	byPath := make(map[string]patch.Change, len(changes))
	for _, change := range changes {
		byPath[change.Path] = change
	}

	update, ok := byPath["a.txt"]
	if !ok || update.Op != "modify" {
		t.Fatalf("a.txt 的变更 = %+v", update)
	}
	if string(update.Old) != "one\ntwo\nthree\n" || string(update.New) != "one\nTWO\nthree\n" {
		t.Fatalf("a.txt 内容不对: old=%q new=%q", update.Old, update.New)
	}
	if update.Added != 1 || update.Removed != 1 || !update.Existed {
		t.Fatalf("a.txt 统计/存在性不对: %+v", update)
	}

	add, ok := byPath["nested/new.txt"]
	if !ok || add.Op != "add" {
		t.Fatalf("nested/new.txt 的变更 = %+v", add)
	}
	if len(add.Old) != 0 || string(add.New) != "hello\n" || add.Existed {
		t.Fatalf("新增项不对: old=%q new=%q existed=%v", add.Old, add.New, add.Existed)
	}

	del, ok := byPath["old.txt"]
	if !ok || del.Op != "delete" {
		t.Fatalf("old.txt 的变更 = %+v", del)
	}
	if string(del.Old) != "gone\n" || len(del.New) != 0 {
		t.Fatalf("删除项不对: old=%q new=%q", del.Old, del.New)
	}

	move, ok := byPath["move-me.txt"]
	if !ok || move.MoveTo != "moved.txt" {
		t.Fatalf("移动项不对: %+v", move)
	}
}

// D2【核心承诺】：预览**一个字节都不落盘**（不建文件、不建目录、不改内容）。
func TestPreviewChangesWritesNothing(t *testing.T) {
	root := t.TempDir()
	target := writeFile(t, root, "a.txt", "one\n")
	patchText := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: a.txt",
		"@@",
		"-one",
		"+ONE",
		"*** Add File: deep/dir/new.txt",
		"+x",
		"*** End Patch",
		"",
	}, "\n")

	if _, err := patch.PreviewChanges(root, patchText, nil); err != nil {
		t.Fatalf("PreviewChanges: %v", err)
	}

	if _, err := os.Stat(filepath.Join(root, "deep")); !os.IsNotExist(err) {
		t.Fatalf("预览创建了目录（应当零副作用）: %v", err)
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(raw) != "one\n" {
		t.Fatalf("预览改写了文件: %q", raw)
	}
	// 目录条目数也不能变。
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("根目录多了条目: %v", entries)
	}
}

// D3：预览的 +N/-M 必须与 Invoke(dry_run) 完全一致——两边走同一份计算。
func TestPreviewChangesStatsMatchInvoke(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "1\n2\n3\n4\n")
	patchText := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: a.txt",
		"@@",
		" 1",
		"-2",
		"-3",
		"+two",
		"+three",
		"+four",
		" 4",
		"*** End Patch",
		"",
	}, "\n")

	changes, err := patch.PreviewChanges(root, patchText, nil)
	if err != nil {
		t.Fatalf("PreviewChanges: %v", err)
	}
	var added, removed int
	for _, change := range changes {
		added += change.Added
		removed += change.Removed
	}

	content, isError := invokeApplyPatch(t, root, patchText)
	if isError {
		t.Fatalf("dry-run 报错: %s", content)
	}
	want := "+" + strconv.Itoa(added) + " -" + strconv.Itoa(removed)
	if !strings.Contains(content, want) {
		t.Fatalf("dry-run 报告 %q 里没有 %q", content, want)
	}
	if !strings.Contains(content, "no files were modified") {
		t.Fatalf("dry-run 应当明说没改文件: %q", content)
	}
}

// D4：非法补丁返回错误，且明确是解析/校验阶段（不落盘）。
func TestPreviewChangesRejectsBadPatch(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "one\n")

	cases := []struct {
		name  string
		patch string
	}{
		{"空补丁", "   "},
		{"语法错", "*** Begin Patch\n*** Update File: a.txt\n@@\n-nope\n+yes\n*** End Patch\n"},
		{"上下文不匹配", "*** Begin Patch\n*** Update File: a.txt\n@@\n-missing\n+x\n*** End Patch\n"},
		{"未知文件", "*** Begin Patch\n*** Update File: nope.txt\n@@\n-a\n+b\n*** End Patch\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := patch.PreviewChanges(root, tc.patch, nil); err == nil {
				t.Fatalf("应当报错，实际成功")
			}
			raw, err := os.ReadFile(filepath.Join(root, "a.txt"))
			if err != nil || string(raw) != "one\n" {
				t.Fatalf("失败路径改动了文件: %q (%v)", raw, err)
			}
		})
	}
}

// D5：只读策略下预览也必须拒绝（预览不是绕过策略的后门）。
func TestPreviewChangesRespectsReadOnlyPolicy(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "one\n")
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeReadOnly))

	patchText := "*** Begin Patch\n*** Update File: a.txt\n@@\n-one\n+two\n*** End Patch\n"
	if _, err := patch.PreviewChanges(root, patchText, policy); err == nil {
		t.Fatal("只读策略下预览应当报错（否则审批界面会承诺一次注定失败的写入）")
	}
}

// D6：路径逃逸必须被拒绝。
func TestPreviewChangesRejectsEscapingPath(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeFile(t, outside, "secret.txt", "secret\n")

	for _, bad := range []string{"../secret.txt", outside + "/secret.txt", "a/../../x.txt"} {
		patchText := "*** Begin Patch\n*** Add File: " + bad + "\n+x\n*** End Patch\n"
		if _, err := patch.PreviewChanges(root, patchText, nil); err == nil {
			t.Fatalf("路径 %q 应当被拒绝", bad)
		}
	}
}

// D7：超大补丁被拒绝（与 Invoke 同一个上限）。
func TestPreviewChangesRejectsOversizePatch(t *testing.T) {
	root := t.TempDir()
	huge := strings.Repeat("x", patch.DefaultMaxPatchBytes+1)
	if _, err := patch.PreviewChanges(root, huge, nil); err == nil {
		t.Fatal("超大补丁应当被拒绝")
	}
}
