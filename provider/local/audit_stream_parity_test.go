package local

// 本文件是"个人用户真的能用它吗"的审计用例（审计者视角，只新增测试、不改实现）：
// 用**模拟真实 ollama / LM Studio 响应**的 httptest server 跑完整 Stream，并把
// 结果与 provider/openai（官方托管语义）逐项比对——"换模型不坏"的判据就是两者
// 装配出来的 assistant 文本与 tool_calls 完全一致。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/provider/openai"
)

// auditAssembly 是"agent 视角"的装配结果：文本、推理、按 Index 装配的工具调用、
// 结束原因、用量。装配逻辑与 agent.runStream 一致（按 delta.Index 分组、参数拼接）。
type auditAssembly struct {
	Text      string
	Reasoning string
	Calls     []string
	Finish    string
	Usage     *rig.Usage
}

func assembleStream(t *testing.T, stream rig.Stream) auditAssembly {
	t.Helper()
	var out auditAssembly
	var text, reasoning strings.Builder
	args := make(map[int]*strings.Builder)
	names := make(map[int]string)
	ids := make(map[int]string)

	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		text.WriteString(chunk.Delta)
		reasoning.WriteString(chunk.Reasoning)
		for _, delta := range chunk.ToolCalls {
			if args[delta.Index] == nil {
				args[delta.Index] = &strings.Builder{}
			}
			if delta.Name != "" {
				names[delta.Index] = delta.Name
			}
			if delta.ID != "" {
				ids[delta.Index] = delta.ID
			}
			args[delta.Index].WriteString(delta.Arguments)
		}
		if chunk.FinishReason != "" {
			out.Finish = chunk.FinishReason
		}
		if chunk.Usage != nil {
			usage := *chunk.Usage
			out.Usage = &usage
		}
	}
	out.Text = text.String()
	out.Reasoning = reasoning.String()

	indexes := make([]int, 0, len(args))
	for index := range args {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	for _, index := range indexes {
		out.Calls = append(out.Calls, fmt.Sprintf("%s/%s(%s)", ids[index], names[index], args[index].String()))
	}
	return out
}

// localStream 用给定 handler 起一个 local provider 并开流。
func localStream(t *testing.T, mutate func(*Config), handler http.HandlerFunc) rig.Stream {
	t.Helper()
	provider := newTestProvider(t, mutate, handler)
	stream, err := provider.Stream(context.Background(), rig.Request{Model: "m"})
	if err != nil {
		t.Fatalf("local.Stream: %v", err)
	}
	t.Cleanup(func() {
		if closer, ok := stream.(io.Closer); ok {
			_ = closer.Close()
		}
	})
	return stream
}

// openAIStream 用给定 handler 起一个 provider/openai 并开流（对照基准）。
func openAIStream(t *testing.T, handler http.HandlerFunc) rig.Stream {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	provider, err := openai.New(openai.Config{
		BaseURL:      server.URL + "/v1",
		APIKey:       "audit-key",
		DefaultModel: "m",
		HTTPClient:   server.Client(),
		RetryBackoff: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("openai.New: %v", err)
	}
	stream, err := provider.Stream(context.Background(), rig.Request{Model: "m"})
	if err != nil {
		t.Fatalf("openai.Stream: %v", err)
	}
	t.Cleanup(func() {
		if closer, ok := stream.(io.Closer); ok {
			_ = closer.Close()
		}
	})
	return stream
}

// sseLines 按 SSE 写事件，行分隔符与是否注入注释可配置（模拟代理注入的 `: ping`）。
func sseLines(w http.ResponseWriter, eol string, comment string, done bool, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	write := func(text string) {
		_, _ = io.WriteString(w, text)
		if flusher != nil {
			flusher.Flush()
		}
	}
	if comment != "" {
		write(comment + eol + eol)
	}
	for _, event := range events {
		write("data: " + event + eol + eol)
	}
	if done {
		write("data: [DONE]" + eol + eol)
	}
}

// assertParity 断言本地装配结果与 openai 逐字段一致。
func assertParity(t *testing.T, got, want auditAssembly) {
	t.Helper()
	if got.Text != want.Text {
		t.Fatalf("文本不一致（换模型会坏）:\n local  = %q\n openai = %q", got.Text, want.Text)
	}
	if got.Reasoning != want.Reasoning {
		t.Fatalf("推理不一致:\n local  = %q\n openai = %q", got.Reasoning, want.Reasoning)
	}
	if strings.Join(got.Calls, "|") != strings.Join(want.Calls, "|") {
		t.Fatalf("工具调用不一致（换模型会坏）:\n local  = %v\n openai = %v", got.Calls, want.Calls)
	}
	if got.Finish != want.Finish {
		t.Fatalf("finish_reason 不一致: local=%q openai=%q", got.Finish, want.Finish)
	}
}

// 审计 1：ollama 的"最后一帧给整条 message 而不是 delta"，且不发 [DONE]。
// 与官方 openai（标准 delta + [DONE]）的装配结果必须一致。
func TestAuditParity_OllamaMessageTailForm(t *testing.T) {
	got := assembleStream(t, localStream(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		sseLines(w, "\n", "", false,
			`{"choices":[{"message":{"role":"assistant","content":"hello world"},"finish_reason":"stop"}]}`,
		)
	}))
	want := assembleStream(t, openAIStream(t, func(w http.ResponseWriter, _ *http.Request) {
		sseLines(w, "\n", "", true,
			`{"choices":[{"delta":{"role":"assistant","content":"hello world"},"finish_reason":"stop"}]}`,
		)
	}))
	assertParity(t, got, want)
	if got.Text != "hello world" {
		t.Fatalf("message 形态没有被采纳: %q", got.Text)
	}
}

