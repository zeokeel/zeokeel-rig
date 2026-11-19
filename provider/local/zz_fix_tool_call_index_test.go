package local

// zz_fix_tool_call_index_test.go —— 修复 tool_calls 缺 index 时塌到 0 的回归测试。
//
// 现象：逐事件到达、各自带不同 id 的 tool_call delta 都没有 index 时，旧实现用
// "事件内位置"补 index，于是每个事件都落到 index 0：id/name 互相覆盖，arguments
// 首尾相接成非法 JSON。

import (
	"context"
	"encoding/json"
	"io"
	"sort"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
)

// collectToolCalls 读取整条流并按 agent 的方式装配（Index → 槽位，arguments 拼接）。
func collectToolCalls(t *testing.T, sse string) ([]rig.ToolCall, error) {
	t.Helper()
	stream := NewStream(context.Background(), StreamOptions{
		Body: io.NopCloser(strings.NewReader(sse)),
	})
	type slot struct {
		id, name string
		args     strings.Builder
	}
	slots := map[int]*slot{}
	order := []int{}
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		for _, delta := range chunk.ToolCalls {
			entry, ok := slots[delta.Index]
			if !ok {
				entry = &slot{}
				slots[delta.Index] = entry
				order = append(order, delta.Index)
			}
			if delta.ID != "" {
				entry.id = delta.ID
			}
			if delta.Name != "" {
				entry.name = delta.Name
			}
			entry.args.WriteString(delta.Arguments)
		}
	}
	sort.Ints(order)
	calls := make([]rig.ToolCall, 0, len(order))
	for _, index := range order {
		entry := slots[index]
		calls = append(calls, rig.ToolCall{ID: entry.id, Name: entry.name, Arguments: entry.args.String()})
	}
	return calls, nil
}

// 两个不同 id、分事件到达、都没有 index → 必须装配成两个调用。
func TestToolCallDeltasMergeByIDWhenIndexMissing(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"a","function":{"name":"f","arguments":"{\"x\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"b","function":{"name":"g","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"a","function":{"arguments":"1}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		``,
	}, "\n\n")

	calls, err := collectToolCalls(t, sse)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("装配出 %d 个调用, want 2: %+v", len(calls), calls)
	}
	if calls[0].ID != "a" || calls[0].Name != "f" || calls[0].Arguments != `{"x":1}` {
		t.Fatalf("第一个调用 = %+v, want {a f {\"x\":1}}", calls[0])
	}
	if calls[1].ID != "b" || calls[1].Name != "g" || calls[1].Arguments != "{}" {
		t.Fatalf("第二个调用 = %+v, want {b g {}}", calls[1])
	}
	for _, call := range calls {
		if !json.Valid([]byte(call.Arguments)) {
			t.Fatalf("arguments 不是合法 JSON：%q", call.Arguments)
		}
	}
}

// 显式 index 仍然权威：带 index 的流必须按 index 归并（同一 index 的 id 相同）。
func TestToolCallDeltasRespectExplicitIndex(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"b","function":{"name":"g","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"f","arguments":"{\"x\":1}"}}]}}]}`,
		`data: [DONE]`,
		``,
	}, "\n\n")
	calls, err := collectToolCalls(t, sse)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if len(calls) != 2 || calls[0].ID != "a" || calls[1].ID != "b" {
		t.Fatalf("显式 index 应按 0/1 归并，得到 %+v", calls)
	}
}

// 既无 index、也无 id/name 且已存在多个槽位 → 无法归属，必须报错而不是静默拼接。
func TestToolCallDeltasAmbiguousDeltaIsAnError(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"a","function":{"name":"f","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"b","function":{"name":"g","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"{\"y\""}}]}}]}`,
		`data: [DONE]`,
		``,
	}, "\n\n")
	if _, err := collectToolCalls(t, sse); err == nil {
		t.Fatal("无法归属的 delta 必须报错，而不是静默拼接到某个调用上")
	} else {
		t.Logf("got expected error: %v", err)
	}
}
