package kit_test

// 激活集持久化在 kit 层的接线：Config.Activations 透传给底层 agent，所以
// 共用同一 store 的两个 kit.Agent 之间能够恢复激活结果。

import (
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/kit"
)

// TestKitActivationStoreWiring：第一个 kit.Agent 激活 web_fetch 后，用同一 store
// 构造的第二个 kit.Agent 在同一 SessionID 下已经能看到它。
func TestKitActivationStoreWiring(t *testing.T) {
	store := kit.NewMemoryActivations()
	cfg := func(provider *scriptProvider) kit.Config {
		return kit.Config{
			Model:         "m",
			Provider:      provider,
			Root:          t.TempDir(),
			Tools:         lazyConfiguredTools(),
			ToolDiscovery: kit.Bool(true),
			AllowNetwork:  kit.Bool(true),
			Activations:   store,
		}
	}
	first := mustNewKitAgent(t, cfg(&scriptProvider{}))
	if activated, unknown := first.ActivateTools("s1", "web_fetch"); len(activated) != 1 || len(unknown) != 0 {
		t.Fatalf("ActivateTools = (%v, %v)", activated, unknown)
	}

	second := mustNewKitAgent(t, cfg(&scriptProvider{}))
	if got := second.ActiveTools("s1"); !contains(got, "web_fetch") {
		t.Fatalf("第二个 kit.Agent 的 s1 active = %v（没有恢复 web_fetch）", got)
	}
	if got := second.ActiveTools("s2"); contains(got, "web_fetch") {
		t.Fatalf("s2 不该继承 s1 的持久化激活: %v", got)
	}
	// 恢复的集合也要出现在排序视图里（设置界面口径）。
	if got := strings.Join(second.ToolsFor("s1"), ","); !strings.Contains(got, "web_fetch") {
		t.Fatalf("ToolsFor(s1) = %v", got)
	}
}
