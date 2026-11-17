package kit_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/gate"
	"github.com/zeokeel/zeokeel-rig/kit"
	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// 下面这些测试覆盖 kit.Config.SandboxGrants 的接线：工具出现的位置、默认门禁
// （安全默认）以及显式门禁下的批准路径。

const grantTestReason = "the tool keeps its credential cache here"

// grantTestArgs 构造 request_sandbox_access 的参数 JSON。
func grantTestArgs(path string) string {
	return fmt.Sprintf(`{"path":%q,"reason":%q}`, path, grantTestReason)
}

// grantTestIsWritable 报告 path（解析符号链接后）是否已在策略的可写集里。
func grantTestIsWritable(policy *sandbox.Policy, path string) bool {
	want := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		want = resolved
	}
	for _, root := range policy.AdditionalWritableRoots() {
		if root == want {
			return true
		}
	}
	return false
}

// TestSandboxGrantsDefaultGateDenies 断言安全默认：开启 SandboxGrants 后默认门禁
// 依然拒绝 request_sandbox_access，策略里不会多出任何授权。
func TestSandboxGrantsDefaultGateDenies(t *testing.T) {
	root := t.TempDir()
	target := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	provider := &scriptProvider{scripts: [][]kit.Chunk{
		toolCallScript(kit.ToolCall{ID: "call_grant", Name: "request_sandbox_access", Arguments: grantTestArgs(target)}),
		textScript("done"),
	}}
	a := mustNew(t, kit.Config{Model: "m", Provider: provider, Root: root, Policy: policy, SandboxGrants: true})

	if _, _, err := a.RunText(context.Background(), "s1", "ask for access", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	message, ok := findToolMessage(provider.request(1).Messages, "request_sandbox_access")
	if !ok {
		t.Fatalf("no request_sandbox_access tool message in %+v", provider.request(1).Messages)
	}
	if !strings.Contains(message.Text(), "denied by approval gate") {
		t.Fatalf("tool result = %q, want an approval-gate denial", message.Text())
	}
	if len(policy.Grants()) != 0 {
		t.Fatalf("Grants() = %+v, want none: a denied call must not grant anything", policy.Grants())
	}
	if grantTestIsWritable(policy, target) {
		t.Fatalf("WritableRoots() = %v, must not contain %q", policy.AdditionalWritableRoots(), target)
	}
}

// TestSandboxGrantsExplicitGateApproves 断言显式门禁下批准生效，且授权写入的是
// 同一个 policy（也就是对默认工具集里的文件/shell 工具立即生效）。
func TestSandboxGrantsExplicitGateApproves(t *testing.T) {
	root := t.TempDir()
	target := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	provider := &scriptProvider{scripts: [][]kit.Chunk{
		toolCallScript(kit.ToolCall{ID: "call_grant", Name: "request_sandbox_access", Arguments: grantTestArgs(target)}),
		textScript("done"),
	}}
	a := mustNew(t, kit.Config{
		Model:         "m",
		Provider:      provider,
		Root:          root,
		Policy:        policy,
		SandboxGrants: true,
		// 显式审批：只有宿主能这样放开，默认门禁不会。
		Gate: gate.AllowTools("request_sandbox_access"),
	})

	if _, _, err := a.RunText(context.Background(), "s1", "ask for access", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	message, ok := findToolMessage(provider.request(1).Messages, "request_sandbox_access")
	if !ok {
		t.Fatalf("no request_sandbox_access tool message in %+v", provider.request(1).Messages)
	}
	if !strings.Contains(message.Text(), `"granted":true`) {
		t.Fatalf("tool result = %q, want a granted result", message.Text())
	}
	if len(policy.Grants()) != 1 {
		t.Fatalf("Grants() = %+v, want exactly one", policy.Grants())
	}
	if !grantTestIsWritable(policy, target) {
		t.Fatalf("WritableRoots() = %v, want it to contain %q", policy.AdditionalWritableRoots(), target)
	}
}

// TestSandboxGrantsOffByDefault 断言默认（SandboxGrants=false）行为不变：工具集里
// 没有该工具，调用会作为未知工具回传给模型。
func TestSandboxGrantsOffByDefault(t *testing.T) {
	root := t.TempDir()
	target := t.TempDir()
	provider := &scriptProvider{scripts: [][]kit.Chunk{
		toolCallScript(kit.ToolCall{ID: "call_grant", Name: "request_sandbox_access", Arguments: grantTestArgs(target)}),
		textScript("done"),
	}}
	a := mustNew(t, kit.Config{Model: "m", Provider: provider, Root: root})

	if _, _, err := a.RunText(context.Background(), "s1", "ask for access", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	message, ok := findToolMessage(provider.request(1).Messages, "request_sandbox_access")
	if !ok {
		t.Fatalf("no tool message in %+v", provider.request(1).Messages)
	}
	if !strings.Contains(message.Text(), "tool not found") {
		t.Fatalf("tool result = %q, want tool-not-found", message.Text())
	}
}

// TestSandboxStatusIsWired 断言开 SandboxGrants 时默认工具集里还有只读的
// sandbox_status，并且它与 request_sandbox_access 共享同一个 policy（授权后立刻
// 能看到新的可写面）。
func TestSandboxStatusIsWired(t *testing.T) {
	root := t.TempDir()
	target := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	provider := &scriptProvider{scripts: [][]kit.Chunk{
		toolCallScript(kit.ToolCall{ID: "call_grant", Name: "request_sandbox_access", Arguments: grantTestArgs(target)}),
		toolCallScript(kit.ToolCall{ID: "call_status", Name: "sandbox_status", Arguments: `{}`}),
		textScript("done"),
	}}
	a := mustNew(t, kit.Config{
		Model:         "m",
		Provider:      provider,
		Root:          root,
		Policy:        policy,
		SandboxGrants: true,
		Gate:          gate.AllowTools("request_sandbox_access", "sandbox_status"),
	})

	if _, _, err := a.RunText(context.Background(), "s1", "grant then inspect", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}

	grantMessage, ok := findToolMessage(provider.request(1).Messages, "request_sandbox_access")
	if !ok || strings.Contains(grantMessage.Text(), "denied by approval gate") {
		t.Fatalf("grant result = %+v, want an executed call", grantMessage)
	}
	if !strings.Contains(grantMessage.Text(), `"granted":true`) {
		t.Fatalf("grant result = %q, want a granted result", grantMessage.Text())
	}
	statusMessage, ok := findToolMessage(provider.request(2).Messages, "sandbox_status")
	if !ok {
		t.Fatalf("no sandbox_status tool message in %+v", provider.request(2).Messages)
	}
	// 状态工具读的是同一个 policy：授权已生效、预算已扣减一次。
	if !strings.Contains(statusMessage.Text(), `"grantBudgetLeft":4`) {
		t.Fatalf("sandbox_status result = %q, want grantBudgetLeft 4", statusMessage.Text())
	}
	if !grantTestIsWritable(policy, target) {
		t.Fatalf("WritableRoots() = %v, want it to contain %q", policy.AdditionalWritableRoots(), target)
	}
}

// TestSandboxStatusAllowedByDefaultGate 断言 sandbox_status 在开启授权能力时可以
// 被默认门禁自动放行：它是只读诊断工具，每次查看可写面都要审批只会制造摩擦。
// 与之对照，request_sandbox_access 仍然必须由人批准（见
// TestSandboxGrantsDefaultGateDenies）。
func TestSandboxStatusAllowedByDefaultGate(t *testing.T) {
	root := t.TempDir()
	provider := &scriptProvider{scripts: [][]kit.Chunk{
		toolCallScript(kit.ToolCall{ID: "call_status", Name: "sandbox_status", Arguments: `{}`}),
		textScript("done"),
	}}
	a := mustNew(t, kit.Config{Model: "m", Provider: provider, Root: root, SandboxGrants: true})

	if _, _, err := a.RunText(context.Background(), "s1", "inspect", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	message, ok := findToolMessage(provider.request(1).Messages, "sandbox_status")
	if !ok {
		t.Fatalf("no sandbox_status tool message in %+v", provider.request(1).Messages)
	}
	if strings.Contains(message.Text(), "denied by approval gate") {
		t.Fatalf("tool result = %q, want auto-allow for the read-only status tool", message.Text())
	}
	if !strings.Contains(message.Text(), `"mode":"workspace-write"`) {
		t.Fatalf("tool result = %q, want the policy view", message.Text())
	}
}

// TestSandboxGrantsIgnoredWithCustomTools 断言只有默认工具集（Tools == nil）才会
// 追加这两个工具：自定义工具集由宿主自行装配，kit 不隐式改动。
func TestSandboxGrantsIgnoredWithCustomTools(t *testing.T) {
	root := t.TempDir()
	target := t.TempDir()
	provider := &scriptProvider{scripts: [][]kit.Chunk{
		toolCallScript(kit.ToolCall{ID: "call_grant", Name: "request_sandbox_access", Arguments: grantTestArgs(target)}),
		textScript("done"),
	}}
	a := mustNew(t, kit.Config{
		Model:         "m",
		Provider:      provider,
		Root:          root,
		Tools:         []kit.Tool{}, // 非 nil 的空集合：不使用默认工具集
		SandboxGrants: true,
	})

	if _, _, err := a.RunText(context.Background(), "s1", "ask for access", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	message, ok := findToolMessage(provider.request(1).Messages, "request_sandbox_access")
	if !ok {
		t.Fatalf("no tool message in %+v", provider.request(1).Messages)
	}
	if !strings.Contains(message.Text(), "tool not found") {
		t.Fatalf("tool result = %q, want tool-not-found", message.Text())
	}
}
