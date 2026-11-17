package local_test

// 本文件是**跨厂商一致性**测试：个人用户会天天换模型，因此 ollama、LM Studio 与
// 通用本地服务对同一个 rig.Request 必须给出同样的 rig.Chunk 语义。
//
// 放在 local_test（外部测试包）里，所以可以同时 import 三个实现而不产生循环依赖。
// 每个用例都跑三遍：换 provider 不该让 agent 坏掉。

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/provider/lmstudio"
	"github.com/zeokeel/zeokeel-rig/provider/local"
	"github.com/zeokeel/zeokeel-rig/provider/ollama"
)

// handle 是被测 provider 的统一视图。
type handle struct {
	provider rig.Provider
	models   func(ctx context.Context) ([]local.ModelInfo, error)
	endpoint local.Endpoint
}

// subject 描述一种 provider 的装配方式。
type subject struct {
	name string
	// newHandle 用测试服务器地址构造 provider。
	newHandle func(t *testing.T, serverURL string, client *http.Client) handle
	// chatPath / modelsPath 是该 provider 期望的路径后缀。
	chatPath   string
	modelsPath string
	// modelsBody 是该 provider 期望的模型列表响应体。
	modelsBody string
}

// subjects 是三个本地 provider（顺序无关，只为稳定输出）。
func subjects() []subject {
	return []subject{
		{
			name: "local",
			newHandle: func(t *testing.T, serverURL string, client *http.Client) handle {
				t.Helper()
				provider, err := local.New(local.Config{
					BaseURL:       serverURL + "/v1",
					DefaultModel:  "m",
					HTTPClient:    client,
					RetryBackoff:  time.Millisecond,
					TransportHint: "",
				})
				if err != nil {
					t.Fatalf("local.New: %v", err)
				}
				return handle{provider: provider, models: provider.Models, endpoint: provider.Endpoint()}
			},
			chatPath:   "/v1/chat/completions",
			modelsPath: "/v1/models",
			modelsBody: `{"object":"list","data":[{"id":"b-model"},{"id":"a-model"}]}`,
		},
		{
			name: "ollama",
			newHandle: func(t *testing.T, serverURL string, client *http.Client) handle {
				t.Helper()
				provider, err := ollama.New(ollama.Config{
					BaseURL:      serverURL,
					DefaultModel: "m",
					HTTPClient:   client,
					RetryBackoff: time.Millisecond,
				})
				if err != nil {
					t.Fatalf("ollama.New: %v", err)
				}
				return handle{provider: provider, models: provider.Models, endpoint: provider.Endpoint()}
			},
			chatPath:   "/v1/chat/completions",
			modelsPath: "/api/tags",
			modelsBody: `{"models":[{"name":"b-model:latest","size":2},{"name":"a-model:latest","size":1}]}`,
		},
		{
			name: "lmstudio",
			newHandle: func(t *testing.T, serverURL string, client *http.Client) handle {
				t.Helper()
				provider, err := lmstudio.New(lmstudio.Config{
					BaseURL:      serverURL,
					DefaultModel: "m",
					HTTPClient:   client,
					RetryBackoff: time.Millisecond,
				})
				if err != nil {
					t.Fatalf("lmstudio.New: %v", err)
				}
				return handle{provider: provider, models: provider.Models, endpoint: provider.Endpoint()}
			},
			chatPath:   "/v1/chat/completions",
			modelsPath: "/v1/models",
			modelsBody: `{"object":"list","data":[{"id":"b-model"},{"id":"a-model"}]}`,
		},
	}
}

// withServer 起一个 test server，把 SSE 事件按 subject 的期望路径回放。
func withServer(t *testing.T, subject subject, sse []string, done bool) handle {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, subject.chatPath):
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, _ := w.(http.Flusher)
			for _, event := range sse {
				_, _ = io.WriteString(w, "data: "+event+"\n\n")
				if flusher != nil {
					flusher.Flush()
				}
			}
			if done {
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
			}
		case strings.HasSuffix(r.URL.Path, subject.modelsPath):
			_, _ = io.WriteString(w, subject.modelsBody)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return subject.newHandle(t, server.URL, server.Client())
}

// drain 读尽流并返回分片。
func drain(t *testing.T, stream rig.Stream) []rig.Chunk {
	t.Helper()
	var chunks []rig.Chunk
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return chunks
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		chunks = append(chunks, chunk)
	}
}

