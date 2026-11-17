package agent_test

// 拒绝理由通道（denial reason channel）的 TDD 用例。
//
// 背景：个人客户端最常见的坏循环是"模型被拒 → 看不到原因 → 原样重试"。rig.Gate
// 的契约只有 Decision，因此 agent 引入一个**可选**接口：门禁可以额外提供
// ExplainDenial，把"命中哪条规则、该怎么办"交给 agent 拼进模型可见的工具错误。
// 不实现该接口的门禁必须得到与历史**逐字节相同**的文案。

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/gate"
)

// denialGateWithReason 是"实现拒绝理由通道"的门禁。
type denialGateWithReason struct {
	reason string
	denies int32
}

func (g *denialGateWithReason) Approve(context.Context, rig.Tool, rig.ToolCall) (rig.Decision, error) {
	atomic.AddInt32(&g.denies, 1)
	return rig.DecisionDeny, nil
}

func (g *denialGateWithReason) ExplainDenial(rig.Tool, rig.ToolCall) string { return g.reason }

// denialGatePlain 不实现理由通道（历史行为）。
type denialGatePlain struct{ denies int32 }

func (g *denialGatePlain) Approve(context.Context, rig.Tool, rig.ToolCall) (rig.Decision, error) {
	atomic.AddInt32(&g.denies, 1)
	return rig.DecisionDeny, nil
}

// denialOutcome 是一次"被拒绝的调用"在模型侧留下的痕迹。
type denialOutcome struct {
	// Message 是回填给模型的 role=tool 消息。
	Message rig.Message
	// Result 是同一内容的 ToolResult（IsError 在这里）。
	Result rig.ToolResult
}

