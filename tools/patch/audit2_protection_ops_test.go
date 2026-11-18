package patch_test

// 全项目对抗审计 第 1 轮 —— 保护路径的**补充**覆盖。
//
// 已有的 audit_protection_test.go 覆盖了目录链接 / 末段链接 / Delete / Move out。
// 本文件补三块它没覆盖的：
//   1. Add 与 Update 直接命中保护文件（含深层路径）；
//   2. 失败时的"全有或全无"（同一补丁里既有保护路径又有普通文件）；
//   3. 含 `..` 的路径（sandbox 一律拒绝，不只是逃逸）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/patch"
)

// auditPatchRoot 造一个带 .git 保护的工作区与工具。
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
	policy.AddProtectedPaths(filepath.Join(root, ".git"))
	return root, patch.New(root, patch.WithPolicy(policy))
}

// TestAuditApplyPatchAddAndUpdateProtectedFile 覆盖 Add/Update 命中保护文件。
func TestAuditApplyPatchAddAndUpdateProtectedFile(t *testing.T) {
	cases := []struct {
		name  string
		patch string
	}{
		{"Add 命中保护文件", "*** Begin Patch\n*** Add File: .git/config\n+pwned\n*** End Patch\n"},
		{"Update 命中保护文件",
			"*** Begin Patch\n*** Update File: .git/config\n@@\n-[core]\n+[pwned]\n*** End Patch\n"},
		{"Add 深层保护路径", "*** Begin Patch\n*** Add File: .git/refs/heads/main\n+pwned\n*** End Patch\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, tool := auditPatchRoot(t)
			content, isError := invokePatch(t, tool, tc.patch)
			if !isError {
				t.Fatalf("%s 竟然成功: %q", tc.name, content)
			}
			raw, err := os.ReadFile(filepath.Join(root, ".git", "config"))
			if err != nil || string(raw) != "[core]\n" {
				t.Fatalf("保护文件被改写: %q (%v)", raw, err)
			}
		})
	}
}

// TestAuditApplyPatchIsAllOrNothingWithProtectedPath：同一补丁里既有普通文件又有
// 保护路径 → 普通文件也不能被改（否则"零改动"承诺就破了）。
func TestAuditApplyPatchIsAllOrNothingWithProtectedPath(t *testing.T) {
	root, tool := auditPatchRoot(t)

	patchText := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: ok.txt",
		"@@",
		"-ok",
		"+updated",
		"*** Update File: .git/config",
		"@@",
		"-[core]",
		"+[pwned]",
		"*** End Patch",
		"",
	}, "\n")
	content, isError := invokePatch(t, tool, patchText)
	if !isError {
		t.Fatalf("命中保护路径的补丁整体应当失败: %q", content)
	}
	raw, err := os.ReadFile(filepath.Join(root, "ok.txt"))
	if err != nil || string(raw) != "ok\n" {
		t.Fatalf("全有或全无被打破：普通文件被改了 %q (%v)", raw, err)
	}
	raw, err = os.ReadFile(filepath.Join(root, ".git", "config"))
	if err != nil || string(raw) != "[core]\n" {
		t.Fatalf("保护文件被改 %q (%v)", raw, err)
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
