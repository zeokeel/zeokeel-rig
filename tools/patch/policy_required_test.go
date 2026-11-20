package patch_test

// apply_patch 的策略契约：**没有策略的实例不可用**。
//
// apply_patch 是唯一能一次改动多个文件、还能删除/移动文件的写入口。它必须遵守
// 与其它写工具同一套 sandbox.Policy（只读模式、根内只读子路径）。旧实现把
// WithPolicy 当可选注入，漏传时静默退化成"只做工作区根围栏"，即一条沙箱旁路。
// 这里钉住新语义：漏传策略的实例每次调用都明确报错，绝不悄悄放行。
//
// 用同一份补丁做对照：无策略 → 报错且不落盘；有策略 → 成功并落盘。这样测的是
// "策略是否存在"而不是"补丁本身有没有写错"。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/patch"
)

// policyTestPatch 是一份对 a.txt 的合法更新补丁。
const policyTestPatch = "*** Begin Patch\n*** Update File: a.txt\n@@\n one\n-two\n+TWO\n*** End Patch\n"

// policyTestPayload 把补丁文本编码成调用参数。
func policyTestPayload(t *testing.T, patchText string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"patch": patchText})
	if err != nil {
		t.Fatalf("marshal arguments: %v", err)
	}
	return string(payload)
}

// TestApplyPatchWithoutPolicyFailsClosed：不传 WithPolicy 时调用必须报错。
func TestApplyPatchWithoutPolicyFailsClosed(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "one\ntwo\n")

	tool := patch.New(root)
	result, err := tool.Invoke(context.Background(), rig.ToolCall{
		ID: "c1", Name: patch.ToolName, Arguments: policyTestPayload(t, policyTestPatch),
	})
	if err != nil {
		t.Fatalf("Invoke 返回 Go 错误（应为工具结果里的可读错误）: %v", err)
	}
	if !result.IsError {
		t.Fatalf("没有策略却成功了: %q", result.Content)
	}
	if !strings.Contains(strings.ToLower(result.Content), "policy") {
		t.Fatalf("错误文案没有提到 policy: %q", result.Content)
	}
	// fail closed：一个字节都不许改。
	raw, readErr := os.ReadFile(filepath.Join(root, "a.txt"))
	if readErr != nil || string(raw) != "one\ntwo\n" {
		t.Fatalf("无策略时仍然落盘了: %q (%v)", raw, readErr)
	}
}

// TestApplyPatchWithPolicySucceeds：同一份补丁，传了策略就应当正常工作。
func TestApplyPatchWithPolicySucceeds(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "one\ntwo\n")

	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	tool := patch.New(root, patch.WithPolicy(policy))
	result, err := tool.Invoke(context.Background(), rig.ToolCall{
		ID: "c1", Name: patch.ToolName, Arguments: policyTestPayload(t, policyTestPatch),
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.IsError {
		t.Fatalf("有策略时不该报错: %s", result.Content)
	}
	raw, readErr := os.ReadFile(filepath.Join(root, "a.txt"))
	if readErr != nil || string(raw) != "one\nTWO\n" {
		t.Fatalf("没有落盘: %q (%v)", raw, readErr)
	}
}

// TestApplyPatchWithReadOnlyPolicyStillRefuses：构造契约没变——策略照旧生效。
func TestApplyPatchWithReadOnlyPolicyStillRefuses(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "one\ntwo\n")

	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeReadOnly))
	tool := patch.New(root, patch.WithPolicy(policy))
	result, err := tool.Invoke(context.Background(), rig.ToolCall{
		ID: "c1", Name: patch.ToolName, Arguments: policyTestPayload(t, policyTestPatch),
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !result.IsError {
		t.Fatalf("只读策略下仍然写入成功: %q", result.Content)
	}
	if !strings.Contains(strings.ToLower(result.Content), "read-only") {
		t.Fatalf("错误文案没有提到 read-only: %q", result.Content)
	}
}