// runDeniedTool 跑一轮"模型请求一个工具调用、门禁拒绝"的运行，返回 tool 消息与结果。
func runDeniedTool(t *testing.T, g rig.Gate, toolName, arguments string) denialOutcome {
	t.Helper()
	tool := &fakeTool{name: toolName, desc: "test tool", schema: map[string]any{"type": "object"}}
	provider := &scriptProvider{scripts: [][]rig.Chunk{
		toolCallScript("call_1", toolName, arguments),
		textScript("done"),
	}}
	a := mustNewAgent(t, rig.AgentSpec{
		Model: "m", Provider: provider, Tools: []rig.Tool{tool}, Gate: g,
	})
	var outcome denialOutcome
	result, err := a.Run(context.Background(), rig.RunRequest{
		Input: []rig.Message{userMessage("go")},
		Emit: func(event rig.Event) {
			if event.Type == rig.EventToolResult && event.Result != nil {
				outcome.Result = *event.Result
			}
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, message := range result.Messages {
		if message.Role == rig.RoleTool {
			outcome.Message = message
			return outcome
		}
	}
	t.Fatalf("没有 tool 消息: %+v", result.Messages)
	return denialOutcome{}
}

// TestDangerousDenialReasonReachesModel 断言：门禁实现 ExplainDenial 时，模型看到的
// 工具错误里带上原因。
func TestDangerousDenialReasonReachesModel(t *testing.T) {
	const reason = "危险命令（命中 rm.recursive-root）：rm 递归删除指向根目录；改用具体路径"
	g := &denialGateWithReason{reason: reason}
	outcome := runDeniedTool(t, g, "bash", `{"command":"rm -rf /"}`)
	message := outcome.Message
	text := message.Text()
	if !strings.Contains(text, reason) {
		t.Fatalf("工具错误里没有门禁给出的原因: %q", text)
	}
	if !strings.Contains(text, "tool call denied") {
		t.Fatalf("工具错误没有标明是审批拒绝: %q", text)
	}
	if !strings.Contains(text, "do not repeat") {
		t.Fatalf("工具错误没有告诉模型不要重试: %q", text)
	}
	if !outcome.Result.IsError {
		t.Fatal("被拒绝的调用必须是 IsError 的工具结果")
	}
	if atomic.LoadInt32(&g.denies) != 1 {
		t.Fatalf("gate 被调用 %d 次, want 1", g.denies)
	}
}

// TestPlainDenialKeepsHistoricalMessage 断言不实现 ExplainDenial 时文案逐字节不变
// （这是兼容性红线：既有宿主/快照依赖这段话）。
func TestPlainDenialKeepsHistoricalMessage(t *testing.T) {
	message := runDeniedTool(t, &denialGatePlain{}, "bash", `{"command":"rm -rf /"}`).Message
	const want = "rig: tool call denied by approval gate: bash"
	if got := message.Text(); got != want {
		t.Fatalf("文案变了：\n got %q\nwant %q", got, want)
	}
}

// TestEmptyExplainFallsBackToHistoricalMessage 断言 ExplainDenial 返回空串时也
// 退回历史文案（空理由等于没有理由）。
func TestEmptyExplainFallsBackToHistoricalMessage(t *testing.T) {
	message := runDeniedTool(t, &denialGateWithReason{reason: "   "}, "bash", `{"command":"x"}`).Message
	const want = "rig: tool call denied by approval gate: bash"
	if got := message.Text(); got != want {
		t.Fatalf("空理由应当退回历史文案：\n got %q\nwant %q", got, want)
	}
}

// TestDangerousCommandRuleIDVisibleEndToEnd 断言经 PersonalDefault 拒绝危险命令时，
// 模型看到的文案里有规则 id 与中文理由（端到端，不经手工 stub）。
func TestDangerousCommandRuleIDVisibleEndToEnd(t *testing.T) {
	g := gate.PersonalDefault(gate.DenyAll())
	message := runDeniedTool(t, g, "bash", `{"command":"rm -rf /"}`).Message
	text := message.Text()
	if !strings.Contains(text, "rm.recursive-root") {
		t.Fatalf("文案里没有命中的规则 id: %q", text)
	}
	if !strings.Contains(text, "危险命令") {
		t.Fatalf("文案里没有中文理由: %q", text)
	}
	if !strings.Contains(text, "do not repeat") {
		t.Fatalf("文案里没有「不要重复」的指引: %q", text)
	}
	t.Logf("真实输出（危险命令）: %s", text)
}

// TestInterpreterWrappedDangerousCommandReason 断言解释器包装（bash -c '…'）也能
// 给出理由——模型最常用来"绕过"只读判定的就是这种写法。
func TestInterpreterWrappedDangerousCommandReason(t *testing.T) {
	g := gate.PersonalDefault(gate.DenyAll())
	message := runDeniedTool(t, g, "bash", `{"command":"bash -c 'rm -rf /'"}`).Message
	text := message.Text()
	if !strings.Contains(text, "rm.recursive-root") {
		t.Fatalf("解释器包装后的危险命令没有给出规则: %q", text)
	}
	if strings.Contains(text, "被 approval gate 拒绝") {
		t.Fatalf("文案不应退回没有理由的旧格式: %q", text)
	}
	t.Logf("真实输出（bash -c 包装）: %s", text)
}

// TestMutatingCommandStillDefersToInner 断言非危险命令仍然交给 inner（理由通道不改变
// 决策语义）。
func TestMutatingCommandStillDefersToInner(t *testing.T) {
	inner := &denialGateWithReason{reason: "用户暂时拒绝：先看改动计划"}
	g := gate.PersonalDefault(inner)
	message := runDeniedTool(t, g, "bash", `{"command":"rm -rf ./build"}`).Message
	if atomic.LoadInt32(&inner.denies) != 1 {
		t.Fatalf("可变命令应当委托 inner 一次，实际 %d 次", inner.denies)
	}
	if !strings.Contains(message.Text(), "用户暂时拒绝") {
		t.Fatalf("inner 的理由没有透出来: %q", message.Text())
	}
}
