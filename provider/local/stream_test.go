package local

// 本文件钉死"本地服务的写法差异"：缺失 [DONE]、finish_reason 变体、tool_calls 的
// 增量/对象/旧式字段、content 分片数组、坏事件容忍、CRLF、ctx 取消。

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

// streamFrom 在一个 test server 上回放给定的 SSE 事件（可按需省略 [DONE]）。
func streamFrom(t *testing.T, mutate func(*Config), withDone bool, events ...string) rig.Stream {
	t.Helper()
	provider := newTestProvider(t, mutate, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, withDone, events...)
	})
	stream, err := provider.Stream(context.Background(), rig.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	t.Cleanup(func() {
		if closer, ok := stream.(io.Closer); ok {
			_ = closer.Close()
		}
	})
	return stream
}

func TestStreamBasic(t *testing.T) {
	stream := streamFrom(t, nil, true,
		`{"choices":[{"delta":{"content":"he"}}]}`,
		`{"choices":[{"delta":{"content":"llo"},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
	)
	chunks := collect(t, stream)
	if got := joinText(chunks); got != "hello" {
		t.Fatalf("text = %q", got)
	}
	if got := finishOf(chunks); got != "stop" {
		t.Fatalf("finish = %q", got)
	}
	var usage *rig.Usage
	for _, chunk := range chunks {
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
	}
	if usage == nil || usage.TotalTokens != 5 || usage.PromptTokens != 3 {
		t.Fatalf("usage = %+v", usage)
	}
}

// TestStreamMissingDoneTolerated 是本地模型最常见的差异：直接关连接、不发 [DONE]。
func TestStreamMissingDoneTolerated(t *testing.T) {
	t.Run("有 finish_reason 无 DONE", func(t *testing.T) {
		stream := streamFrom(t, nil, false,
			`{"choices":[{"delta":{"content":"hi"}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		)
		chunks := collect(t, stream)
		if got := joinText(chunks); got != "hi" {
			t.Fatalf("text = %q", got)
		}
		if got := finishOf(chunks); got != "stop" {
			t.Fatalf("finish = %q", got)
		}
	})
	// 真正的"截断"形态：既没有 finish_reason 也没有 [DONE]。默认必须容忍
	// （本地服务常常这样结束），RequireDone 时才报错。
	t.Run("既无 finish_reason 也无 DONE", func(t *testing.T) {
		stream := streamFrom(t, nil, false,
			`{"choices":[{"delta":{"content":"hi"}}]}`,
		)
		chunks := collect(t, stream)
		if got := joinText(chunks); got != "hi" {
			t.Fatalf("text = %q", got)
		}
	})
}

func TestStreamRequireDoneRejectsTruncated(t *testing.T) {
	provider := newTestProvider(t, func(cfg *Config) { cfg.Quirks.RequireDone = true }, func(w http.ResponseWriter, r *http.Request) {
		// 没有 finish_reason、没有 [DONE]，直接关连接。
		writeSSE(w, false, `{"choices":[{"delta":{"content":"partial"}}]}`)
	})
	stream, err := provider.Stream(context.Background(), rig.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var got error
	for {
		_, recvErr := stream.Recv()
		if recvErr != nil {
			got = recvErr
			break
		}
	}
	if !errors.Is(got, ErrIncompleteStream) {
		t.Fatalf("err = %v, want ErrIncompleteStream", got)
	}
}

func TestStreamFinishReasonAliases(t *testing.T) {
	cases := map[string]string{
		"tool_call":     "tool_calls",
		"function_call": "tool_calls",
		"max_tokens":    "length",
		"stop":          "stop",
		"length":        "length",
	}
	for raw, want := range cases {
		raw, want := raw, want
		t.Run(raw, func(t *testing.T) {
			stream := streamFrom(t, nil, false,
				`{"choices":[{"delta":{"content":"x"},"finish_reason":"`+raw+`"}]}`,
			)
			if got := finishOf(collect(t, stream)); got != want {
				t.Fatalf("finish = %q, want %q", got, want)
			}
		})
	}
}

func TestStreamCustomFinishReasonAlias(t *testing.T) {
	stream := streamFrom(t, func(cfg *Config) {
		cfg.Quirks.FinishReasonAliases = map[string]string{"EOS_TOKEN": "stop"}
	}, false,
		`{"choices":[{"delta":{"content":"x"},"finish_reason":"eos_token"}]}`,
	)
	if got := finishOf(collect(t, stream)); got != "stop" {
		t.Fatalf("finish = %q, want stop（自定义别名）", got)
	}
}

func TestStreamToolCallsIncremental(t *testing.T) {
	stream := streamFrom(t, nil, true,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"echo","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"text\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"hi\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	chunks := collect(t, stream)
	var calls []rig.ToolCallDelta
	for _, chunk := range chunks {
		calls = append(calls, chunk.ToolCalls...)
	}
	if len(calls) == 0 {
		t.Fatal("没有工具调用片段")
	}
	if calls[0].ID != "call_1" || calls[0].Name != "echo" || calls[0].Index != 0 {
		t.Fatalf("首个片段 = %+v", calls[0])
	}
	var arguments strings.Builder
	for _, call := range calls {
		arguments.WriteString(call.Arguments)
	}
	if got := arguments.String(); got != `{"text":"hi"}` {
		t.Fatalf("arguments = %q", got)
	}
	if got := finishOf(chunks); got != "tool_calls" {
		t.Fatalf("finish = %q", got)
	}
}

// TestStreamToolCallArgumentsAsObject 覆盖"一次给完整对象"的服务端。
func TestStreamToolCallArgumentsAsObject(t *testing.T) {
	stream := streamFrom(t, nil, true,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"echo","arguments":{"text":"hi"}}}]}}]}`,
	)
	chunks := collect(t, stream)
	var got string
	for _, chunk := range chunks {
		for _, call := range chunk.ToolCalls {
			got = call.Arguments
		}
	}
	if got != `{"text":"hi"}` {
		t.Fatalf("arguments = %q, want 紧凑 JSON 字符串", got)
	}
}

// TestStreamLegacyFunctionCall 覆盖旧式 function_call（单数、没有 index）。
func TestStreamLegacyFunctionCall(t *testing.T) {
	stream := streamFrom(t, nil, true,
		`{"choices":[{"delta":{"function_call":{"name":"echo","arguments":"{\"a\":1}"}}}]}`,
	)
	chunks := collect(t, stream)
	var calls []rig.ToolCallDelta
	for _, chunk := range chunks {
		calls = append(calls, chunk.ToolCalls...)
	}
	if len(calls) != 1 {
		t.Fatalf("工具调用片段数 = %d, want 1", len(calls))
	}
	if calls[0].Index != 0 || calls[0].Name != "echo" || calls[0].Arguments != `{"a":1}` {
		t.Fatalf("片段 = %+v", calls[0])
	}
}

// TestStreamContentArray 覆盖 content 发成分片数组的服务端。
func TestStreamContentArray(t *testing.T) {
	stream := streamFrom(t, nil, true,
		`{"choices":[{"delta":{"content":[{"type":"text","text":"he"},{"type":"text","text":"llo"}]}}]}`,
	)
	if got := joinText(collect(t, stream)); got != "hello" {
		t.Fatalf("text = %q", got)
	}
}

// TestStreamMessageInsteadOfDelta 覆盖"整条消息放在 message 字段"的服务端。
func TestStreamMessageInsteadOfDelta(t *testing.T) {
	stream := streamFrom(t, nil, true,
		`{"choices":[{"delta":{"content":"a"},"finish_reason":null}]}`,
		`{"choices":[{"message":{"content":"b"},"finish_reason":"stop"}]}`,
	)
	if got := joinText(collect(t, stream)); got != "ab" {
		t.Fatalf("text = %q", got)
	}
}

func TestStreamReasoningKeys(t *testing.T) {
	t.Run("reasoning_content", func(t *testing.T) {
		stream := streamFrom(t, nil, true, `{"choices":[{"delta":{"reasoning_content":"think"}}]}`)
		var reasoning string
		for _, chunk := range collect(t, stream) {
			reasoning += chunk.Reasoning
		}
		if reasoning != "think" {
			t.Fatalf("reasoning = %q", reasoning)
		}
	})
	t.Run("自定义键名", func(t *testing.T) {
		stream := streamFrom(t, func(cfg *Config) {
			cfg.Quirks.ReasoningKeys = []string{"thinking"}
		}, true, `{"choices":[{"delta":{"thinking":"hmm"}}]}`)
		var reasoning string
		for _, chunk := range collect(t, stream) {
			reasoning += chunk.Reasoning
		}
		if reasoning != "hmm" {
			t.Fatalf("reasoning = %q", reasoning)
		}
	})
}

func TestStreamToleratesBadEvents(t *testing.T) {
	stream := streamFrom(t, nil, true,
		`{"choices":[{"delta":{"content":"a"}}]}`,
		`{not json`,
		`{"choices":[{"delta":{"content":"b"}}]}`,
		`{"choices":[{"delta":{"content":12345}}]}`, // content 形状不对：跳过该事件
	)
	if got := joinText(collect(t, stream)); got != "ab" {
		t.Fatalf("text = %q（坏事件必须被跳过）", got)
	}
}

func TestStreamHandlesCRLFAndComments(t *testing.T) {
	provider := newTestProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, ": ping\r\n\r\n")
		_, _ = io.WriteString(w, "event: message\r\n")
		_, _ = io.WriteString(w, "data:{\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\r\n\r\n")
		_, _ = io.WriteString(w, "data: [DONE]\r\n\r\n")
	})
	stream, err := provider.Stream(context.Background(), rig.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := joinText(collect(t, stream)); got != "hi" {
		t.Fatalf("text = %q", got)
	}
}

func TestStreamInBandError(t *testing.T) {
	provider := newTestProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"error\":{\"message\":\"模型未加载\",\"type\":\"server_error\"}}\n\n")
	})
	stream, err := provider.Stream(context.Background(), rig.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	_, recvErr := stream.Recv()
	if recvErr == nil || !strings.Contains(recvErr.Error(), "模型未加载") {
		t.Fatalf("err = %v, want 带内错误", recvErr)
	}
}

func TestStreamContextCancel(t *testing.T) {
	provider := newTestProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := provider.Stream(ctx, rig.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("首个分片: %v", err)
	}
	cancel()
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, recvErr := stream.Recv()
		if recvErr != nil {
			if !errors.Is(recvErr, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", recvErr)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("取消后 Recv 没有返回")
		}
	}
}

func TestStreamMissingIndexDefaultsToZero(t *testing.T) {
	stream := streamFrom(t, nil, true,
		`{"choices":[{"delta":{"tool_calls":[{"id":"c1","function":{"name":"echo","arguments":"{}"}}]}}]}`,
	)
	var call *rig.ToolCallDelta
	for _, chunk := range collect(t, stream) {
		for i := range chunk.ToolCalls {
			call = &chunk.ToolCalls[i]
		}
	}
	if call == nil {
		t.Fatal("没有工具调用")
	}
	if call.Index != 0 {
		t.Fatalf("Index = %d, want 0（省略 index 的服务端）", call.Index)
	}
}