// 审计 2：tool_calls 的 arguments 是 **JSON 对象**（ollama / llama.cpp 的常见写法），
// 与官方 openai 的"字符串分片"结果必须一致。
func TestAuditParity_ToolCallArgumentsAsObject(t *testing.T) {
	got := assembleStream(t, localStream(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		sseLines(w, "\n", "", false,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":{"path":"a.txt","limit":10}}}]},"finish_reason":"tool_calls"}]}`,
		)
	}))
	want := assembleStream(t, openAIStream(t, func(w http.ResponseWriter, _ *http.Request) {
		sseLines(w, "\n", "", true,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a.txt\",\"limit\":10}"}}]},"finish_reason":"tool_calls"}]}`,
		)
	}))
	assertParity(t, got, want)
	if len(got.Calls) != 1 || !strings.Contains(got.Calls[0], `"path":"a.txt"`) {
		t.Fatalf("对象型 arguments 没有被归一: %v", got.Calls)
	}
}

// 审计 3：index 缺失（单次调用）应当等价于 index 0。
func TestAuditParity_ToolCallMissingIndexSingle(t *testing.T) {
	got := assembleStream(t, localStream(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		sseLines(w, "\n", "", false,
			`{"choices":[{"delta":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"glob","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
		)
	}))
	want := assembleStream(t, openAIStream(t, func(w http.ResponseWriter, _ *http.Request) {
		sseLines(w, "\n", "", true,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"glob","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
		)
	}))
	assertParity(t, got, want)
}

// 审计 4（已修）：同一事件里两个 tool_calls 且都不带 index —— 必须装配成 2 个调用。
// 修复前两者都落到 index 0，ID/Name 被后者覆盖、arguments 被拼接成非法 JSON。
func TestAuditIndex_TwoToolCallsWithoutIndexStaySeparate(t *testing.T) {
	got := assembleStream(t, localStream(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		sseLines(w, "\n", "", false,
			`{"choices":[{"delta":{"tool_calls":[{"id":"call_a","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a\"}"}},{"id":"call_b","type":"function","function":{"name":"glob","arguments":"{\"pattern\":\"*.go\"}"}}]},"finish_reason":"tool_calls"}]}`,
		)
	}))
	if len(got.Calls) != 2 {
		t.Fatalf("装配出 %d 个调用（应当 2 个，index 缺失不能把调用塌在一起）: %v", len(got.Calls), got.Calls)
	}
	if !strings.Contains(got.Calls[0], "read_file") || !strings.Contains(got.Calls[0], `"path":"a"`) {
		t.Fatalf("第 1 个调用错了: %v", got.Calls)
	}
	if !strings.Contains(got.Calls[1], "glob") || !strings.Contains(got.Calls[1], `"pattern":"*.go"`) {
		t.Fatalf("第 2 个调用错了: %v", got.Calls)
	}
}

// 审计 4b（已修）：混合形态——第一个显式 index 0、第二个缺失 → 第二个补成 1。
func TestAuditIndex_MixedExplicitAndMissing(t *testing.T) {
	got := assembleStream(t, localStream(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		sseLines(w, "\n", "", false,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"read_file","arguments":"{}"}},{"id":"call_b","type":"function","function":{"name":"glob","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
		)
	}))
	if len(got.Calls) != 2 {
		t.Fatalf("混合形态装配出 %d 个调用: %v", len(got.Calls), got.Calls)
	}
	if !strings.Contains(got.Calls[0], "read_file") || !strings.Contains(got.Calls[1], "glob") {
		t.Fatalf("混合形态装配错误: %v", got.Calls)
	}
}

// 审计 4c（已修）：跨事件增量拼接（真实流式形态）仍必须正确。
func TestAuditIndex_IncrementalAcrossEvents(t *testing.T) {
	got := assembleStream(t, localStream(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		sseLines(w, "\n", "", false,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"read_file","arguments":"{\"path\":\""}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"main.go\"}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		)
	}))
	if len(got.Calls) != 1 {
		t.Fatalf("增量形态装出了 %d 个调用: %v", len(got.Calls), got.Calls)
	}
	if !strings.Contains(got.Calls[0], `{"path":"main.go"}`) {
		t.Fatalf("跨事件参数拼接错误: %v", got.Calls)
	}
}

