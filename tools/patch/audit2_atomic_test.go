package patch_test

// 第 2D 轮对抗审计（只读）：apply_patch 的原子性与边界文件形态。
//
// 覆盖：
//   - 多文件中途失败 → 全部回滚、**不留临时文件**；
//   - CRLF 文件打补丁后行尾不变；
//   - 无尾换行文件；
//   - 二进制文件（应拒绝）；
//   - 同一补丁里 "add + update 同一文件" 的冲突检测；
//   - `Move to` 跨目录。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/tools/patch"
)

// snapshotTree 返回"相对路径 → 内容"的完整树快照（含隐藏文件）。
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	tree := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		tree[filepath.ToSlash(relative)] = string(raw)
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
	return tree
}

// diffTrees 返回两个树快照的差异（用于断言"零改动"）。
func diffTrees(before, after map[string]string) []string {
	var problems []string
	for path, content := range before {
		got, ok := after[path]
		if !ok {
			problems = append(problems, "消失: "+path)
			continue
		}
		if got != content {
			problems = append(problems, "内容变了: "+path)
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			problems = append(problems, "新增: "+path)
		}
	}
	return problems
}

// invokePatchText 用不带策略的工具执行补丁。
func invokePatchText(t *testing.T, root, patchText string) (string, bool) {
	t.Helper()
	return invokePatch(t, patch.New(root), patchText)
}

// 1【核心】多文件中途失败 → 全部回滚，且不留临时文件。
func TestAuditAtomicRollbackLeavesNoResidue(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "alpha\n")
	writeFile(t, root, "b.txt", "beta\n")
	before := snapshotTree(t, root)

	// 第一个文件能成、第二个文件的上下文匹配不到 → 整体失败。
	patchText := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: a.txt",
		"@@",
		"-alpha",
		"+ALPHA",
		"*** Update File: b.txt",
		"@@",
		"-does-not-exist",
		"+nope",
		"*** End Patch",
		"",
	}, "\n")
	content, isError := invokePatchText(t, root, patchText)
	if !isError {
		t.Fatalf("应当整体失败: %q", content)
	}
	after := snapshotTree(t, root)
	if problems := diffTrees(before, after); len(problems) > 0 {
		t.Fatalf("失败后工作区被改了: %v", problems)
	}
	// 目录里不能出现任何临时文件（.tmp / 点开头 / 随机名）。
	for path := range after {
		base := filepath.Base(path)
		if strings.HasPrefix(base, ".") || strings.Contains(base, ".tmp") || strings.Contains(base, "~") {
			t.Fatalf("残留临时文件: %s", path)
		}
	}
}

// 2：CRLF 文件打补丁后行尾仍是 CRLF（不能悄悄变成 LF）。
func TestAuditPatchPreservesCRLF(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "crlf.txt", "line1\r\nline2\r\nline3\r\n")

	patchText := "*** Begin Patch\n*** Update File: crlf.txt\n@@\n line1\n-line2\n+LINE2\n line3\n*** End Patch\n"
	content, isError := invokePatchText(t, root, patchText)
	if isError {
		t.Fatalf("CRLF 补丁失败: %q", content)
	}
	raw, err := os.ReadFile(filepath.Join(root, "crlf.txt"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	want := "line1\r\nLINE2\r\nline3\r\n"
	if string(raw) != want {
		t.Fatalf("CRLF 行尾被破坏:\n got %q\nwant %q", raw, want)
	}
}

// 3：无尾换行的文件——打完补丁仍然没有尾换行（不能凭空加一个）。
func TestAuditPatchPreservesMissingTrailingNewline(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "eof.txt", "one\ntwo")

	patchText := "*** Begin Patch\n*** Update File: eof.txt\n@@\n one\n-two\n+TWO\n*** End Patch\n"
	content, isError := invokePatchText(t, root, patchText)
	if isError {
		t.Fatalf("补丁失败: %q", content)
	}
	raw, err := os.ReadFile(filepath.Join(root, "eof.txt"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(raw) != "one\nTWO" {
		t.Fatalf("无尾换行的文件被改写:\n got %q\nwant %q", raw, "one\nTWO")
	}
}

// 4：二进制文件（含 NUL）必须被拒绝，且不改动内容。
func TestAuditPatchRejectsBinaryFile(t *testing.T) {
	root := t.TempDir()
	binary := "abc\x00def\n"
	writeFile(t, root, "data.bin", binary)

	patchText := "*** Begin Patch\n*** Update File: data.bin\n@@\n-abc\n+xyz\n*** End Patch\n"
	content, isError := invokePatchText(t, root, patchText)
	if !isError {
		t.Fatalf("二进制文件竟然可以打补丁: %q", content)
	}
	raw, err := os.ReadFile(filepath.Join(root, "data.bin"))
	if err != nil || string(raw) != binary {
		t.Fatalf("二进制文件被改了: %q (%v)", raw, err)
	}
	t.Logf("二进制拒绝文案: %s", strings.TrimSpace(content))
}

// 5：同一补丁里 "add + update 同一文件" → 必须在碰磁盘之前就拒绝。
func TestAuditPatchRejectsAddAndUpdateSameFile(t *testing.T) {
	root := t.TempDir()
	before := snapshotTree(t, root)

	patchText := strings.Join([]string{
		"*** Begin Patch",
		"*** Add File: dup.txt",
		"+created",
		"*** Update File: dup.txt",
		"@@",
		"-created",
		"+updated",
		"*** End Patch",
		"",
	}, "\n")
	content, isError := invokePatchText(t, root, patchText)
	if !isError {
		t.Fatalf("同一文件既 add 又 update 竟然成功: %q", content)
	}
	if problems := diffTrees(before, snapshotTree(t, root)); len(problems) > 0 {
		t.Fatalf("冲突补丁留下了改动: %v", problems)
	}
	t.Logf("冲突检测文案: %s", strings.TrimSpace(content))
}

// 6：`Move to` 跨目录（含目标目录不存在 → 自动创建；纯移动不改内容）。
func TestAuditPatchMoveAcrossDirectories(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "src/deep/file.txt", "payload\n")

	patchText := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: src/deep/file.txt",
		"*** Move to: other/place/moved.txt",
		"*** End Patch",
		"",
	}, "\n")
	content, isError := invokePatchText(t, root, patchText)
	if isError {
		t.Fatalf("跨目录移动失败: %q", content)
	}
	if _, err := os.Stat(filepath.Join(root, "src", "deep", "file.txt")); !os.IsNotExist(err) {
		t.Fatalf("源文件还在: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "other", "place", "moved.txt"))
	if err != nil || string(raw) != "payload\n" {
		t.Fatalf("目标文件内容不对: %q (%v)", raw, err)
	}
	// 源目录不能被顺手删掉（只移动文件）。
	if _, err := os.Stat(filepath.Join(root, "src", "deep")); err != nil {
		t.Fatalf("源目录被删了: %v", err)
	}
}
