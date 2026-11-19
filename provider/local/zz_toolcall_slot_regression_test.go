package local

// review-1 M1 回归：流级 toolCallIndexer 把「一次调用」算成多个槽位（allocate、id、
// name 各计一次），于是 `slots == 1` 的兜底不可达——任何缺 index/id/name 的后续
// delta 都变成硬错误。真实流里这三种形态很常见：id/name 只在首片、旧式 function_call
// 分片、首片只有 arguments。修复前它们都能装配成功，上一轮改动让整轮失败。

import (
	"strings"
	"testing"
)

func TestSingleCallWithIDOnlyOnFirstChunk(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"call_1","function":{"name":"read_file","arguments":"{\"pa"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"th\":\"a\"}"}}]}}]}`,
		`data: [DONE]`,
		"",
	}, "\n\n")
	calls, err := collectToolCalls(t, sse)
	if err != nil {
		t.Fatalf("单次调用（id/name 只在首片）应当装配成功，却失败：%v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1: %+v", len(calls), calls)
	}
	if calls[0].Name != "read_file" || calls[0].Arguments != `{"path":"a"}` {
		t.Fatalf("call = %+v, want read_file {\"path\":\"a\"}", calls[0])
	}
}

func TestLegacyFunctionCallDeltaSplit(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"function_call":{"name":"f","arguments":"{\"x\""}}}]}`,
		`data: {"choices":[{"delta":{"function_call":{"arguments":":1}"}}}]}`,
		`data: [DONE]`,
		"",
	}, "\n\n")
	calls, err := collectToolCalls(t, sse)
	if err != nil {
		t.Fatalf("旧式 function_call 分片应当装配成功，却失败：%v", err)
	}
	if len(calls) != 1 || calls[0].Name != "f" || calls[0].Arguments != `{"x":1}` {
		t.Fatalf("calls = %+v, want 1 次 f {\"x\":1}", calls)
	}
}

func TestFirstDeltaCarriesOnlyArguments(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"{\"a\""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"function":{"arguments":":1}"}}]}}]}`,
		`data: [DONE]`,
		"",
	}, "\n\n")
	calls, err := collectToolCalls(t, sse)
	if err != nil {
		t.Fatalf("首片只有 arguments 时应能归到唯一槽位，却失败：%v", err)
	}
	if len(calls) != 1 || calls[0].Arguments != `{"a":1}` {
		t.Fatalf("calls = %+v, want 1 次 {\"a\":1}", calls)
	}
}

func TestTwoCallsWithoutIndexStillError(t *testing.T) {
	// 反例守卫：真的无法归属时必须报错，而不是把参数拼在一起。
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"a","function":{"name":"f","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"b","function":{"name":"g","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"{\"x\":1}"}}]}}]}`,
		`data: [DONE]`,
		"",
	}, "\n\n")
	if _, err := collectToolCalls(t, sse); err == nil {
		t.Fatal("两个槽位已开且 delta 无任何标识，应当报错")
	}
}
