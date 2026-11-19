package patch_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/full"
	"github.com/zeokeel/zeokeel-rig/tools/patch"
)

// invokePatch 调用一次 apply_patch。
func invokePatch(t *testing.T, tool rig.Tool, patchText string) (string, bool) {
	t.Helper()
	encoded := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`).Replace(patchText)
	result, err := tool.Invoke(context.Background(), rig.ToolCall{
		ID: "c", Name: "apply_patch", Arguments: `{"patch":"` + encoded + `"}`,
	})
	if err != nil {
		return "GO ERROR: " + err.Error(), true
	}
	return result.Content, result.IsError
}

func readAll(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return "<err: " + err.Error() + ">"
	}
	return string(data)
}

// TestAuditPolicyIsHonored 断言 patch.WithPolicy 仍然遵守**保留下来的**围栏：
// 只读模式、根外写入；并钉住 DSH 对齐后**已停用**的强制（拒绝 glob、保护区）不再
// 拦任何东西。
//
// 背景：apply_patch 已经接进 full.All，但构造函数只拿 root、不拿 *sandbox.Policy，
// 因此默认情况下 read-only 模式拦不住它（见 TestAuditFullAllWiringStatus）。
// WithPolicy 是补齐这条边界的最小改动。
func TestAuditPolicyIsHonored(t *testing.T) {
	t.Run("只读模式", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "keep.md")
		if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeReadOnly))
		tool := patch.New(root, patch.WithPolicy(policy))
		out, isErr := invokePatch(t, tool, "*** Begin Patch\n*** Update File: keep.md\n@@\n-old\n+new\n*** End Patch\n")
		if !isErr {
			t.Fatalf("只读策略下仍然写入成功: %q", out)
		}
		if !strings.Contains(out, "read-only") {
			t.Fatalf("错误文案应说明只读策略: %q", out)
		}
		if got := readAll(t, path); got != "old\n" {
			t.Fatalf("只读策略下文件被改动: %q", got)
		}
	})

	t.Run("拒绝 glob 已停用", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, ".env"), []byte("SECRET=1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		policy := sandbox.New(
			sandbox.WithRoots(root),
			sandbox.WithMode(sandbox.ModeWorkspaceWrite),
			sandbox.WithDeniedGlobs("**/.env"),
		)
		tool := patch.New(root, patch.WithPolicy(policy))
		if out, isErr := invokePatch(t, tool, "*** Begin Patch\n*** Update File: .env\n@@\n-SECRET=1\n+SECRET=0\n*** End Patch\n"); isErr {
			t.Fatalf("命中拒绝 glob 仍被拒绝（glob 已停用）: %q", out)
		}
		if got := readAll(t, filepath.Join(root, ".env")); got != "SECRET=0\n" {
			t.Fatalf(".env = %q, want 写入生效", got)
		}
		if out, isErr := invokePatch(t, tool, "*** Begin Patch\n*** Add File: nested/.env\n+x\n*** End Patch\n"); isErr {
			t.Fatalf("新增曾命中 glob 的路径被拒绝（glob 已停用）: %q", out)
		}
	})

	t.Run("符号链接落到曾命中 glob 的目标（已停用）", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, ".env"), []byte("SECRET=1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(".env", filepath.Join(root, "notes.md")); err != nil {
			t.Skipf("符号链接不可用: %v", err)
		}
		policy := sandbox.New(
			sandbox.WithRoots(root),
			sandbox.WithMode(sandbox.ModeWorkspaceWrite),
			sandbox.WithDeniedGlobs("**/.env"),
		)
		tool := patch.New(root, patch.WithPolicy(policy))
		out, isErr := invokePatch(t, tool, "*** Begin Patch\n*** Update File: notes.md\n@@\n-SECRET=1\n+pwned\n*** End Patch\n")
		if isErr {
			t.Fatalf("经符号链接写曾命中 glob 的目标被拒绝（glob 已停用）: %q", out)
		}
		if got := readAll(t, filepath.Join(root, ".env")); got != "pwned\n" {
			t.Fatalf(".env = %q, want 经链接的写入生效", got)
		}
	})

	t.Run("保护区已停用", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "hooks"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "hooks", "pre-commit"), []byte("old\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		policy := sandbox.New(
			sandbox.WithRoots(root),
			sandbox.WithMode(sandbox.ModeWorkspaceWrite),
			sandbox.WithProtectedPaths("hooks"),
		)
		tool := patch.New(root, patch.WithPolicy(policy))
		out, isErr := invokePatch(t, tool, "*** Begin Patch\n*** Update File: hooks/pre-commit\n@@\n-old\n+new\n*** End Patch\n")
		if isErr {
			t.Fatalf("曾受保护路径被拒绝（保护已停用）: %q", out)
		}
		if got := readAll(t, filepath.Join(root, "hooks", "pre-commit")); got != "new\n" {
			t.Fatalf("hooks/pre-commit = %q, want 写入生效", got)
		}
	})

	t.Run("根外写入仍被拒绝", func(t *testing.T) {
		root := t.TempDir()
		policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
		tool := patch.New(root, patch.WithPolicy(policy))
		outside := filepath.Join(t.TempDir(), "outside.txt")
		if err := os.WriteFile(outside, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		out, isErr := invokePatch(t, tool, "*** Begin Patch\n*** Update File: "+filepath.ToSlash(outside)+"\n@@\n-x\n+y\n*** End Patch\n")
		if !isErr {
			t.Fatalf("根外写入没有被拒绝: %q", out)
		}
		if got := readAll(t, outside); got != "x\n" {
			t.Fatalf("根外文件被改写: %q", got)
		}
	})

	t.Run("不传策略时保持旧行为", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "keep.md")
		if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		tool := patch.New(root)
		if out, isErr := invokePatch(t, tool, "*** Begin Patch\n*** Update File: keep.md\n@@\n-old\n+new\n*** End Patch\n"); isErr {
			t.Fatalf("无策略时应当照常工作: %s", out)
		}
		if got := readAll(t, path); got != "new\n" {
			t.Fatalf("内容 = %q", got)
		}
	})
}

// TestAuditFullAllWiringStatus 记录 full.All 当前的接线状态：它把 apply_patch 挂上
// 了，但没有把 sandbox 策略传进去，于是 read-only 模式与 deny glob 都拦不住它。
//
// 这是"接线待办"：patch.WithPolicy 已经就位，tools/full/full.go 只需在构造时补
// patch.WithPolicy(opts.Policy)。
func TestAuditFullAllWiringStatus(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "keep.md"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeReadOnly),
		sandbox.WithDeniedGlobs("**/*.md"),
	)
	var applyTool rig.Tool
	for _, tool := range full.All(full.AllOptions{Policy: policy, Root: root}) {
		if tool.Name() == "apply_patch" {
			applyTool = tool
			break
		}
	}
	if applyTool == nil {
		t.Skip("full.All 没有 apply_patch")
	}
	out, isErr := invokePatch(t, applyTool, "*** Begin Patch\n*** Update File: keep.md\n@@\n-old\n+new\n*** End Patch\n")
	content := readAll(t, filepath.Join(root, "keep.md"))
	if isErr {
		t.Logf("full.All 已传入策略：apply_patch 被拒绝 (%s)", out)
		return
	}
	t.Logf("注意：full.All 尚未把 Policy 传给 apply_patch —— read-only/deny 未生效；"+
		"keep.md=%q 结果=%q；接线点 tools/full/full.go 的 patch.New(workspaceRoot, patchOpts...)",
		content, out)
}
