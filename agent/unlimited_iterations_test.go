package agent_test

// v0.19.0 默认行为回归测试：模型↔工具轮数默认不限制（unlimited by default）。
//
//   - 默认 spec / 默认 RunRequest（以及负值配置）下，超过旧默认 20 轮的长工具
//     循环必须正常完成，不报 rig.ErrMaxIterations；
//   - 显式 >0 的上限能力保留：spec 或 RunRequest 配置 cap 后，超限仍返回包装
//     rig.ErrMaxIterations 的错误（对照组，改动前后都必须通过）。

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/agent"
)

// longToolScripts 构造 rounds 个请求同一工具的模型轮次，最后跟一个纯文本收尾
// 轮次（共 rounds+1 次模型调用）。
//
// 每轮参数不同：agent 自带"抽风守卫"（连续相同的调用会被提示/熔断），本用例
// 验证的是**轮数不设上限**，不该顺带把守卫的阈值当上限。
func longToolScripts(rounds int) [][]rig.Chunk {
	scripts := make([][]rig.Chunk, 0, rounds+1)
	for i := 1; i <= rounds; i++ {
		scripts = append(scripts, toolCallScript(fmt.Sprintf("call_%d", i), "echo", fmt.Sprintf(`{"round":%d}`, i)))
	}
	return append(scripts, textScript("done"))
}

// requireUnlimitedRun 断言一次长循环运行正常完成：无错误、轮数与工具调用数
// 符合脚本、没有 EventError。
func requireUnlimitedRun(t *testing.T, spec rig.AgentSpec, req rig.RunRequest, toolRounds int) {
	t.Helper()
	echo, _ := spec.Tools[0].(*fakeTool)
	provider, _ := spec.Provider.(*scriptProvider)
	rec := &recorder{}
	req.Emit = rec.emit
	a := mustNewAgent(t, spec)
	result, err := a.Run(context.Background(), req)
	requireNoError(t, err)
	if result.Iterations != toolRounds+1 {
		t.Fatalf("Iterations = %d, want %d", result.Iterations, toolRounds+1)
	}
	if result.FinishReason != "stop" {
		t.Fatalf("FinishReason = %q, want stop", result.FinishReason)
	}
	if echo.callCount() != toolRounds {
		t.Fatalf("echo calls = %d, want %d", echo.callCount(), toolRounds)
	}
	if provider.callCount() != toolRounds+1 {
		t.Fatalf("provider calls = %d, want %d", provider.callCount(), toolRounds+1)
	}
	if events := rec.ofType(rig.EventError); len(events) != 0 {
		t.Fatalf("unexpected error events: %+v", events)
	}
}

// TestRunDefaultSpecUnlimitedIterations：默认 spec（未设置 MaxIterations）与
// 默认 RunRequest 下，25 轮工具循环 + 收尾轮（26 轮 > 旧默认 20）必须成功。
func TestRunDefaultSpecUnlimitedIterations(t *testing.T) {
	const toolRounds = 25
	requireUnlimitedRun(t,
		rig.AgentSpec{
			Model:    "m",
			Provider: &scriptProvider{scripts: longToolScripts(toolRounds)},
			Tools:    []rig.Tool{&fakeTool{name: "echo"}},
		},
		rig.RunRequest{Input: []rig.Message{userMessage("loop")}},
		toolRounds)
}

// TestRunNegativeSpecMaxIterationsUnlimited：spec.MaxIterations 为负值同样表示
// 不限制轮数。
func TestRunNegativeSpecMaxIterationsUnlimited(t *testing.T) {
	const toolRounds = 25
	requireUnlimitedRun(t,
		rig.AgentSpec{
			Model:         "m",
			Provider:      &scriptProvider{scripts: longToolScripts(toolRounds)},
			Tools:         []rig.Tool{&fakeTool{name: "echo"}},
			MaxIterations: -3,
		},
		rig.RunRequest{Input: []rig.Message{userMessage("loop")}},
		toolRounds)
}

