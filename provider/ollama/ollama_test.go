package ollama

// 本文件是 provider/ollama 的契约：默认地址、OpenAI 兼容端点路径、无鉴权、模型列表
// 走原生 /api/tags，以及"连不上"时给用户的可执行提示。全部用 httptest，不依赖本机
// 真的装了 ollama。

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

// newTestProvider 起一个 test server（同时扮演 /v1 与 /api/tags），返回指向它的
// provider 与记录到的请求路径。
func newTestProvider(t *testing.T, mutate func(*Config), handler http.HandlerFunc) (*Provider, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	cfg := Config{
		BaseURL:      server.URL,
		DefaultModel: "llama3.2",
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

// writeSSE 回放 SSE 事件（done=false 模拟直接关连接）。
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

// collect 读尽一个流。
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
	if provider.Name() != Name {
		t.Fatalf("Name = %q, want %q", provider.Name(), Name)
	}
	if Name != "ollama" {
		t.Fatalf("Name 常量 = %q", Name)
	}
	if DefaultBaseURL != "http://127.0.0.1:11434" {
		t.Fatalf("DefaultBaseURL = %q", DefaultBaseURL)
	}
	endpoint := Endpoint()
	if endpoint.Name != Name || endpoint.DefaultBaseURL != DefaultBaseURL {
		t.Fatalf("endpoint = %+v", endpoint)
	}
	if endpoint.AuthRequired {
		t.Fatal("ollama 默认无鉴权：AuthRequired 必须是 false")
	}
	if endpoint.ModelsPath != ModelsPath {
		t.Fatalf("ModelsPath = %q, want %q", endpoint.ModelsPath, ModelsPath)
	}
}

// TestChatEndpointAndNoAuth 是本包最重要的两条事实：走 /v1/chat/completions，
// 且默认不发 Authorization。
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
	stream, err := provider.Stream(context.Background(), rig.Request{Model: "llama3.2"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	chunks := collect(t, stream)
	if got, _ := path.Load().(string); got != "/v1/chat/completions" {
		t.Fatalf("path = %q, want /v1/chat/completions", got)
	}
	if sawAuth.Load() {
		t.Fatal("默认不得发送 Authorization（本地 ollama 无鉴权）")
	}
	if len(chunks) == 0 || chunks[0].Delta != "hi" {
		t.Fatalf("chunks = %+v", chunks)
	}
}

func TestAPIKeySentWhenConfigured(t *testing.T) {
	var sawAuth atomic.Bool
	provider, _ := newTestProvider(t, func(cfg *Config) { cfg.APIKey = "proxy-token" }, func(w http.ResponseWriter, r *http.Request) {
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
		t.Fatal("显式配置 APIKey 时（例如套了反代）必须发送 Authorization")
	}
}

func TestBaseURLNormalization(t *testing.T) {
	// 用测试服务器的地址拼出各种写法，断言兼容层与原生 API 的路径都被正确归一化。
	cases := []struct {
		name     string
		suffix   string
		wantChat string
		wantTags string
	}{
		{"裸地址", "", "/v1/chat/completions", "/api/tags"},
		{"结尾斜杠", "/", "/v1/chat/completions", "/api/tags"},
		{"已带 v1", "/v1", "/v1/chat/completions", "/api/tags"},
		{"带路径前缀", "/ollama", "/ollama/v1/chat/completions", "/ollama/api/tags"},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			var chatPath, tagsPath atomic.Value
			var base atomic.Value
			provider, _ := newTestProvider(t, func(cfg *Config) {
				// newTestProvider 已经把 BaseURL 指向测试服务器，这里追加后缀。
				cfg.BaseURL = cfg.BaseURL + testCase.suffix
			}, func(w http.ResponseWriter, r *http.Request) {
				base.Store(r.URL.Path)
				switch {
				case strings.HasSuffix(r.URL.Path, "/chat/completions"):
					chatPath.Store(r.URL.Path)
					writeSSE(w, true, `{"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}`)
				case strings.HasSuffix(r.URL.Path, "/api/tags"):
					tagsPath.Store(r.URL.Path)
					_, _ = io.WriteString(w, `{"models":[]}`)
				default:
					t.Errorf("意外路径 %q", r.URL.Path)
				}
			})
			stream, err := provider.Stream(context.Background(), rig.Request{Model: "m"})
			if err != nil {
				t.Fatalf("Stream（BaseURL 后缀 %q）: %v", testCase.suffix, err)
			}
			collect(t, stream)
			if _, err := provider.Models(context.Background()); err != nil {
				t.Fatalf("Models（BaseURL 后缀 %q）: %v", testCase.suffix, err)
			}
			if got, _ := chatPath.Load().(string); got != testCase.wantChat {
				t.Fatalf("chat path = %q, want %q", got, testCase.wantChat)
			}
			if got, _ := tagsPath.Load().(string); got != testCase.wantTags {
				t.Fatalf("tags path = %q, want %q", got, testCase.wantTags)
			}
		})
	}
}

// TestModelsFromAPITags 覆盖 ollama 原生 /api/tags 的形状。
func TestModelsFromAPITags(t *testing.T) {
	provider, _ := newTestProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			t.Errorf("path = %q, want /api/tags", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"models":[
			{"name":"qwen2.5-coder:7b","model":"qwen2.5-coder:7b","modified_at":"2025-02-03T04:05:06.789Z","size":4661224676,"digest":"sha256:abc","details":{"family":"qwen2","parameter_size":"7.6B","quantization_level":"Q4_K_M"}},
			{"name":"llama3.2:latest","model":"llama3.2:latest","modified_at":"2025-01-02T03:04:05Z","size":2019393189,"digest":"sha256:def","details":{"family":"llama","parameter_size":"3.2B"}}
		]}`)
	})
	models, err := provider.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("模型数 = %d", len(models))
	}
	// 按 ID 排序：llama3.2 在前。
	if models[0].ID != "llama3.2:latest" || models[1].ID != "qwen2.5-coder:7b" {
		t.Fatalf("排序/ID 不对: %+v", models)
	}
	if models[0].Size != 2019393189 {
		t.Fatalf("Size = %d", models[0].Size)
	}
	if models[0].Modified.UTC().Format(time.RFC3339) != "2025-01-02T03:04:05Z" {
		t.Fatalf("Modified = %v", models[0].Modified)
	}
	// ModelLister 接口（kit 的模型预设用它）。
	var lister local.ModelLister = provider
	if _, err := lister.Models(context.Background()); err != nil {
		t.Fatalf("ModelLister: %v", err)
	}
}

func TestModelsStatusError(t *testing.T) {
	provider, _ := newTestProvider(t, func(cfg *Config) { cfg.MaxRetries = -1 }, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"broken"}`, http.StatusInternalServerError)
	})
	_, err := provider.Models(context.Background())
	if err == nil || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("err = %v", err)
	}
}

