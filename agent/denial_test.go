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

// allowAsker 永远回答 allow（模拟用户在审批弹窗里点了"允许"）。
type allowAsker struct{ asks int }

func (a *allowAsker) Ask(context.Context, string, []string) (string, error) {
	a.asks++
	return "allow", nil
}

// gatedOutcome 是一次"模型请求工具、门禁放行、工具真的执行"的端到端痕迹。
type gatedOutcome struct {
	// Message 是回填给模型的 role=tool 消息。
	Message rig.Message
	// Invocations 是工具被真正执行的次数（0 表示审批拦下了它）。
	Invocations int
}

// runGatedTool 跑一轮完整链路：模型发起调用 → 门禁 →（通过则）工具执行 → 回填消息。
func runGatedTool(t *testing.T, g rig.Gate, toolName, arguments string) gatedOutcome {
	t.Helper()
	tool := &fakeTool{name: toolName, desc: "test tool", schema: map[string]any{"type": "object"}}
	provider := &scriptProvider{scripts: [][]rig.Chunk{
		toolCallScript("call_1", toolName, arguments),
		textScript("done"),
	}}
	a := mustNewAgent(t, rig.AgentSpec{
		Model: "m", Provider: provider, Tools: []rig.Tool{tool}, Gate: g,
	})
	result, err := a.Run(context.Background(), rig.RunRequest{
		Input: []rig.Message{userMessage("go")},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, message := range result.Messages {
		if message.Role == rig.RoleTool {
			return gatedOutcome{Message: message, Invocations: tool.callCount()}
		}
	}
	t.Fatalf("没有 tool 消息: %+v", result.Messages)
	return gatedOutcome{}
}

// TestDangerousCommandRuleIDIsAdvisoryOnlyEndToEnd（版本 A：ask + 记录型 Asker）。
//
// 命令分类已经移出默认门禁：ask 档下 `rm -rf /` 与其它调用一样**问人**，人批准后
// 工具真的执行，模型看到的工具结果里不再有"危险命令 / 规则 id / denied"——规则 id
// 只在对**显式组合** `gate.CommandRiskGate` 的宿主里才有意义。
//
// 这条用例走完整 agent→gate→工具链路（不是手工 stub），端到端价值是"批准即执行"。
func TestDangerousCommandRuleIDIsAdvisoryOnlyEndToEnd(t *testing.T) {
	asker := &allowAsker{}
	g := gate.PersonalAsk(gate.Ask(asker))
	outcome := runGatedTool(t, g, "bash", `{"command":"rm -rf /"}`)

	if asker.asks != 1 {
		t.Fatalf("审批人被问 %d 次，want 1（危险命令必须问人）", asker.asks)
	}
	if outcome.Invocations != 1 {
		t.Fatalf("工具被执行 %d 次，want 1（人批准后必须真的执行）", outcome.Invocations)
	}
	text := outcome.Message.Text()
	for _, forbidden := range []string{"rm.recursive-root", "危险命令", "denied"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("工具结果里不应再出现 %q（命令分类已移出默认门禁）: %q", forbidden, text)
		}
	}
	if !strings.Contains(text, "ok:bash") {
		t.Fatalf("工具结果应当是执行结果而不是拒绝文案: %q", text)
	}
	t.Logf("真实输出（ask + 人批准）: %s", text)
}

// TestInterpreterWrappedDangerousCommandIsAdvisoryOnly（版本 B：auto）。
//
// auto = AllowAll：解释器包装（`bash -c '…'`）与裸命令都没有区别，也不再需要
// "理由"——不问人、直接执行。想恢复命令级拦截的宿主可以显式写
// `gate.CommandRiskGate(gate.PersonalAsk(...))`（见 gate.CommandRiskGate 的文档）。
func TestInterpreterWrappedDangerousCommandIsAdvisoryOnly(t *testing.T) {
	asker := &allowAsker{}
	g := gate.PersonalAuto()
	outcome := runGatedTool(t, g, "bash", `{"command":"bash -c 'rm -rf /'"}`)

	if asker.asks != 0 {
		t.Fatalf("auto 档不该问人，实际被问 %d 次", asker.asks)
	}
	if outcome.Invocations != 1 {
		t.Fatalf("工具被执行 %d 次，want 1（auto 直接放行）", outcome.Invocations)
	}
	text := outcome.Message.Text()
	if strings.Contains(text, "rm.recursive-root") || strings.Contains(text, "denied") {
		t.Fatalf("auto 档的工具结果不该是拒绝文案: %q", text)
	}
	if !strings.Contains(text, "ok:bash") {
		t.Fatalf("工具结果应当是执行结果: %q", text)
	}
	t.Logf("真实输出（auto）: %s", text)
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
