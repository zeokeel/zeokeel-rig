package lmstudio

// 本文件是 provider/lmstudio 的契约：默认 1234 端口、/v1/* 路径、无鉴权、
// 模型列表走 /v1/models，以及"连不上"时的可执行提示。全部用 httptest。

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/provider/local"
)

func newTestProvider(t *testing.T, mutate func(*Config), handler http.HandlerFunc) (*Provider, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	cfg := Config{
		BaseURL:      server.URL,
		DefaultModel: "qwen2.5-7b-instruct",
		HTTPClient:   server.Client(),
		RetryBackoff: time.Millisecond,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	provider, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return provider, server
}

func writeSSE(w http.ResponseWriter, done bool, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	for _, event := range events {
		_, _ = io.WriteString(w, "data: "+event+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}
	if done {
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func collect(t *testing.T, stream rig.Stream) []rig.Chunk {
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

func TestNewDefaults(t *testing.T) {
	provider, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if provider.Name() != Name || Name != "lmstudio" {
		t.Fatalf("Name = %q", provider.Name())
	}
	if DefaultBaseURL != "http://127.0.0.1:1234" {
		t.Fatalf("DefaultBaseURL = %q", DefaultBaseURL)
	}
	endpoint := Endpoint()
	if endpoint.Name != Name || endpoint.DefaultBaseURL != DefaultBaseURL {
		t.Fatalf("endpoint = %+v", endpoint)
	}
	if endpoint.AuthRequired {
		t.Fatal("LM Studio 默认无鉴权")
	}
}

func TestChatEndpointAndNoAuth(t *testing.T) {
	var path atomic.Value
	var sawAuth atomic.Bool
	provider, _ := newTestProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
		path.Store(r.URL.Path)
		if r.Header.Get("Authorization") != "" {
			sawAuth.Store(true)
		}
		writeSSE(w, true, `{"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}`)
	})
	stream, err := provider.Stream(context.Background(), rig.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	chunks := collect(t, stream)
	if got, _ := path.Load().(string); got != "/v1/chat/completions" {
		t.Fatalf("path = %q", got)
	}
	if sawAuth.Load() {
		t.Fatal("默认不得发送 Authorization")
	}
	if len(chunks) == 0 || chunks[0].Delta != "hi" {
		t.Fatalf("chunks = %+v", chunks)
	}
}

func TestAPIKeySentWhenConfigured(t *testing.T) {
	var sawAuth atomic.Bool
	provider, _ := newTestProvider(t, func(cfg *Config) { cfg.APIKey = "proxy" }, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			sawAuth.Store(true)
		}
		writeSSE(w, true, `{"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}`)
	})
	stream, err := provider.Stream(context.Background(), rig.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	collect(t, stream)
	if !sawAuth.Load() {
		t.Fatal("显式配置 APIKey 时必须发送 Authorization")
	}
}

func TestBaseURLNormalization(t *testing.T) {
	cases := []struct {
		name   string
		suffix string
		want   string
	}{
		{"裸地址", "", "/v1/chat/completions"},
		{"结尾斜杠", "/", "/v1/chat/completions"},
		{"已带 v1", "/v1", "/v1/chat/completions"},
		{"带路径前缀", "/lmstudio", "/lmstudio/v1/chat/completions"},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			var path atomic.Value
			provider, _ := newTestProvider(t, func(cfg *Config) {
				cfg.BaseURL = cfg.BaseURL + testCase.suffix
			}, func(w http.ResponseWriter, r *http.Request) {
				path.Store(r.URL.Path)
				writeSSE(w, true, `{"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}`)
			})
			stream, err := provider.Stream(context.Background(), rig.Request{Model: "m"})
			if err != nil {
				t.Fatalf("Stream（后缀 %q）: %v", testCase.suffix, err)
			}
			collect(t, stream)
			if got, _ := path.Load().(string); got != testCase.want {
				t.Fatalf("path = %q, want %q", got, testCase.want)
			}
		})
	}
}

// TestStreamToolCalls 覆盖 LM Studio 的工具调用增量（标准 OpenAI 形状）。
func TestStreamToolCalls(t *testing.T) {
	provider, _ := newTestProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, true,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"read_file","arguments":""}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"a.txt\"}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		)
	})
	stream, err := provider.Stream(context.Background(), rig.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var calls []rig.ToolCallDelta
	for _, chunk := range collect(t, stream) {
		calls = append(calls, chunk.ToolCalls...)
	}
	if len(calls) == 0 || calls[0].Name != "read_file" || calls[0].ID != "c1" {
		t.Fatalf("calls = %+v", calls)
	}
	var arguments strings.Builder
	for _, call := range calls {
		arguments.WriteString(call.Arguments)
	}
	if arguments.String() != `{"path":"a.txt"}` {
		t.Fatalf("arguments = %q", arguments.String())
	}
}

