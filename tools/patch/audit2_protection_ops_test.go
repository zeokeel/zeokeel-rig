package patch_test

// 全项目对抗审计 第 1 轮 —— 保护路径的**补充**覆盖（DSH 对齐后已停用强制）。
//
// 旧版本补三块：Add/Update 直接命中保护文件、失败时的"全有或全无"、含 `..` 的路径。
// 策略层没有保护路径这一层（相关 API 已删除），所以：
//   1. Add/Update 命中 `.git/**` 现在**必须成功**（反向用例）；
//   2. "全有或全无"改用**真正会失败的补丁**（第二个文件 hunk 不匹配）验证，不再拿
//      保护路径当失败注入点；
//   3. 含 `..` 的路径行为保持不变（apply_patch 词法归一化，write_file 直接拒绝）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/patch"
)

// auditPatchRoot 造一个带 .git 目录的普通 workspace-write 工具。
//
// 刻意不调用 AddProtectedPaths（已删除；策略层没有保护路径这一层）。
func auditPatchRoot(t *testing.T) (root string, tool rig.Tool) {
	t.Helper()
	root = t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "config"), []byte("[core]\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "ok.txt"), []byte("ok\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	return root, patch.New(root, patch.WithPolicy(policy))
}

// TestAuditApplyPatchAddAndUpdateFormerlyProtectedFile 是旧用例
// TestAuditApplyPatchAddAndUpdateProtectedFile 的反向版本（DSH 对齐：有意变更）：
// Add/Update 命中 `.git/**` 现在必须成功并且真的落盘。
func TestAuditApplyPatchAddAndUpdateFormerlyProtectedFile(t *testing.T) {
	cases := []struct {
		name   string
		patch  string
		target string
		want   string
	}{
		{"Add 命中曾受保护目录", "*** Begin Patch\n*** Add File: .git/added.txt\n+legit\n*** End Patch\n", ".git/added.txt", "legit\n"},
		{"Update 命中曾受保护文件",
			"*** Begin Patch\n*** Update File: .git/config\n@@\n-[core]\n+[legit]\n*** End Patch\n", ".git/config", "[legit]\n"},
		{"Add 深层曾受保护路径", "*** Begin Patch\n*** Add File: .git/refs/heads/main\n+legit\n*** End Patch\n", ".git/refs/heads/main", "legit\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, tool := auditPatchRoot(t)
			content, isError := invokePatch(t, tool, tc.patch)
			if isError {
				t.Fatalf("%s 被拒绝（保护已停用）: %q", tc.name, content)
			}
			raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(tc.target)))
			if err != nil || string(raw) != tc.want {
				t.Fatalf("%s = %q (%v), want %q", tc.target, raw, err, tc.want)
			}
		})
	}
}

// TestAuditApplyPatchIsAllOrNothingOnHunkMismatch：同一补丁里第一个文件可改、
// 第二个文件的 hunk 对不上 → 整体失败，第一个文件**零改动**。
//
// 旧用例用"保护路径"当失败注入点；DSH 对齐后保护不再拦，所以改用真正会失败的
// 补丁（hunk 不匹配）来验证"全有或全无"。
func TestAuditApplyPatchIsAllOrNothingOnHunkMismatch(t *testing.T) {
	root, tool := auditPatchRoot(t)
	if err := os.WriteFile(filepath.Join(root, "other.txt"), []byte("actual content\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	patchText := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: ok.txt",
		"@@",
		"-ok",
		"+updated",
		"*** Update File: other.txt",
		"@@",
		"-this line does not exist",
		"+nope",
		"*** End Patch",
		"",
	}, "\n")
	content, isError := invokePatch(t, tool, patchText)
	if !isError {
		t.Fatalf("hunk 不匹配的补丁整体应当失败: %q", content)
	}
	if raw, err := os.ReadFile(filepath.Join(root, "ok.txt")); err != nil || string(raw) != "ok\n" {
		t.Fatalf("全有或全无被打破：第一个文件被改了 %q (%v)", raw, err)
	}
	if raw, err := os.ReadFile(filepath.Join(root, "other.txt")); err != nil || string(raw) != "actual content\n" {
		t.Fatalf("第二个文件被改了 %q (%v)", raw, err)
	}
}

// TestAuditApplyPatchTreatsDotDotDifferentlyFromWriteFile【一致性发现】：
//
// write_file 对**任何**含 `..` 的路径报 "contains a parent-directory component"（见
// tools/builtin/audit_protection_test.go），而 apply_patch 的应用式补丁解析器先用
// filepath.Clean 把 `..` 词法归一化掉，于是 `.git/../escape.txt` 变成了
// `escape.txt` 并被**成功创建**——模型以为写的是 `.git/../escape.txt`，实际落点是
// `escape.txt`。
//
// 这不是越权（Clean 之后仍在根内，且后续仍经策略与 os.Root 解析），但两个写工具对
// 同一输入给出完全不同结论，属于"实现不一致"，建议统一为拒绝。
func TestAuditApplyPatchCleansDotDotInsteadOfRejecting(t *testing.T) {
	root, tool := auditPatchRoot(t)

	// (a) `sub/../ok.txt` 被归一化成 `ok.txt`：于是撞上"已存在"。
	content, isError := invokePatch(t, tool, "*** Begin Patch\n*** Add File: sub/../ok.txt\n+x\n*** End Patch\n")
	if !isError {
		t.Fatalf("`sub/../ok.txt` 竟然被接受: %q", content)
	}
	if !strings.Contains(content, "ok.txt") {
		t.Fatalf("可见路径没有归一化（错误文案 %q）", content)
	}

	// (b) `.git/../escape.txt` → 归一化成 `escape.txt` 并真的创建了。
	content, isError = invokePatch(t, tool, "*** Begin Patch\n*** Add File: .git/../escape.txt\n+x\n*** End Patch\n")
	if isError {
		t.Logf("含 .. 的路径被拒绝（与 write_file 一致，更安全）: %s", content)
		return
	}
	if _, err := os.Stat(filepath.Join(root, "escape.txt")); err != nil {
		t.Fatalf("归一化后的文件没被创建: %v", err)
	}
	t.Logf("⚠️ 一致性：apply_patch 把 `.git/../escape.txt` 归一化成 `escape.txt` 并创建，" +
		"而 write_file 对同一路径直接报 parent-directory component。建议统一为拒绝。")
}
