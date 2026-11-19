package kit_test

// 默认不设轮数上限（用户要求：不保留人为边界，成本交给 Meter/MaxRunCostUSD 与人工审批）。
//
// 旧行为：kit.Config.MaxIterations 未配置（0）时被解析成 DefaultSafeMaxIterations=200，
// 超过 200 轮返回 rig.ErrMaxIterations；显式负数才是"真的不限"。现在 0 就是不限，
// 显式 >0 仍是显式上限。

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/kit"
)

// TestKitIterationLimitIsUnlimitedByDefault：默认配置下超过旧的 200 轮上限也必须跑完。
func TestKitIterationLimitIsUnlimitedByDefault(t *testing.T) {
	if kit.DefaultSafeMaxIterations != 0 {
		t.Fatalf("kit.DefaultSafeMaxIterations = %d, want 0 (unlimited by default)",
			kit.DefaultSafeMaxIterations)
	}

	const toolRounds = 205 // 旧默认上限 200 + 5
	provider := &scriptProvider{scripts: kitLongToolScripts(toolRounds)}
	tool := &fakeTool{name: "echo"}
	agent, err := kit.New(kit.Config{
		Model:    "m",
		Provider: provider,
		Tools:    []kit.Tool{tool},
		Root:     t.TempDir(),
		Approval: kit.ApprovalAuto,
	})
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	result, err := agent.Run(context.Background(), kit.RunRequest{
		Input: []kit.Message{kit.NewMessage(kit.RoleUser, "long loop")},
	})
	if err != nil {
		t.Fatalf("Run: %v (default config must not cap the model<->tool rounds)", err)
	}
	if result.Iterations != toolRounds+1 {
		t.Fatalf("Iterations = %d, want %d", result.Iterations, toolRounds+1)
	}
	if result.FinishReason != "stop" {
		t.Fatalf("FinishReason = %q, want stop", result.FinishReason)
	}
}

// TestKitExplicitIterationCapStillApplies：显式 >0 的上限仍然生效（护栏是显式选择）。
func TestKitExplicitIterationCapStillApplies(t *testing.T) {
	const cap = 6
	provider := &scriptProvider{scripts: kitLongToolScripts(cap + 3)}
	agent, err := kit.New(kit.Config{
		Model:         "m",
		Provider:      provider,
		Tools:         []kit.Tool{&fakeTool{name: "echo"}},
		Root:          t.TempDir(),
		Approval:      kit.ApprovalAuto,
		MaxIterations: cap,
	})
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	result, err := agent.Run(context.Background(), kit.RunRequest{
		Input: []kit.Message{kit.NewMessage(kit.RoleUser, "loop")},
	})
	if !errors.Is(err, rig.ErrMaxIterations) {
		t.Fatalf("err = %v, want rig.ErrMaxIterations for an explicit cap", err)
	}
	if result.Iterations != cap {
		t.Fatalf("Iterations = %d, want %d", result.Iterations, cap)
	}
	_ = fmt.Sprint // 保持与其它 kit 测试相同的导入形状
}
