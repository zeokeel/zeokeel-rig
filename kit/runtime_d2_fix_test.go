package kit_test

// D2 复审遗留四条（审计 .audit/audit/d2-roots-mcp.md 第 2/4/5/6 条）：
//
//  1. `RemoveRoot` 对静态 `Config.Roots` 必须持久：宿主用同一份 Config 再建 Agent 时
//     该根不能复活；被撤销的根若被当作主根，`NewAgent` 返回可读错误（而不是静默变成
//     无根）。
//  2. `DeriveRunner` 在只给了共享 `SandboxRunner` 时要继承它的配置，拿不到配置就
//     报错（不静默退回平台链）；`Close` 之后返回 `ErrRuntimeClosed`。
//  3. `Config.Roots` 与宿主显式 `Config.Policy` 并存时，`Roots[1:]` 作为追加可写根
//     加入该 Policy，并做存在性/目录校验（文档承诺的"每个根都被校验"）。
//  4. `MCPPrefix` 两侧规范化一致：带空白的前缀仍能通过预设过滤。

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/gate"
	"github.com/zeokeel/zeokeel-rig/kit"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/sandbox/osrunner"
	"github.com/zeokeel/zeokeel-rig/tools/mcp"
)

// TestRuntimeRemoveRootIsPersistentForNewAgents：RemoveRoot 对静态 Config.Roots 同样
// 持久——同一个 Config 新建的 Agent 不再拿到被撤销的根；被撤销的根不能静默降级成
// 无根主根；AddRoot 可以把它加回来。
func TestRuntimeRemoveRootIsPersistentForNewAgents(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	third := t.TempDir()
	rt, err := kit.NewRuntime(kit.RuntimeConfig{Model: "m"})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	cfg := kit.Config{Model: "m", Roots: []string{first, second}, Gate: gate.AllowAll()}
	before, err := rt.NewAgent(cfg)
	if err != nil {
		t.Fatalf("NewAgent(before): %v", err)
	}
	t.Cleanup(func() { _ = before.Shutdown(context.Background()) })

	if err := rt.RemoveRoot(second); err != nil {
		t.Fatalf("RemoveRoot(%q): %v", second, err)
	}
	if containsPath(before.Policy().Roots(), canonicalPath(t, second)) {
		t.Fatalf("RemoveRoot 没有撤销已存在 Agent 的静态根: %v", before.Policy().Roots())
	}

	// 同一个静态 Config 再建 Agent：被撤销的根不能复活（AddRoot/RemoveRoot 的语义
	// 必须对称）。
	after, err := rt.NewAgent(cfg)
	if err != nil {
		t.Fatalf("NewAgent(after): %v", err)
	}
	t.Cleanup(func() { _ = after.Shutdown(context.Background()) })
	if containsPath(after.Policy().Roots(), canonicalPath(t, second)) {
		t.Fatalf("RemoveRoot 对静态 Config.Roots 不持久：同一个 Config 新建的 Agent 又拿到了 %q: %v",
			second, after.Policy().Roots())
	}

	// 被撤销的根被用作主根：必须可读报错，而不是静默丢掉主根。
	if _, err := rt.NewAgent(kit.Config{Model: "m", Roots: []string{second, third}, Gate: gate.AllowAll()}); err == nil {
		t.Fatalf("把被撤销的根 %q 当主根时必须报错", second)
	} else if !strings.Contains(err.Error(), "revoked") && !strings.Contains(err.Error(), "RemoveRoot") {
		t.Fatalf("错误文案应说明该根已被 RemoveRoot 撤销: %v", err)
	}

	// 撤销不是永久的：AddRoot 把它加回来，新 Agent 又能用它（含当主根）。
	if err := rt.AddRoot(second); err != nil {
		t.Fatalf("AddRoot(%q): %v", second, err)
	}
	restored, err := rt.NewAgent(kit.Config{Model: "m", Roots: []string{second, third}, Gate: gate.AllowAll()})
	if err != nil {
		t.Fatalf("NewAgent(restored): %v", err)
	}
	t.Cleanup(func() { _ = restored.Shutdown(context.Background()) })
	if !containsPath(restored.Policy().Roots(), canonicalPath(t, second)) {
		t.Fatalf("AddRoot 之后新 Agent 仍缺少 %q: %v", second, restored.Policy().Roots())
	}
}

