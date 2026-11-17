package kit_test

// v0.19.0 默认行为回归测试（门面层）：kit.Config 未设置 MaxIterations 时使用
// kit.DefaultSafeMaxIterations（200）作为成本护栏；显式 >0 的上限仍然生效；
// 显式负数表示"真的不限"（透传给 agent）。

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/kit"
)

// kitLongToolScripts 构造 rounds 个请求 echo 工具的模型轮次 + 一个文本收尾轮。
//
// 每轮参数不同：agent 的"抽风守卫"会拦下完全相同的重复调用，而本用例验证的是
// 轮数护栏，不该被守卫阈值干扰。
func kitLongToolScripts(rounds int) [][]kit.Chunk {
	scripts := make([][]kit.Chunk, 0, rounds+1)
	for i := 1; i <= rounds; i++ {
		scripts = append(scripts, toolCallScript(kit.ToolCall{
			ID: fmt.Sprintf("call_%d", i), Name: "echo", Arguments: fmt.Sprintf(`{"round":%d}`, i),
		}))
	}
	return append(scripts, textScript("done"))
}

// TestDefaultConfigLongRunSucceeds：零值 MaxIterations 的 kit.Config 下，25 轮
// 工具循环 + 收尾轮必须成功完成（默认上限 200 远大于此，不会误伤正常长任务）。
func TestDefaultConfigUnlimitedIterations(t *testing.T) {
	const toolRounds = 25
	provider := &scriptProvider{scripts: kitLongToolScripts(toolRounds)}
	tool := &fakeTool{name: "echo"}
	a, err := kit.New(kit.Config{
		Model:    "m",
		Provider: provider,
		Tools:    []kit.Tool{tool},
		Root:     t.TempDir(),
		Approval: kit.ApprovalAuto,
	})
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	result, err := a.Run(context.Background(), kit.RunRequest{
		Input: []kit.Message{kit.NewMessage(kit.RoleUser, "loop")},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Iterations != toolRounds+1 {
		t.Fatalf("Iterations = %d, want %d", result.Iterations, toolRounds+1)
	}
	if result.FinishReason != "stop" {
		t.Fatalf("FinishReason = %q, want stop", result.FinishReason)
	}
	tool.mu.Lock()
	calls := len(tool.calls)
	tool.mu.Unlock()
	if calls != toolRounds {
		t.Fatalf("echo calls = %d, want %d", calls, toolRounds)
	}
}

// TestConfigCapStillEnforced：显式 Config.MaxIterations=3 时，超限仍返回包装
// rig.ErrMaxIterations 的错误（对照组：保留上限能力）。
func TestConfigCapStillEnforced(t *testing.T) {
	scripts := make([][]kit.Chunk, 0, 4)
	for i := 1; i <= 4; i++ {
		scripts = append(scripts, toolCallScript(kit.ToolCall{
			ID: fmt.Sprintf("call_%d", i), Name: "echo", Arguments: fmt.Sprintf(`{"round":%d}`, i),
		}))
	}
	provider := &scriptProvider{scripts: scripts}
	tool := &fakeTool{name: "echo"}
	a, err := kit.New(kit.Config{
		Model:         "m",
		Provider:      provider,
		Tools:         []kit.Tool{tool},
		Root:          t.TempDir(),
		Approval:      kit.ApprovalAuto,
		MaxIterations: 3,
	})
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	result, err := a.Run(context.Background(), kit.RunRequest{
		Input: []kit.Message{kit.NewMessage(kit.RoleUser, "loop")},
	})
	if !errors.Is(err, rig.ErrMaxIterations) {
		t.Fatalf("Run error = %v, want ErrMaxIterations", err)
	}
	if result.Iterations != 3 {
		t.Fatalf("Iterations = %d, want 3", result.Iterations)
	}
	if provider.callCount() != 3 {
		t.Fatalf("provider calls = %d, want 3", provider.callCount())
	}
}

// TestDefaultMaxIterationsIsUnlimited：门面常量与 agent 常量一致，均为 0
// （unlimited）。
func TestDefaultMaxIterationsIsUnlimited(t *testing.T) {
	if kit.DefaultMaxIterations != 0 {
		t.Fatalf("kit.DefaultMaxIterations = %d, want 0 (unlimited)", kit.DefaultMaxIterations)
	}
}