// TestUnreachableHint 覆盖"ollama 没启动"这条最常遇到的错误。
func TestUnreachableHint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := server.URL
	server.Close()
	provider, err := New(Config{BaseURL: url, DefaultModel: "m", MaxRetries: -1, RetryBackoff: time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = provider.Stream(context.Background(), rig.Request{Model: "m"})
	if err == nil {
		t.Fatal("连不上必须报错")
	}
	if !strings.Contains(err.Error(), "ollama serve") {
		t.Fatalf("错误里应有 'ollama serve' 提示: %v", err)
	}
	if !strings.HasPrefix(err.Error(), "ollama:") {
		t.Fatalf("错误前缀应指向真正出问题的服务: %v", err)
	}
	// 模型列表同样要给提示。
	if _, modelsErr := provider.Models(context.Background()); modelsErr == nil || !strings.Contains(modelsErr.Error(), "ollama serve") {
		t.Fatalf("Models err = %v", modelsErr)
	}
}

// TestStreamToleratesMissingDone 覆盖 ollama 直接关连接、不发 [DONE] 的常见行为。
func TestStreamToleratesMissingDone(t *testing.T) {
	provider, _ := newTestProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, false,
			`{"choices":[{"delta":{"content":"he"}}]}`,
			`{"choices":[{"delta":{"content":"llo"},"finish_reason":"stop"}]}`,
		)
	})
	stream, err := provider.Stream(context.Background(), rig.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var text strings.Builder
	for _, chunk := range collect(t, stream) {
		text.WriteString(chunk.Delta)
	}
	if text.String() != "hello" {
		t.Fatalf("text = %q", text.String())
	}
}

// TestRequireDoneQuirkIsForwarded 确认 Quirks 能透传给共享基座。
func TestRequireDoneQuirkIsForwarded(t *testing.T) {
	provider, _ := newTestProvider(t, func(cfg *Config) {
		cfg.Quirks = local.Quirks{RequireDone: true}
	}, func(w http.ResponseWriter, r *http.Request) {
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
	if !errors.Is(got, local.ErrIncompleteStream) {
		t.Fatalf("err = %v, want local.ErrIncompleteStream", got)
	}
}

func TestDisableToolsQuirk(t *testing.T) {
	var body map[string]any
	provider, _ := newTestProvider(t, func(cfg *Config) {
		cfg.Quirks = local.Quirks{DisableTools: true}
	}, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		writeSSE(w, true, `{"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}`)
	})
	stream, err := provider.Stream(context.Background(), rig.Request{
		Model:    "m",
		Messages: []rig.Message{rig.NewMessage(rig.RoleUser, "hi")},
		Tools:    []rig.ToolSpec{{Name: "echo"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	collect(t, stream)
	if _, exists := body["tools"]; exists {
		t.Fatalf("DisableTools 未生效: %v", body["tools"])
	}
}

func TestNewValidatesBaseURL(t *testing.T) {
	if _, err := New(Config{BaseURL: "ftp://127.0.0.1"}); err == nil {
		t.Fatal("不支持的 scheme 必须构造失败")
	}
}

var _ = errors.Is