// 审计 5：没有 [DONE]、只有 finish_reason（ollama 默认行为）。
func TestAuditParity_NoDoneOnlyFinishReason(t *testing.T) {
	got := assembleStream(t, localStream(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		sseLines(w, "\n", "", false,
			`{"choices":[{"delta":{"content":"abc"}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		)
	}))
	want := assembleStream(t, openAIStream(t, func(w http.ResponseWriter, _ *http.Request) {
		sseLines(w, "\n", "", true,
			`{"choices":[{"delta":{"content":"abc"}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		)
	}))
	assertParity(t, got, want)
}

// 审计 6：usage 缺 total_tokens —— local 会按 prompt+completion 推导；
// openai 原样留 0。这里记录**两者不一致**（local 更正确），并断言 local 的行为。
func TestAuditUsage_MissingTotalTokensDerived(t *testing.T) {
	got := assembleStream(t, localStream(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		sseLines(w, "\n", "", false,
			`{"choices":[{"delta":{"content":"x"}}]}`,
			`{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7}}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		)
	}))
	if got.Usage == nil {
		t.Fatal("没有解析到 usage")
	}
	if got.Usage.TotalTokens != 18 {
		t.Fatalf("local 应推导 total = 11+7 = 18，实际 %d", got.Usage.TotalTokens)
	}

	want := assembleStream(t, openAIStream(t, func(w http.ResponseWriter, _ *http.Request) {
		sseLines(w, "\n", "", true,
			`{"choices":[{"delta":{"content":"x"}}]}`,
			`{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7}}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		)
	}))
	// 已修：两个 provider 都必须推导 total_tokens（否则 costmeter 会因路径不同算错）。
	if want.Usage.TotalTokens != 18 {
		t.Fatalf("openai provider 没有推导 total_tokens: %+v", want.Usage)
	}
	if *want.Usage != *got.Usage {
		t.Fatalf("两个 provider 用量不一致: openai=%+v local=%+v", want.Usage, got.Usage)
	}
}

// 审计 7：cached_tokens 的两种拼写（嵌套 prompt_tokens_details / 扁平
// prompt_cache_hit_tokens）都要落到 rig.Usage.CachedTokens。
func TestAuditUsage_CachedTokensTwoSpellings(t *testing.T) {
	cases := map[string]string{
		"嵌套 prompt_tokens_details.cached_tokens": `{"prompt_tokens":100,"completion_tokens":5,"total_tokens":105,"prompt_tokens_details":{"cached_tokens":64}}`,
		"扁平 prompt_cache_hit_tokens":             `{"prompt_tokens":100,"completion_tokens":5,"total_tokens":105,"prompt_cache_hit_tokens":64}`,
	}
	for name, usage := range cases {
		t.Run(name, func(t *testing.T) {
			got := assembleStream(t, localStream(t, nil, func(w http.ResponseWriter, _ *http.Request) {
				sseLines(w, "\n", "", false,
					`{"choices":[{"delta":{"content":"x"}}]}`,
					`{"choices":[],"usage":`+usage+`}`,
					`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
				)
			}))
			if got.Usage == nil || got.Usage.CachedTokens != 64 {
				t.Fatalf("cached_tokens 没解出来: %+v", got.Usage)
			}
		})
	}
}

// 审计 8：代理注入的注释行 + CRLF 行尾不能破坏解析，结果必须与纯 LF 完全一致。
func TestAuditParity_CRLFAndCommentLines(t *testing.T) {
	events := []string{
		`{"choices":[{"delta":{"content":"line1"}}]}`,
		`{"choices":[{"delta":{"content":" line2"},"finish_reason":"stop"}]}`,
	}
	plain := assembleStream(t, localStream(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		sseLines(w, "\n", "", false, events...)
	}))
	messy := assembleStream(t, localStream(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		sseLines(w, "\r\n", ": keep-alive", false, events...)
	}))
	assertParity(t, messy, plain)
	if plain.Text != "line1 line2" {
		t.Fatalf("文本 = %q", plain.Text)
	}
}

// 审计 9：finish_reason 的本地变体（tool_call / max_tokens / eos）要归一化成
// rig 约定的三个值，否则 agent 侧"是否 tool_calls"的判断会失效。
func TestAuditFinishReasonAliases(t *testing.T) {
	cases := map[string]string{
		"tool_call":  "tool_calls",
		"max_tokens": "length",
		"eos":        "stop",
		"stop":       "stop",
	}
	for raw, want := range cases {
		t.Run(raw, func(t *testing.T) {
			got := assembleStream(t, localStream(t, nil, func(w http.ResponseWriter, _ *http.Request) {
				sseLines(w, "\n", "", false,
					`{"choices":[{"delta":{"content":"x"},"finish_reason":"`+raw+`"}]}`,
				)
			}))
			if got.Finish != want {
				t.Fatalf("finish_reason %q 归一化 = %q, want %q", raw, got.Finish, want)
			}
		})
	}
}
