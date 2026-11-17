package patch_test

// A6 + A7：补丁路径的形态约束（与 write_file/edit_file 对齐）。
//
//   - A6：非 Windows 上路径里的 "\" 不是分隔符 → 拒绝（不能落盘怪名字文件）；
//   - A7：任何含 ".." 分量的路径 → 拒绝。write_file 已经一律拒 ".."，而 apply_patch
//     之前先 Clean，`.git/../escape.txt` 会变成 `escape.txt` 并被创建 —— 两个写工具
//     行为相反，模型会学错。一致性 > 灵活性。

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// assertNothingCreated 断言工作区里没有出现新文件（只允许原有的那些）。
func assertNothingCreated(t *testing.T, f *fixture, allowed ...string) {
	t.Helper()
	wanted := map[string]bool{}
	for _, name := range allowed {
		wanted[name] = true
	}
	err := filepath.WalkDir(f.root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, relErr := filepath.Rel(f.root, path)
		if relErr != nil {
			return relErr
		}
		relative = filepath.ToSlash(relative)
		if !wanted[relative] {
			t.Errorf("意外创建了文件: %q（允许: %v）", relative, allowed)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

// TestPatchRejectsBackslashPaths 覆盖 A6 的三个入口（Update/Add/Delete + Move 目标）。
func TestPatchRejectsBackslashPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上 \\ 是合法分隔符")
	}
	f := newFixture(t)
	f.write(t, ".git/config", "orig\n")
	f.write(t, "a.txt", "a\n")
	patches := []string{
		"*** Begin Patch\n*** Update File: .git\\config\n@@\n-orig\n+x\n*** End Patch\n",
		"*** Begin Patch\n*** Add File: dir\\new.txt\n+x\n*** End Patch\n",
		// 反斜杠出现在 Delete 行同样拒绝（Delete 也是写操作）。
		"*** Begin Patch\n*** Delete File: a\\b.txt\n*** End Patch\n",
		"*** Begin Patch\n*** Update File: a.txt\n*** Move to: dir\\moved.txt\n*** End Patch\n",
	}
	for _, patchText := range patches {
		result := f.invoke(t, patchText)
		if !result.IsError {
			t.Fatalf("含反斜杠的补丁没有被拒绝: %s", result.Content)
		}
		lowered := strings.ToLower(result.Content)
		if !strings.Contains(lowered, "backslash") && !strings.Contains(lowered, "separator") {
			t.Fatalf("错误文案没说清分隔符问题: %q", result.Content)
		}
	}
	assertNothingCreated(t, f, ".git/config", "a.txt")
	if got := f.read(t, ".git/config"); got != "orig\n" {
		t.Fatalf(".git/config 被改写: %q", got)
	}
}

// TestPatchRejectsDotDotSegments 覆盖 A7 的三类 ".." 形态。
func TestPatchRejectsDotDotSegments(t *testing.T) {
	targets := []string{
		".git/../escape.txt",
		"dir/../../escape.txt",
		"./a/../b.txt",
		"a/..",
	}
	for _, target := range targets {
		f := newFixture(t)
		f.write(t, ".git/config", "orig\n")
		// Update（不存在则会在 Add 分支创建）
		result := f.invoke(t, "*** Begin Patch\n*** Add File: "+target+"\n+x\n*** End Patch\n")
		if !result.IsError {
			t.Fatalf("Add File %q 没有被拒绝: %s", target, result.Content)
		}
		if !strings.Contains(result.Content, "..") {
			t.Fatalf("错误文案没提到 .. : %q", result.Content)
		}
		// Update 同一形态也要拒。
		result = f.invoke(t, "*** Begin Patch\n*** Update File: "+target+"\n@@\n-x\n+y\n*** End Patch\n")
		if !result.IsError {
			t.Fatalf("Update File %q 没有被拒绝: %s", target, result.Content)
		}
		assertNothingCreated(t, f, ".git/config")
	}

	// 对照组："." 与普通相对路径仍然可用（Clean 之后等价）。
	f := newFixture(t)
	f.write(t, "a/b.txt", "x\n")
	result := f.invoke(t, "*** Begin Patch\n*** Update File: ./a/b.txt\n@@\n-x\n+y\n*** End Patch\n")
	requireOK(t, result)
	if got := f.read(t, "a/b.txt"); got != "y\n" {
		t.Fatalf("a/b.txt = %q", got)
	}
}
