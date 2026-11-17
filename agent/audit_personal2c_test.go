package agent_test

// 个人客户端审计（第 2C 轮）：拒绝理由通道在"个人客户端最常见三条路径"上的表现。
//
//   - 人在审批里点了 deny → 模型看到的文案是否说明"是人工拒绝的"；
//   - 门禁配置错误（返回 Go error）→ 会不会把**整个 run** 干掉；
//   - 危险命令的文案是否足以让模型换方案（已有 denial_test 覆盖，这里只做端到端回放）。

import (
	"context"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/gate"
)

// denyAsker 永远回答 deny（模拟用户在审批弹窗里点了拒绝）。
type denyAsker struct{ asks int }

func (a *denyAsker) Ask(context.Context, string, []string) (string, error) {
	a.asks++
	return "deny", nil
}

// A2C-A1：人工拒绝必须让模型看到"是被人拒绝的"，而不是系统故障。
func TestAudit2CAskDenialReasonReachesModel(t *testing.T) {
	asker := &denyAsker{}
	g := gate.PersonalAsk(gate.Ask(asker))
	outcome := runDeniedTool(t, g, "bash", `{"command":"go test ./..."}`)
	text := outcome.Message.Text()
	if asker.asks != 1 {
		t.Fatalf("审批人被问 %d 次，want 1", asker.asks)
	}
	for _, want := range []string{"denied", "人工审批拒绝"} {
		if !strings.Contains(text, want) {
			t.Fatalf("模型看到的拒绝文案缺 %q：%q", want, text)
		}
	}
	if !outcome.Result.IsError {
		t.Fatal("被拒绝的调用必须是 IsError 的工具结果")
	}
	t.Logf("人工拒绝时模型看到的原文：\n%s", text)
}

// A2C-A2：门禁缺 inner 时必须 **deny + 理由**，而不是中止整个 run。
//
// 历史：这里曾经是 Go error（fail closed），agent 见到 error 会 abort，模型只拿到
// 一条失败、没有下一轮，宿主持到"运行失败"。审计 2C 报告后改为 DecisionDeny +
// 理由文案（同样 fail closed，但对话能继续）。
func TestAudit2CNoApprovalChannelDeniesWithoutAborting(t *testing.T) {
	tool := &fakeTool{name: "bash", desc: "test tool", schema: map[string]any{"type": "object"}}
	provider := &scriptProvider{scripts: [][]rig.Chunk{
		toolCallScript("call_1", "bash", `{"command":"go test ./..."}`),
		textScript("done"),
	}}
	a := mustNewAgent(t, rig.AgentSpec{
		Model: "m", Provider: provider, Tools: []rig.Tool{tool}, Gate: gate.PersonalDefault(nil),
	})
	result, err := a.Run(context.Background(), rig.RunRequest{
		Input: []rig.Message{userMessage("go")},
	})
	if err != nil {
		t.Fatalf("没有 inner 不应中止 run（应为 deny + 理由）：%v", err)
	}
	var toolText string
	for _, message := range result.Messages {
		if message.Role == rig.RoleTool {
			toolText = message.Text()
		}
	}
	if toolText == "" {
		t.Fatalf("模型没收到任何工具消息")
	}
	for _, want := range []string{"denied", "没有配置审批通道"} {
		if !strings.Contains(toolText, want) {
			t.Fatalf("拒绝文案缺 %q：%q", want, toolText)
		}
	}
	if provider.callCount() != 2 {
		t.Fatalf("模型被调用 %d 次，want 2（被拒之后必须还有下一轮去换方案）", provider.callCount())
	}
	t.Logf("模型看到：%s", toolText)
}