// TestModelsFromV1Models 覆盖 LM Studio 的 /v1/models（含 max_context_length）。
func TestModelsFromV1Models(t *testing.T) {
	var sawPath atomic.Value
	provider, _ := newTestProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
		sawPath.Store(r.URL.Path)
		_, _ = io.WriteString(w, `{"object":"list","data":[
			{"id":"qwen2.5-7b-instruct","object":"model","owned_by":"organization_owner","max_context_length":32768},
			{"id":"llama-3.2-3b-instruct","object":"model","owned_owner":"x","context_length":8192}
		]}`)
	})
	models, err := provider.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if got, _ := sawPath.Load().(string); got != "/v1/models" {
		t.Fatalf("path = %q, want /v1/models", got)
	}
	if len(models) != 2 {
		t.Fatalf("models = %+v", models)
	}
	byID := map[string]local.ModelInfo{}
	for _, model := range models {
		byID[model.ID] = model
	}
	if byID["qwen2.5-7b-instruct"].ContextLength != 32768 {
		t.Fatalf("ContextLength = %d", byID["qwen2.5-7b-instruct"].ContextLength)
	}
	if byID["llama-3.2-3b-instruct"].ContextLength != 8192 {
		t.Fatalf("ContextLength = %d", byID["llama-3.2-3b-instruct"].ContextLength)
	}
	var lister local.ModelLister = provider
	if _, err := lister.Models(context.Background()); err != nil {
		t.Fatalf("ModelLister: %v", err)
	}
}

func TestModelsStatusError(t *testing.T) {
	provider, _ := newTestProvider(t, func(cfg *Config) { cfg.MaxRetries = -1 }, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"no models loaded"}`, http.StatusServiceUnavailable)
	})
	_, err := provider.Models(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no models loaded") {
		t.Fatalf("err = %v", err)
	}
}

// TestUnreachableHint 覆盖"LM Studio 本地服务器没开"的提示。
func TestUnreachableHint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := server.URL
	server.Close()
	provider, err := New(Config{BaseURL: url, DefaultModel: "m", MaxRetries: -1, Quirks: local.Quirks{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = provider.Stream(context.Background(), rig.Request{Model: "m"})
	if err == nil {
		t.Fatal("连不上必须报错")
	}
	if !strings.Contains(err.Error(), "Local Server") && !strings.Contains(err.Error(), "LM Studio") {
		t.Fatalf("错误里应有 LM Studio 的提示: %v", err)
	}
	if !strings.HasPrefix(err.Error(), "lmstudio:") {
		t.Fatalf("错误前缀应指向真正出问题的服务: %v", err)
	}
}

func TestNewValidatesBaseURL(t *testing.T) {
	if _, err := New(Config{BaseURL: "gopher://127.0.0.1"}); err == nil {
		t.Fatal("不支持的 scheme 必须构造失败")
	}
}

func TestExtraFieldsMerged(t *testing.T) {
	var body map[string]any
	provider, _ := newTestProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		writeSSE(w, true, `{"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}`)
	})
	stream, err := provider.Stream(context.Background(), rig.Request{
		Model:    "m",
		Messages: []rig.Message{rig.NewMessage(rig.RoleUser, "hi")},
		Extra:    map[string]any{"seed": float64(42)},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	collect(t, stream)
	if body["seed"] != float64(42) {
		t.Fatalf("seed = %v", body["seed"])
	}
}