// TestProviderConformance 对三个 provider 重复同一组断言。
func TestProviderConformance(t *testing.T) {
	// 1) 流式基本形状：正文拼接、结束原因、用量。
	t.Run("流式基本形状", func(t *testing.T) {
		for _, subject := range subjects() {
			subject := subject
			t.Run(subject.name, func(t *testing.T) {
				handle := withServer(t, subject, []string{
					`{"choices":[{"delta":{"content":"he"}}]}`,
					`{"choices":[{"delta":{"content":"llo"},"finish_reason":"stop"}]}`,
					`{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
				}, true)
				stream, err := handle.provider.Stream(context.Background(), rig.Request{Model: "m"})
				if err != nil {
					t.Fatalf("Stream: %v", err)
				}
				chunks := drain(t, stream)
				var text strings.Builder
				finish := ""
				var usage *rig.Usage
				for _, chunk := range chunks {
					text.WriteString(chunk.Delta)
					if chunk.FinishReason != "" {
						finish = chunk.FinishReason
					}
					if chunk.Usage != nil {
						usage = chunk.Usage
					}
				}
				if text.String() != "hello" || finish != "stop" {
					t.Fatalf("text=%q finish=%q", text.String(), finish)
				}
				if usage == nil || usage.TotalTokens != 5 {
					t.Fatalf("usage = %+v", usage)
				}
			})
		}
	})

	// 2) 缺失 [DONE] 必须容忍（本地服务常见）。
	t.Run("缺失 DONE 容忍", func(t *testing.T) {
		for _, subject := range subjects() {
			subject := subject
			t.Run(subject.name, func(t *testing.T) {
				handle := withServer(t, subject, []string{
					`{"choices":[{"delta":{"content":"ok"}}]}`,
				}, false)
				stream, err := handle.provider.Stream(context.Background(), rig.Request{Model: "m"})
				if err != nil {
					t.Fatalf("Stream: %v", err)
				}
				if chunks := drain(t, stream); len(chunks) != 1 || chunks[0].Delta != "ok" {
					t.Fatalf("chunks = %+v", chunks)
				}
			})
		}
	})

	// 3) 工具调用增量：ID/Name 先到、Arguments 累加、finish_reason 归一化。
	t.Run("工具调用增量", func(t *testing.T) {
		for _, subject := range subjects() {
			subject := subject
			t.Run(subject.name, func(t *testing.T) {
				handle := withServer(t, subject, []string{
					`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"read_file","arguments":""}}]}}]}`,
					`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"a\"}"}}]}}]}`,
					`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
				}, true)
				stream, err := handle.provider.Stream(context.Background(), rig.Request{Model: "m"})
				if err != nil {
					t.Fatalf("Stream: %v", err)
				}
				var calls []rig.ToolCallDelta
				finish := ""
				for _, chunk := range drain(t, stream) {
					calls = append(calls, chunk.ToolCalls...)
					if chunk.FinishReason != "" {
						finish = chunk.FinishReason
					}
				}
				if len(calls) == 0 || calls[0].ID != "c1" || calls[0].Name != "read_file" {
					t.Fatalf("calls = %+v", calls)
				}
				var arguments strings.Builder
				for _, call := range calls {
					arguments.WriteString(call.Arguments)
				}
				if arguments.String() != `{"path":"a"}` || finish != "tool_calls" {
					t.Fatalf("arguments=%q finish=%q", arguments.String(), finish)
				}
			})
		}
	})

	// 4) 模型列表：非空、按 ID 排序、ID 可直接用于请求。
	t.Run("模型列表", func(t *testing.T) {
		for _, subject := range subjects() {
			subject := subject
			t.Run(subject.name, func(t *testing.T) {
				handle := withServer(t, subject, nil, true)
				models, err := handle.models(context.Background())
				if err != nil {
					t.Fatalf("Models: %v", err)
				}
				if len(models) != 2 {
					t.Fatalf("models = %+v", models)
				}
				if models[0].ID > models[1].ID {
					t.Fatalf("模型列表未按 ID 排序: %+v", models)
				}
			})
		}
	})

	// 5) 端点描述：名字与默认地址必须自洽（供宿主的选择器展示）。
	t.Run("端点描述", func(t *testing.T) {
		for _, subject := range subjects() {
			subject := subject
			t.Run(subject.name, func(t *testing.T) {
				handle := withServer(t, subject, nil, true)
				if handle.endpoint.Name != handle.provider.Name() {
					t.Fatalf("endpoint.Name = %q, provider.Name = %q", handle.endpoint.Name, handle.provider.Name())
				}
				if handle.provider.Name() != subject.name {
					t.Fatalf("Name = %q, want %q", handle.provider.Name(), subject.name)
				}
			})
		}
	})
}

// TestProviderConformanceErrorClassification 固定"换 provider 不改错误语义"：
// 4xx 不重试且错误里带响应体；5xx 会重试。
func TestProviderConformanceErrorClassification(t *testing.T) {
	for _, subject := range subjects() {
		subject := subject
		t.Run(subject.name, func(t *testing.T) {
			t.Run("4xx 不重试", func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					http.Error(w, `{"error":"bad request body"}`, http.StatusBadRequest)
				}))
				t.Cleanup(server.Close)
				handle := subject.newHandle(t, server.URL, server.Client())
				if _, err := handle.provider.Stream(context.Background(), rig.Request{Model: "m"}); err == nil {
					t.Fatal("必须返回错误")
				} else if !strings.Contains(err.Error(), "bad request body") {
					t.Fatalf("错误里应带响应体: %v", err)
				}
				if got := calls.Load(); got != 1 {
					t.Fatalf("调用次数 = %d, want 1（4xx 不重试）", got)
				}
			})
			t.Run("5xx 重试后成功", func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if calls.Add(1) < 3 {
						http.Error(w, "overloaded", http.StatusServiceUnavailable)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`+"\n\n")
					_, _ = io.WriteString(w, "data: [DONE]\n\n")
				}))
				t.Cleanup(server.Close)
				handle := subject.newHandle(t, server.URL, server.Client())
				stream, err := handle.provider.Stream(context.Background(), rig.Request{Model: "m"})
				if err != nil {
					t.Fatalf("Stream: %v", err)
				}
				if chunks := drain(t, stream); len(chunks) == 0 || chunks[0].Delta != "ok" {
					t.Fatalf("chunks = %+v", chunks)
				}
				if got := calls.Load(); got != 3 {
					t.Fatalf("调用次数 = %d, want 3", got)
				}
			})
		})
	}
}
