package kit_test

// 事件 payload 不再有损（E6）：rig.ToolResult.Data（宿主的结构化结果）必须出现在
// tool.result 事件的 payload 里；不可序列化的 Data 不能让事件桥 panic，也不能
// 丢掉整条工具结果——流里要留一条 notice 说明原因。

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/kit"
)

// TestEventStreamIncludesToolResultData：工具返回 Data（map）时，事件流里能读到
// 同一份结构化结果。
func TestEventStreamIncludesToolResultData(t *testing.T) {
	var buffer bytes.Buffer
	tool := &fakeTool{name: "run", fn: func(context.Context, kit.ToolCall) (kit.ToolResult, error) {
		return kit.ToolResult{Content: "exit status 1", IsError: true, Data: map[string]any{"exit": 1}}, nil
	}}
	provider := &scriptProvider{scripts: [][]kit.Chunk{
		toolCallScript(kit.ToolCall{ID: "c1", Name: "run", Arguments: `{}`}),
		textScript("done"),
	}}
	a := mustNew(t, kit.Config{
		Model:       "m",
		Provider:    provider,
		Root:        t.TempDir(),
		Tools:       []kit.Tool{tool},
		Approval:    kit.ApprovalAuto,
		EventWriter: &buffer,
	})
	if _, _, err := a.RunText(context.Background(), "s1", "go", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	stream := buffer.String()
	if !strings.Contains(stream, `"kind":"tool.result"`) {
		t.Fatalf("事件流里没有 tool.result：\n%s", stream)
	}
	if !strings.Contains(stream, `"data":{"exit":1}`) {
		t.Fatalf("tool.result 丢了结构化结果：\n%s", stream)
	}
	if err := a.EventError(); err != nil {
		t.Fatalf("EventError = %v", err)
	}
}

// TestEventStreamHandlesUnserializableData：Data 不可 JSON 序列化时不 panic、
// 事件流保留 tool.result，并附一条说明性 notice。
func TestEventStreamHandlesUnserializableData(t *testing.T) {
	var buffer bytes.Buffer
	tool := &fakeTool{name: "run", fn: func(context.Context, kit.ToolCall) (kit.ToolResult, error) {
		return kit.ToolResult{Content: "did something", Data: make(chan int)}, nil
	}}
	provider := &scriptProvider{scripts: [][]kit.Chunk{
		toolCallScript(kit.ToolCall{ID: "c1", Name: "run", Arguments: `{}`}),
		textScript("done"),
	}}
	a := mustNew(t, kit.Config{
		Model:       "m",
		Provider:    provider,
		Root:        t.TempDir(),
		Tools:       []kit.Tool{tool},
		Approval:    kit.ApprovalAuto,
		EventWriter: &buffer,
	})
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Fatalf("不可序列化的 Data 让事件桥 panic 了: %v", recovered)
			}
		}()
		if _, _, err := a.RunText(context.Background(), "s1", "go", nil); err != nil {
			t.Fatalf("RunText: %v", err)
		}
	}()
	stream := buffer.String()
	if !strings.Contains(stream, `"kind":"tool.result"`) {
		t.Fatalf("不可序列化的 Data 让 tool.result 整条消失了：\n%s", stream)
	}
	if !strings.Contains(stream, "tool result data was not serializable") {
		t.Fatalf("事件流里没有说明 notice：\n%s", stream)
	}
	if !strings.Contains(stream, "did something") {
		t.Fatalf("工具结果文本被丢掉了：\n%s", stream)
	}
	if err := a.EventError(); err != nil {
		t.Fatalf("EventError = %v", err)
	}
}

// TestEventStreamOmitsTypedNilToolResultData：Data 是 typed-nil（(*T)(nil)、
// json.RawMessage(nil)）时按"没有结构化结果"处理——payload 里不能出现
// "data":null（events.ToolResult.Data 的文档承诺 nil 时省略字段）。
func TestEventStreamOmitsTypedNilToolResultData(t *testing.T) {
	cases := []struct {
		name string
		data any
	}{
		{"typed-nil-pointer", (*struct{})(nil)},
		{"nil-raw-message", json.RawMessage(nil)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buffer bytes.Buffer
			data := tc.data
			tool := &fakeTool{name: "run", fn: func(context.Context, kit.ToolCall) (kit.ToolResult, error) {
				return kit.ToolResult{Content: "text", Data: data}, nil
			}}
			provider := &scriptProvider{scripts: [][]kit.Chunk{
				toolCallScript(kit.ToolCall{ID: "c1", Name: "run", Arguments: `{}`}),
				textScript("done"),
			}}
			a := mustNew(t, kit.Config{
				Model:       "m",
				Provider:    provider,
				Root:        t.TempDir(),
				Tools:       []kit.Tool{tool},
				Approval:    kit.ApprovalAuto,
				EventWriter: &buffer,
			})
			if _, _, err := a.RunText(context.Background(), "s1", "go", nil); err != nil {
				t.Fatalf("RunText: %v", err)
			}
			found := false
			for _, line := range strings.Split(strings.TrimSpace(buffer.String()), "\n") {
				var envelope struct {
					Kind    string          `json:"kind"`
					Payload json.RawMessage `json:"payload"`
				}
				if err := json.Unmarshal([]byte(line), &envelope); err != nil {
					t.Fatalf("事件行不是合法 JSON（%v）：%s", err, line)
				}
				if envelope.Kind != "tool.result" {
					continue
				}
				found = true
				var payload map[string]any
				if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
					t.Fatalf("tool.result payload 不是合法 JSON：%v", err)
				}
				if _, ok := payload["data"]; ok {
					t.Fatalf("typed-nil Data 仍然写出了 data 字段：%s", line)
				}
			}
			if !found {
				t.Fatalf("事件流里没有 tool.result：\n%s", buffer.String())
			}
		})
	}
}
