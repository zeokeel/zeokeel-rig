package agent_test

// ToolResult.Data 是"给宿主的结构化结果"：模型仍然只看 Content，但宿主的
// 状态栏 / 任务卡 / 事件订阅需要 id、状态、退出码这类字段，不该去解析人读文本。
//
// 本用例钉住两个契约：① 工具填的 Data 原样到达 EventToolResult；② 它**不**进
// 模型消息（模型看到的内容必须仍然只有 Content，否则 token 与格式都不可控）。

import (
	"context"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
)

func TestToolResultDataReachesTheEvent(t *testing.T) {
	provider := &scriptProvider{scripts: [][]rig.Chunk{
		toolCallScript("call_1", "job_start", `{}`),
		textScript("done"),
	}}
	job := &fakeTool{
		name: "job_start",
		fn: func(_ context.Context, _ rig.ToolCall) (rig.ToolResult, error) {
			return rig.ToolResult{
				Content: "started job 7",
				Data:    map[string]any{"id": "7", "state": "running", "exit_code": 0},
			}, nil
		},
	}
	rec := &recorder{}
	a := mustNewAgent(t, rig.AgentSpec{Model: "m", Provider: provider, Tools: []rig.Tool{job}})

	result, err := a.Run(context.Background(), rig.RunRequest{
		Input: []rig.Message{userMessage("start it")},
		Emit:  rec.emit,
	})
	requireNoError(t, err)

	var toolResult *rig.ToolResult
	for _, event := range rec.list() {
		if event.Type == rig.EventToolResult && event.Result != nil {
			toolResult = event.Result
		}
	}
	if toolResult == nil {
		t.Fatal("没有收到 EventToolResult")
	}
	data, ok := toolResult.Data.(map[string]any)
	if !ok {
		t.Fatalf("Data 类型丢了：%#v", toolResult.Data)
	}
	if data["state"] != "running" || data["id"] != "7" {
		t.Fatalf("Data 内容不对：%#v", data)
	}

	// 模型面只看 Content。
	for _, message := range result.Messages {
		if message.Role != rig.RoleTool {
			continue
		}
		if got := message.Text(); got != "started job 7" {
			t.Fatalf("tool 消息应只含 Content，实际 %q", got)
		}
	}
}
