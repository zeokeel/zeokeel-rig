package agent_test

// 审计 2C/2F 端到端：**门禁缺配置不能让 run 死掉**。
//
// 曾经的行为：`gate.PersonalDefault(nil)` 在"需要审批"时返回 Go error → agent 中止
// 整个 run → 模型没有下一轮、用户看到"运行失败"。一次配置疏漏（忘了传 inner/Asker）
// 就能杀掉整个会话，这是个人客户端最差的失败模式。
//
// 现在：门禁返回 DecisionDeny + 理由，agent 把它当普通工具错误回给模型，run 继续。

import (
	"context"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/gate"
)

// TestGateWithoutApprovalChannelKeepsRunAlive 断言 run 成功、模型看到理由、并且
// **确实有下一轮**（provider 被调用了两次）。
func TestGateWithoutApprovalChannelKeepsRunAlive(t *testing.T) {
	bashTool := &fakeTool{name: "bash", desc: "run a shell command", schema: map[string]any{"type": "object"}}
	provider := &scriptProvider{scripts: [][]rig.Chunk{
		// 第一轮：模型想跑一条需要审批的可变命令。
		toolCallScript("call_1", "bash", `{"command":"rm -rf ./build"}`),
		// 第二轮：模型看到拒绝理由后收尾（证明对话没有中断）。
		textScript("明白，我不重试了"),
	}}
	a := mustNewAgent(t, rig.AgentSpec{
		Model:    "m",
		Provider: provider,
		Tools:    []rig.Tool{bashTool},
		Gate:     gate.PersonalDefault(nil), // 故意不配 inner
	})

	result, err := a.Run(context.Background(), rig.RunRequest{Input: []rig.Message{userMessage("清理构建产物")}})
	if err != nil {
		t.Fatalf("run 不该因为门禁缺配置而失败（那会让模型没有下一轮）: %v", err)
	}
	if len(provider.scripts) > 0 && provider.callCount() < 2 {
		t.Fatalf("provider 只被调用 %d 次，模型没有拿到拒绝理由后的下一轮", provider.callCount())
	}

	// 模型侧的 tool 消息必须带上"没有配置审批通道"的理由。
	var toolMessage *rig.Message
	for index := range result.Messages {
		if result.Messages[index].Role == rig.RoleTool {
			toolMessage = &result.Messages[index]
			break
		}
	}
	if toolMessage == nil {
		t.Fatalf("没有 tool 消息: %+v", result.Messages)
	}
	text := toolMessage.Text()
	if !strings.Contains(text, "没有配置审批通道") {
		t.Fatalf("tool 消息里没有「没有配置审批通道」的理由: %q", text)
	}
	if !strings.Contains(text, "tool call denied") {
		t.Fatalf("tool 消息应当标明是审批拒绝: %q", text)
	}
	// 最后一条是模型的收尾文本，说明它真的继续了。
	if got := result.Messages[len(result.Messages)-1].Text(); !strings.Contains(got, "不重试") {
		t.Fatalf("最后一轮不是模型的收尾消息: %q", got)
	}
	t.Logf("模型看到的拒绝消息: %s", text)
}

// TestGateWithoutApproverAskNilKeepsRunAlive 断言 gate.Ask(nil) 同样不中断 run。
func TestGateWithoutApproverAskNilKeepsRunAlive(t *testing.T) {
	bashTool := &fakeTool{name: "bash", desc: "run a shell command", schema: map[string]any{"type": "object"}}
	provider := &scriptProvider{scripts: [][]rig.Chunk{
		toolCallScript("call_1", "bash", `{"command":"ls -la"}`),
		textScript("好"),
	}}
	a := mustNewAgent(t, rig.AgentSpec{
		Model: "m", Provider: provider, Tools: []rig.Tool{bashTool},
		Gate: gate.Ask(nil), // 没有 Asker
	})
	result, err := a.Run(context.Background(), rig.RunRequest{Input: []rig.Message{userMessage("看看目录")}})
	if err != nil {
		t.Fatalf("Ask(nil) 不该让 run 失败: %v", err)
	}
	for _, message := range result.Messages {
		if message.Role == rig.RoleTool {
			if !strings.Contains(message.Text(), "没有可用的审批通道") {
				t.Fatalf("拒绝理由应当是「没有可用的审批通道」: %q", message.Text())
			}
			return
		}
	}
	t.Fatalf("没有 tool 消息: %+v", result.Messages)
}
