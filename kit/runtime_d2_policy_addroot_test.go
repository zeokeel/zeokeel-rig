package kit_test

// `Runtime.AddRoot` 与宿主显式 `Config.Policy` 的组合：策略是宿主交进来的**共享**
// 对象，但宿主把同一个策略给 Runtime 并调用 AddRoot 时，"给这个 Runtime 加根"必须
// 真的生效——否则 AddRoot 静默什么都不做，而 `Config.Roots` 在同一组合下却会追加。
// 修法 (a)：把根声明进每个已存在 Agent 的那份显式策略（并记进 kit 声明的追加根，
// 让 RemoveRoot 仍能撤销），同时 slog.Warn 让宿主知道自己传的策略被改了。

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/zeokeel/zeokeel-rig/gate"
	"github.com/zeokeel/zeokeel-rig/kit"
	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// TestRuntimeAddRootAppliesToExplicitPolicyAgents：显式 Policy + 两个 Agent，
// AddRoot 之后两者都能写新根；之后新建的 Agent（换了一份显式策略）同样拿到它。
func TestRuntimeAddRootAppliesToExplicitPolicyAgents(t *testing.T) {
	root := t.TempDir()
	extra := t.TempDir()
	target := filepath.Join(extra, "out.txt")
	rt, err := kit.NewRuntime(kit.RuntimeConfig{Provider: &scriptProvider{}})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	// 宿主自己的策略：两个 Agent 共用同一份。
	shared := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	cfg := kit.Config{Model: "test-model", Root: root, Policy: shared, Gate: gate.AllowAll()}
	first, err := rt.NewAgent(cfg)
	if err != nil {
		t.Fatalf("NewAgent(first): %v", err)
	}
	t.Cleanup(func() { _ = first.Shutdown(context.Background()) })
	second, err := rt.NewAgent(cfg)
	if err != nil {
		t.Fatalf("NewAgent(second): %v", err)
	}
	t.Cleanup(func() { _ = second.Shutdown(context.Background()) })
	for name, agent := range map[string]*kit.Agent{"first": first, "second": second} {
		if agent.Policy().AllowsWrite(target) {
			t.Fatalf("AddRoot 之前 %s 就能写 %q", name, target)
		}
	}

	if err := rt.AddRoot(extra); err != nil {
		t.Fatalf("AddRoot(%q): %v", extra, err)
	}
	for name, agent := range map[string]*kit.Agent{"first": first, "second": second} {
		if !agent.Policy().AllowsWrite(target) {
			t.Fatalf("显式 Policy 下 AddRoot 没有对已存在的 %s 生效（%q 不可写）: writable=%v",
				name, target, agent.Policy().WritableRoots())
		}
	}
	// 宿主策略被明确修改：这也是文档承诺的那一半。
	if !shared.AllowsWrite(target) {
		t.Fatalf("AddRoot 没有把根声明进宿主传入的那份策略: %v", shared.WritableRoots())
	}

	// 之后新建的 Agent 用**另一份**显式策略：Runtime 级别的追加根同样进它的策略。
	other := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	later, err := rt.NewAgent(kit.Config{Model: "test-model", Root: root, Policy: other, Gate: gate.AllowAll()})
	if err != nil {
		t.Fatalf("NewAgent(later): %v", err)
	}
	t.Cleanup(func() { _ = later.Shutdown(context.Background()) })
	if !later.Policy().AllowsWrite(target) {
		t.Fatalf("显式 Policy 下新建的 Agent 没有拿到 AddRoot 的根（%q 不可写）: writable=%v",
			target, later.Policy().WritableRoots())
	}

	// 兼容性：AddRoot 声明进宿主策略的根仍归 kit 管，RemoveRoot 能撤销它。
	if err := rt.RemoveRoot(extra); err != nil {
		t.Fatalf("RemoveRoot(%q): %v", extra, err)
	}
	for name, agent := range map[string]*kit.Agent{"first": first, "second": second, "later": later} {
		if agent.Policy().AllowsWrite(target) {
			t.Fatalf("RemoveRoot 之后 %s 仍能写 %q: writable=%v", name, target, agent.Policy().WritableRoots())
		}
	}
}
