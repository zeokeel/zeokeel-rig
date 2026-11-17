package openai

// 工具调用 index 的审计用例（TDD 补充）：`index` 缺失时不能把多个调用塌成一个。
// 这是 openai 与 local 共用的设计缺陷（`Index int` 的零值无法区分"0"与"缺失"）。

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
)

// assembleCalls 用与 agent 相同的方式（按 delta.Index 分组、参数拼接）装配工具调用。
func assembleCalls(t *testing.T, stream rig.Stream) []string {
	t.Helper()
	args := map[int]*strings.Builder{}
	names := map[int]string{}
	ids := map[int]string{}
	order := []int{}
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		for _, delta := range chunk.ToolCalls {
			if args[delta.Index] == nil {
				args[delta.Index] = &strings.Builder{}
				order = append(order, delta.Index)
			}
			if delta.Name != "" {
				names[delta.Index] = delta.Name
			}
			if delta.ID != "" {
				ids[delta.Index] = delta.ID
			}
			args[delta.Index].WriteString(delta.Arguments)
		}
	}
	calls := make([]string, 0, len(order))
	for _, index := range order {
		calls = append(calls, ids[index]+"/"+names[index]+"("+args[index].String()+")")
	}
	return calls
}

// 一个事件里两个调用都不带 index → 必须装配成 **2 个** 调用。
func TestAuditIndex_TwoCallsWithoutIndexStaySeparate(t *testing.T) {
	provider := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		writeSSE(w,
			`{"choices":[{"delta":{"tool_calls":[{"id":"call_a","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a\"}"}},{"id":"call_b","type":"function","function":{"name":"glob","arguments":"{\"pattern\":\"*.go\"}"}}]},"finish_reason":"tool_calls"}]}`,
		)
	})
	stream, err := provider.Stream(context.Background(), rig.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() {
		if closer, ok := stream.(io.Closer); ok {
			_ = closer.Close()
		}
	}()

	calls := assembleCalls(t, stream)
	if len(calls) != 2 {
		t.Fatalf("装配出 %d 个调用（应当 2 个，否则 index 缺失会把调用塌在一起）: %v", len(calls), calls)
	}
	if !strings.Contains(calls[0], "read_file") || !strings.Contains(calls[0], `"path":"a"`) {
		t.Fatalf("第 1 个调用错了: %v", calls)
	}
	if !strings.Contains(calls[1], "glob") || !strings.Contains(calls[1], `"pattern":"*.go"`) {
		t.Fatalf("第 2 个调用错了: %v", calls)
	}
}

// 混合：第一个显式 index 0、第二个缺失 → 第二个应补成 1。
func TestAuditIndex_MixedExplicitAndMissing(t *testing.T) {
	provider := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		writeSSE(w,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"read_file","arguments":"{}"}},{"id":"call_b","type":"function","function":{"name":"glob","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
		)
	})
	stream, err := provider.Stream(context.Background(), rig.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() {
		if closer, ok := stream.(io.Closer); ok {
			_ = closer.Close()
		}
	}()
	calls := assembleCalls(t, stream)
	if len(calls) != 2 {
		t.Fatalf("混合形态装配出 %d 个调用: %v", len(calls), calls)
	}
	if !strings.Contains(calls[0], "read_file") || !strings.Contains(calls[1], "glob") {
		t.Fatalf("混合形态装配错误: %v", calls)
	}
}

// 跨事件增量拼接（真实流式形态）：index 显式给出时，片段必须累加到同一个调用。
func TestAuditIndex_IncrementalAcrossEvents(t *testing.T) {
	provider := newTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		writeSSE(w,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"read_file","arguments":"{\"path\":\""}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"main.go\"}"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"glob","arguments":"{}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		)
	})
	stream, err := provider.Stream(context.Background(), rig.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() {
		if closer, ok := stream.(io.Closer); ok {
			_ = closer.Close()
		}
	}()
	calls := assembleCalls(t, stream)
	if len(calls) != 2 {
		t.Fatalf("增量形态装配出 %d 个调用: %v", len(calls), calls)
	}
	if !strings.Contains(calls[0], `{"path":"main.go"}`) {
		t.Fatalf("跨事件参数拼接错误: %v", calls)
	}
}