// TestRunNegativeRequestMaxIterationsUnlimited：默认 spec 下 RunRequest 的负值
// 回退到 agent 默认（不限制）。
func TestRunNegativeRequestMaxIterationsUnlimited(t *testing.T) {
	const toolRounds = 25
	requireUnlimitedRun(t,
		rig.AgentSpec{
			Model:    "m",
			Provider: &scriptProvider{scripts: longToolScripts(toolRounds)},
			Tools:    []rig.Tool{&fakeTool{name: "echo"}},
		},
		rig.RunRequest{Input: []rig.Message{userMessage("loop")}, MaxIterations: -1},
		toolRounds)
}

// TestRunSpecCapAboveLegacyDefault：显式 spec 上限（30）高于旧默认 20 时按配置
// 生效，26 轮运行成功（对照组：改动前后都通过）。
func TestRunSpecCapAboveLegacyDefault(t *testing.T) {
	const toolRounds = 25
	requireUnlimitedRun(t,
		rig.AgentSpec{
			Model:         "m",
			Provider:      &scriptProvider{scripts: longToolScripts(toolRounds)},
			Tools:         []rig.Tool{&fakeTool{name: "echo"}},
			MaxIterations: 30,
		},
		rig.RunRequest{Input: []rig.Message{userMessage("loop")}},
		toolRounds)
}

// TestRunRequestCapStillEnforced：显式 RunRequest.MaxIterations=3 时，第 4 轮
// 仍被拒绝并返回包装 rig.ErrMaxIterations 的错误（对照组：保留上限能力）。
func TestRunRequestCapStillEnforced(t *testing.T) {
	provider := &scriptProvider{
		scripts:    [][]rig.Chunk{toolCallScript("c1", "echo", `{}`)},
		repeatLast: true,
	}
	echo := &fakeTool{name: "echo"}
	rec := &recorder{}
	a := mustNewAgent(t, rig.AgentSpec{Model: "m", Provider: provider, Tools: []rig.Tool{echo}})

	result, err := a.Run(context.Background(), rig.RunRequest{
		Input:         []rig.Message{userMessage("loop")},
		MaxIterations: 3,
		Emit:          rec.emit,
	})
	requireErrorIs(t, err, rig.ErrMaxIterations)
	if result.Iterations != 3 {
		t.Fatalf("Iterations = %d, want 3", result.Iterations)
	}
	if echo.callCount() != 3 {
		t.Fatalf("echo calls = %d, want 3 (last allowed round still executes tools)", echo.callCount())
	}
	errorEvents := rec.ofType(rig.EventError)
	if len(errorEvents) != 1 || !errors.Is(errorEvents[0].Err, rig.ErrMaxIterations) {
		t.Fatalf("error events = %+v, want one wrapping ErrMaxIterations", errorEvents)
	}
}

// TestRunSpecCapStillEnforced：显式 spec.MaxIterations=3、默认 RunRequest 时，
// 超限仍返回包装 rig.ErrMaxIterations 的错误（对照组：spec 层上限不被默认
// unlimited 语义吞掉）。
func TestRunSpecCapStillEnforced(t *testing.T) {
	provider := &scriptProvider{
		scripts:    [][]rig.Chunk{toolCallScript("c1", "echo", `{}`)},
		repeatLast: true,
	}
	echo := &fakeTool{name: "echo"}
	a := mustNewAgent(t, rig.AgentSpec{
		Model:         "m",
		Provider:      provider,
		Tools:         []rig.Tool{echo},
		MaxIterations: 3,
	})

	result, err := a.Run(context.Background(), rig.RunRequest{Input: []rig.Message{userMessage("loop")}})
	requireErrorIs(t, err, rig.ErrMaxIterations)
	if result.Iterations != 3 {
		t.Fatalf("Iterations = %d, want 3", result.Iterations)
	}
	if echo.callCount() != 3 {
		t.Fatalf("echo calls = %d, want 3", echo.callCount())
	}
}

// TestDefaultMaxIterationsIsUnlimited：导出常量重定义为 0（unlimited），不再
// 是 20。
func TestDefaultMaxIterationsIsUnlimited(t *testing.T) {
	if agent.DefaultMaxIterations != 0 {
		t.Fatalf("agent.DefaultMaxIterations = %d, want 0 (unlimited)", agent.DefaultMaxIterations)
	}
}
