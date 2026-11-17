package local_test

// 本文件覆盖**严格档案**（Quirks.StrictFinishReasons / StrictDeltas）：它是给
// 托管服务（provider/openai）准备的开关，但属于本包的公开 API，因此在本包也要有
// 用例钉住——否则"共享基座里的分支"只有调用方在测。
//
// 与宽松默认的差异（四条的完整跨 provider 对照见
// provider/openai/audit_cross_provider_test.go）：
//  1. finish_reason 原样透传（不转小写、不套别名）；
//  2. content / arguments 必须是字符串（对象型 arguments 会让整条事件被跳过）；
//  3. 不认 choice.message 回退；
//  4. 不认旧式 function_call。

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/provider/local"
)

// strictStream 起一个 local provider（严格档案）并开流。
func strictStream(t *testing.T, body string, quirks local.Quirks) rig.Stream {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	provider, err := local.New(local.Config{
		BaseURL:      server.URL + "/v1",
		DefaultModel: "m",
		HTTPClient:   server.Client(),
		RetryBackoff: time.Millisecond,
		Quirks:       quirks,
	})
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}
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

// strictQuirks 是"托管服务"档案。
func strictQuirks() local.Quirks {
	return local.Quirks{
		RequireDone:         true,
		StrictFinishReasons: true,
		StrictDeltas:        true,
	}
}

// drainAll 读尽流，返回分片与终止错误。
func drainAll(t *testing.T, stream rig.Stream) ([]rig.Chunk, error) {
	t.Helper()
	var chunks []rig.Chunk
	for {
		chunk, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return chunks, nil
			}
			return chunks, err
		}
		chunks = append(chunks, chunk)
	}
}

// TestStrictProfileKeepsFinishReasonRaw：严格档案不归一化 finish_reason。
func TestStrictProfileKeepsFinishReasonRaw(t *testing.T) {
	cases := map[string]string{
		"eos":        "eos",
		"MAX_TOKENS": "MAX_TOKENS",
		"tool_call":  "tool_call",
	}
	for raw, want := range cases {
		raw, want := raw, want
		t.Run(raw, func(t *testing.T) {
			body := `data: {"choices":[{"delta":{"content":"x"},"finish_reason":"` + raw + `"}]}` + "\n\ndata: [DONE]\n\n"
			chunks, err := drainAll(t, strictStream(t, body, strictQuirks()))
			if err != nil {
				t.Fatalf("Recv: %v", err)
			}
			finish := ""
			for _, chunk := range chunks {
				if chunk.FinishReason != "" {
					finish = chunk.FinishReason
				}
			}
			if finish != want {
				t.Fatalf("finish = %q, want %q（严格档案原样透传）", finish, want)
			}
		})
	}

	// 对照：宽松默认会归一化。
	body := `data: {"choices":[{"delta":{"content":"x"},"finish_reason":"eos"}]}` + "\n\ndata: [DONE]\n\n"
	chunks, err := drainAll(t, strictStream(t, body, local.Quirks{}))
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if finish := chunks[len(chunks)-1].FinishReason; finish != "stop" {
		t.Fatalf("宽松档案 finish = %q, want stop", finish)
	}
}

// TestStrictProfileIgnoresLenientShapes：message 回退、旧式 function_call、对象型
// arguments 在严格档案下都不被采纳。
func TestStrictProfileIgnoresLenientShapes(t *testing.T) {
	cases := map[string]struct {
		event string
		want  string // 期望拼接出的正文
	}{
		"message 回退": {
			event: `{"choices":[{"message":{"content":"whole"},"finish_reason":"stop"}]}`,
			want:  "",
		},
		"旧式 function_call": {
			event: `{"choices":[{"delta":{"function_call":{"name":"read_file","arguments":"{}"}}}]}`,
			want:  "",
		},
		"对象型 arguments": {
			event: `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"f","arguments":{"a":1}}}]}}]}`,
			want:  "",
		},
	}
	for name, testCase := range cases {
		testCase := testCase
		t.Run(name, func(t *testing.T) {
			body := "data: " + testCase.event + "\n\ndata: [DONE]\n\n"
			chunks, err := drainAll(t, strictStream(t, body, strictQuirks()))
			if err != nil {
				t.Fatalf("Recv: %v", err)
			}
			var text strings.Builder
			for _, chunk := range chunks {
				text.WriteString(chunk.Delta)
				if len(chunk.ToolCalls) > 0 {
					t.Fatalf("严格档案不该产出工具调用: %+v", chunk.ToolCalls)
				}
			}
			if text.String() != testCase.want {
				t.Fatalf("文本 = %q, want %q", text.String(), testCase.want)
			}
		})
	}
}

// TestStrictProfileRequiresTerminator：严格档案下缺终止标记要报错（宽松默认容忍）。
func TestStrictProfileRequiresTerminator(t *testing.T) {
	body := `data: {"choices":[{"delta":{"content":"partial"}}]}` + "\n\n"
	chunks, err := drainAll(t, strictStream(t, body, strictQuirks()))
	if !errors.Is(err, local.ErrIncompleteStream) {
		t.Fatalf("err = %v, want local.ErrIncompleteStream", err)
	}
	if len(chunks) != 1 || chunks[0].Delta != "partial" {
		t.Fatalf("chunks = %+v（报错前的分片不能丢）", chunks)
	}

	// 有 finish_reason 就不算截断（与 provider/openai 的历史语义一致）。
	body = `data: {"choices":[{"delta":{"content":"x"},"finish_reason":"stop"}]}` + "\n\n"
	if _, err := drainAll(t, strictStream(t, body, strictQuirks())); err != nil {
		t.Fatalf("带 finish_reason 时不该报错: %v", err)
	}
}