// configuredRunner 是"能交回自己构造配置"的共享 runner：DeriveRunner 应当以它为
// 基准，而不是退回平台链。
type configuredRunner struct {
	cfg osrunner.Config
}

func (r *configuredRunner) Confine(argv []string, policy *sandbox.Policy) (sandbox.ConfinedCommand, error) {
	return sandbox.ConfinedCommand{Argv: append([]string(nil), argv...), Enforcement: sandbox.EnforcementFull}, nil
}

// RunnerConfig 交回构造配置（kit 用这个结构化接口派生，不 import 具体类型）。
func (r *configuredRunner) RunnerConfig() osrunner.Config { return r.cfg }

// TestDeriveRunnerInheritsSharedConfig：只给共享 runner、不给 SandboxRunnerConfig 时，
// 派生必须基于共享 runner 的配置；连配置都拿不到时返回可读错误。
func TestDeriveRunnerInheritsSharedConfig(t *testing.T) {
	policy := sandbox.New(sandbox.WithRoots(t.TempDir()), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	shared := &configuredRunner{cfg: osrunner.Config{
		RunnerCommand:           []string{"shared-wrap"},
		RunnerFailureSignatures: []string{"shared wrap failed"},
	}}
	rt, err := kit.NewRuntime(kit.RuntimeConfig{Model: "m", SandboxRunner: shared})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	derived, err := rt.DeriveRunner()
	if err != nil {
		t.Fatalf("DeriveRunner: %v", err)
	}
	confined, err := derived.Confine([]string{"/bin/sh", "-c", "echo hi"}, policy)
	if err != nil {
		t.Fatalf("derived.Confine: %v", err)
	}
	if len(confined.Argv) == 0 || confined.Argv[0] != "shared-wrap" {
		t.Fatalf("DeriveRunner 没有继承共享 runner 的配置（退回了平台链）: %v", confined.Argv)
	}

	// 共享 runner 不暴露配置：必须报错点名缺 SandboxRunnerConfig，不能静默换后端。
	plain, err := kit.NewRuntime(kit.RuntimeConfig{Model: "m", SandboxRunner: &kitRunner{}})
	if err != nil {
		t.Fatalf("NewRuntime(plain): %v", err)
	}
	t.Cleanup(func() { _ = plain.Close() })
	if _, err := plain.DeriveRunner(); err == nil {
		t.Fatal("共享 runner 不暴露配置时 DeriveRunner 必须报错（不能静默换后端）")
	} else if !strings.Contains(err.Error(), "SandboxRunnerConfig") {
		t.Fatalf("错误文案应指明缺 RuntimeConfig.SandboxRunnerConfig: %v", err)
	}
}

// TestDeriveRunnerAfterCloseFails：Close 是硬边界，DeriveRunner 与其它运行期 API
// 一致返回 ErrRuntimeClosed。
func TestDeriveRunnerAfterCloseFails(t *testing.T) {
	rt, err := kit.NewRuntime(kit.RuntimeConfig{Model: "m", SandboxRunnerConfig: &osrunner.Config{
		RunnerCommand:           []string{"wrap"},
		RunnerFailureSignatures: []string{"wrap failed"},
	}})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	if err := rt.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := rt.DeriveRunner(); !errors.Is(err, kit.ErrRuntimeClosed) {
		t.Fatalf("Close 之后 DeriveRunner = %v, want ErrRuntimeClosed", err)
	}
}

// policyListsRoot 报告策略的可写面模型里是否有 path（Roots ∪ 追加根）。
func policyListsRoot(policy *sandbox.Policy, path string) bool {
	for _, root := range policy.WritableRoots() {
		if sandbox.PathEqual(root.Root, path) {
			return true
		}
	}
	return false
}

// TestConfigRootsAppendToExplicitPolicy：显式 Policy + Config.Roots 时，Roots[1:] 作为
// 追加可写根加入策略（并校验存在/是目录），而不是被静默丢弃。
func TestConfigRootsAppendToExplicitPolicy(t *testing.T) {
	root := t.TempDir()
	extra := t.TempDir()
	target := filepath.Join(extra, "out.txt")
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	agent, err := kit.New(kit.Config{
		Model:    "m",
		Provider: &scriptProvider{},
		Root:     root,
		Roots:    []string{root, extra},
		Policy:   policy,
		Gate:     gate.AllowAll(),
	})
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	t.Cleanup(func() { _ = agent.Shutdown(context.Background()) })
	if !policyListsRoot(agent.Policy(), canonicalPath(t, extra)) {
		t.Fatalf("显式 Policy + Config.Roots 时 Roots[1:] 被静默丢弃: writable=%v",
			agent.Policy().WritableRoots())
	}
	if !agent.Policy().AllowsWrite(target) {
		t.Fatalf("显式 Policy 下 Roots[1:] 没有进可写面: %q 不可写", target)
	}

	// 显式 Policy 下同样 fail-closed：不存在的根必须构造期报错。
	missing := filepath.Join(t.TempDir(), "missing")
	missingPolicy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	if _, err := kit.New(kit.Config{
		Model:    "m",
		Provider: &scriptProvider{},
		Root:     root,
		Roots:    []string{root, missing},
		Policy:   missingPolicy,
		Gate:     gate.AllowAll(),
	}); err == nil {
		t.Fatalf("显式 Policy 下不存在的 Config.Roots 项 %q 必须构造期报错", missing)
	}

	// kit 追加到宿主策略上的根可以被 Runtime.RemoveRoot 撤销（策略本身仍归宿主），
	// 否则"撤销的根还在可写面里"就换了个位置复发。
	rt, err := kit.NewRuntime(kit.RuntimeConfig{Model: "m"})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	revocable := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	rtAgent, err := rt.NewAgent(kit.Config{
		Model:  "m",
		Root:   root,
		Roots:  []string{root, extra},
		Policy: revocable,
		Gate:   gate.AllowAll(),
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	t.Cleanup(func() { _ = rtAgent.Shutdown(context.Background()) })
	if err := rt.RemoveRoot(extra); err != nil {
		t.Fatalf("RemoveRoot(%q): %v", extra, err)
	}
	if rtAgent.Policy().AllowsWrite(target) {
		t.Fatalf("RemoveRoot 之后显式 Policy 上仍可写 %q（撤销没生效）", target)
	}
}

// TestMCPPrefixWithWhitespaceKeepsToolsRegistered：MCPPrefix 带首尾空白时，工具名
// 生成与"服务端绑定工具"识别必须用同一份规范化结果——否则 personal 预设会把 MCP
// 工具静默裁掉。
func TestMCPPrefixWithWhitespaceKeepsToolsRegistered(t *testing.T) {
	root := t.TempDir()
	client := newFakeMCPClient(t, "fake", "echo")
	agent, err := kit.New(kit.Config{
		Model:      "m",
		Provider:   &scriptProvider{},
		Root:       root,
		MCPClients: []*mcp.Client{client},
		MCPPrefix:  " mcp__ ",
		ToolPreset: "personal",
		Gate:       gate.AllowAll(),
	})
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	t.Cleanup(func() { _ = agent.Shutdown(context.Background()) })

	registered := agent.RegisteredTools()
	if !slices.Contains(registered, "mcp__fake__echo") {
		t.Fatalf("MCPPrefix 带空白时 MCP 工具被预设静默裁掉: %v", registered)
	}
	if slices.ContainsFunc(registered, func(name string) bool { return strings.Contains(name, " ") }) {
		t.Fatalf("工具名里出现了空白（前缀没有规范化）: %v", registered)
	}
}
