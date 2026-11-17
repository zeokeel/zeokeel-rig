package local

// 本文件是 provider/local 的行为契约：请求形状、鉴权开关、重试/退避、错误分类。
// 全部用 httptest 造真实的 HTTP 响应，不依赖本机装有任何本地模型服务。

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

// newTestProvider 起一个 test server 并返回指向它的 provider（重试间隔 1ms）。
func newTestProvider(t *testing.T, mutate func(*Config), handler http.HandlerFunc) *Provider {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	cfg := Config{
		BaseURL:      server.URL + "/v1",
		DefaultModel: "local-model",
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
	return provider
}

// writeSSE 按 SSE 写事件；done=false 时不发 [DONE]（模拟直接关连接的本地服务）。
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

// joinText 拼接 delta 正文。
func joinText(chunks []rig.Chunk) string {
	var builder strings.Builder
	for _, chunk := range chunks {
		builder.WriteString(chunk.Delta)
	}
	return builder.String()
}

// finishOf 返回最后一个非空的结束原因。
func finishOf(chunks []rig.Chunk) string {
	for i := len(chunks) - 1; i >= 0; i-- {
		if chunks[i].FinishReason != "" {
			return chunks[i].FinishReason
		}
	}
	return ""
}

func TestNewDefaults(t *testing.T) {
	provider, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if provider.Name() != DefaultName {
		t.Fatalf("Name = %q, want %q", provider.Name(), DefaultName)
	}
	if provider.cfg.baseURL != DefaultBaseURL {
		t.Fatalf("baseURL = %q, want %q", provider.cfg.baseURL, DefaultBaseURL)
	}
	if provider.cfg.chatPath != "/chat/completions" {
		t.Fatalf("chatPath = %q", provider.cfg.chatPath)
	}
	if provider.cfg.modelsPath != "/models" {
		t.Fatalf("modelsPath = %q", provider.cfg.modelsPath)
	}
	if provider.cfg.maxRetries != DefaultMaxRetries {
		t.Fatalf("maxRetries = %d", provider.cfg.maxRetries)
	}
	if provider.cfg.backoff != DefaultRetryBackoff {
		t.Fatalf("backoff = %s", provider.cfg.backoff)
	}
}

func TestNewValidatesBaseURL(t *testing.T) {
	for name, base := range map[string]string{
		"空 scheme":    "127.0.0.1:11434/v1",
		"不支持的 scheme": "ftp://127.0.0.1/v1",
		"缺 host":      "http:///v1",
	} {
		base := base
		t.Run(name, func(t *testing.T) {
			if _, err := New(Config{BaseURL: base}); err == nil {
				t.Fatalf("BaseURL=%q 必须构造失败", base)
			}
		})
	}
	// 结尾斜杠会被去掉。
	provider, err := New(Config{BaseURL: "http://127.0.0.1:11434/v1/"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if strings.HasSuffix(provider.cfg.baseURL, "/") {
		t.Fatalf("baseURL 未去掉结尾斜杠: %q", provider.cfg.baseURL)
	}
}

// TestAuthHeaderIsOmittedWithoutKey 是"本地模型通常没有 key"的核心用例。
func TestAuthHeaderIsOmittedWithoutKey(t *testing.T) {
	t.Setenv("LOCAL_TEST_KEY", "")
	var sawAuth atomic.Bool
	var sawPath atomic.Value
	provider := newTestProvider(t, func(cfg *Config) { cfg.APIKeyEnv = "LOCAL_TEST_KEY" }, func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Header["Authorization"]; ok {
			sawAuth.Store(true)
		}
		sawPath.Store(r.URL.Path)
		writeSSE(w, true, `{"choices":[{"delta":{"content":"hi"}}]}`)
	})
	stream, err := provider.Stream(context.Background(), rig.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	collect(t, stream)
	if sawAuth.Load() {
		t.Fatal("无 key 时不得发送 Authorization 头")
	}
	if path, _ := sawPath.Load().(string); path != "/v1/chat/completions" {
		t.Fatalf("path = %q, want /v1/chat/completions", path)
	}
}

func TestAuthHeaderModes(t *testing.T) {
	cases := map[string]struct {
		mutate   func(*Config)
		wantAuth bool
	}{
		"显式 key":         {func(cfg *Config) { cfg.APIKey = "secret" }, true},
		"环境变量 key":       {func(cfg *Config) { cfg.APIKeyEnv = "LOCAL_TEST_KEY" }, true},
		"DisableAuth 覆盖": {func(cfg *Config) { cfg.APIKey = "secret"; cfg.DisableAuth = true }, false},
	}
	for name, testCase := range cases {
		testCase := testCase
		t.Run(name, func(t *testing.T) {
			t.Setenv("LOCAL_TEST_KEY", "from-env")
			var sawAuth atomic.Bool
			var header atomic.Value
			provider := newTestProvider(t, testCase.mutate, func(w http.ResponseWriter, r *http.Request) {
				if value := r.Header.Get("Authorization"); value != "" {
					sawAuth.Store(true)
					header.Store(value)
				}
				writeSSE(w, true, `{"choices":[{"delta":{"content":"hi"}}]}`)
			})
			stream, err := provider.Stream(context.Background(), rig.Request{Model: "m"})
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			collect(t, stream)
			if sawAuth.Load() != testCase.wantAuth {
				t.Fatalf("Authorization 发送=%v, want %v", sawAuth.Load(), testCase.wantAuth)
			}
			if testCase.wantAuth {
				if got, _ := header.Load().(string); !strings.HasPrefix(got, "Bearer ") {
					t.Fatalf("Authorization = %q", got)
				}
			}
		})
	}
}

func TestCustomPaths(t *testing.T) {
	var chatPath atomic.Value
	provider := newTestProvider(t, func(cfg *Config) {
		cfg.ChatPath = "/api/chat"
		cfg.ModelsPath = "/api/models"
	}, func(w http.ResponseWriter, r *http.Request) {
		chatPath.Store(r.URL.Path)
		writeSSE(w, true, `{"choices":[{"delta":{"content":"hi"}}]}`)
	})
	stream, err := provider.Stream(context.Background(), rig.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	collect(t, stream)
	if path, _ := chatPath.Load().(string); path != "/v1/api/chat" {
		t.Fatalf("path = %q, want /v1/api/chat", path)
	}
}

func TestRequestBodyShape(t *testing.T) {
	var body map[string]any
	provider := newTestProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("请求体不是 JSON: %v", err)
		}
		writeSSE(w, true, `{"choices":[{"delta":{"content":"hi"}}]}`)
	})
	temperature := 0.2
	stream, err := provider.Stream(context.Background(), rig.Request{
		Model:       "m1",
		Messages:    []rig.Message{rig.NewMessage(rig.RoleUser, "hello")},
		Temperature: &temperature,
		MaxTokens:   64,
		Tools: []rig.ToolSpec{{
			Name: "echo", Description: "echo", Parameters: map[string]any{"type": "object"},
		}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	collect(t, stream)
	if body["model"] != "m1" {
		t.Fatalf("model = %v", body["model"])
	}
	if body["stream"] != true {
		t.Fatalf("stream = %v", body["stream"])
	}
	if body["max_tokens"] != float64(64) {
		t.Fatalf("max_tokens = %v", body["max_tokens"])
	}
	if body["temperature"] != 0.2 {
		t.Fatalf("temperature = %v", body["temperature"])
	}
	options, ok := body["stream_options"].(map[string]any)
	if !ok || options["include_usage"] != true {
		t.Fatalf("stream_options = %v（默认应请求用量）", body["stream_options"])
	}
	tools, ok := body["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %v", body["tools"])
	}
}

// TestQuirksDisableToolsAndStreamOptions 覆盖本地模型的常见差异：不支持 tools、
// 不认识 stream_options。
func TestQuirksDisableToolsAndStreamOptions(t *testing.T) {
	var body map[string]any
	provider := newTestProvider(t, func(cfg *Config) {
		cfg.Quirks.DisableTools = true
		cfg.Quirks.DisableStreamOptions = true
	}, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		writeSSE(w, true, `{"choices":[{"delta":{"content":"hi"}}]}`)
	})
	stream, err := provider.Stream(context.Background(), rig.Request{
		Model:    "m",
		Tools:    []rig.ToolSpec{{Name: "echo"}},
		Messages: []rig.Message{rig.NewMessage(rig.RoleUser, "hi")},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	collect(t, stream)
	if _, exists := body["tools"]; exists {
		t.Fatalf("DisableTools 为真时不该发送 tools: %v", body["tools"])
	}
	if _, exists := body["stream_options"]; exists {
		t.Fatalf("DisableStreamOptions 为真时不该发送 stream_options: %v", body["stream_options"])
	}
}

func TestStreamRequiresModel(t *testing.T) {
	provider, err := New(Config{BaseURL: "http://127.0.0.1:1/v1"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := provider.Stream(context.Background(), rig.Request{}); !errors.Is(err, rig.ErrModelRequired) {
		t.Fatalf("err = %v, want rig.ErrModelRequired", err)
	}
}

func TestRetriesOnServerError(t *testing.T) {
	var calls atomic.Int32
	provider := newTestProvider(t, func(cfg *Config) { cfg.MaxRetries = 2 }, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			http.Error(w, `{"error":"暂时过载"}`, http.StatusServiceUnavailable)
			return
		}
		writeSSE(w, true, `{"choices":[{"delta":{"content":"ok"}}]}`)
	})
	stream, err := provider.Stream(context.Background(), rig.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := joinText(collect(t, stream)); got != "ok" {
		t.Fatalf("text = %q", got)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("调用次数 = %d, want 3（两次失败 + 一次成功）", got)
	}
}

func TestRetryExhaustedReturnsStatusError(t *testing.T) {
	var calls atomic.Int32
	provider := newTestProvider(t, func(cfg *Config) { cfg.MaxRetries = 1 }, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, `{"error":"model not found"}`, http.StatusNotFound)
	})
	_, err := provider.Stream(context.Background(), rig.Request{Model: "m"})
	if err == nil {
		t.Fatal("404 必须返回错误")
	}
	// 404 不可重试：只调用一次。
	if got := calls.Load(); got != 1 {
		t.Fatalf("调用次数 = %d, want 1（4xx 不重试）", got)
	}
	if !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "model not found") {
		t.Fatalf("错误文本应包含状态码与响应体: %v", err)
	}
}

func TestRetryHonorsRetryAfter(t *testing.T) {
	var calls atomic.Int32
	provider := newTestProvider(t, func(cfg *Config) {
		cfg.MaxRetries = 1
		cfg.RetryBackoff = time.Second // Retry-After 应优先，测试才不会等 1s
	}, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		writeSSE(w, true, `{"choices":[{"delta":{"content":"ok"}}]}`)
	})
	start := time.Now()
	stream, err := provider.Stream(context.Background(), rig.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	collect(t, stream)
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("Retry-After 没有生效，耗时 %s", elapsed)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("调用次数 = %d, want 2", got)
	}
}

func TestRetryDisabledByNegative(t *testing.T) {
	var calls atomic.Int32
	provider := newTestProvider(t, func(cfg *Config) { cfg.MaxRetries = -1 }, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	if _, err := provider.Stream(context.Background(), rig.Request{Model: "m"}); err == nil {
		t.Fatal("必须返回错误")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("调用次数 = %d, want 1（负 MaxRetries 禁用重试）", got)
	}
}

func TestBackoffRespectsContextCancel(t *testing.T) {
	provider := newTestProvider(t, func(cfg *Config) {
		cfg.MaxRetries = 3
		cfg.RetryBackoff = time.Minute // 必须被 ctx 取消打断
	}, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := provider.Stream(ctx, rig.Request{Model: "m"})
	if err == nil {
		t.Fatal("必须返回错误")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("退避没有被 ctx 打断，耗时 %s", elapsed)
	}
}

func TestNonStreamingResponseRejected(t *testing.T) {
	provider := newTestProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hi"}}]}`)
	})
	_, err := provider.Stream(context.Background(), rig.Request{Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "text/event-stream") {
		t.Fatalf("err = %v, want 提到 text/event-stream", err)
	}
}

func TestTransportHintOnUnreachableServer(t *testing.T) {
	// 一个已经关闭的端口：连接被拒绝。
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := server.URL
	server.Close()
	provider, err := New(Config{
		BaseURL:       url + "/v1",
		DefaultModel:  "m",
		RetryBackoff:  time.Millisecond,
		MaxRetries:    -1,
		TransportHint: "本地服务没起来？试试 `ollama serve`",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = provider.Stream(context.Background(), rig.Request{Model: "m"})
	if err == nil {
		t.Fatal("连不上必须报错")
	}
	if !strings.Contains(err.Error(), "ollama serve") {
		t.Fatalf("错误里应带宿主的提示: %v", err)
	}
}

func TestNameOverride(t *testing.T) {
	provider, err := New(Config{Name: "my-llm"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if provider.Name() != "my-llm" {
		t.Fatalf("Name = %q", provider.Name())
	}
}

func TestExtraFieldsMergedAndReserved(t *testing.T) {
	var body map[string]any
	provider := newTestProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		writeSSE(w, true, `{"choices":[{"delta":{"content":"hi"}}]}`)
	})
	stream, err := provider.Stream(context.Background(), rig.Request{
		Model:    "m",
		Messages: []rig.Message{rig.NewMessage(rig.RoleUser, "hi")},
		Extra: map[string]any{
			"seed":   float64(7),
			"stream": false, // 保留字段：不能被 Extra 覆盖
		},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	collect(t, stream)
	if body["seed"] != float64(7) {
		t.Fatalf("Extra.seed 没有合并: %v", body["seed"])
	}
	if body["stream"] != true {
		t.Fatalf("Extra 覆盖了保留字段 stream: %v", body["stream"])
	}
}

// TestHeaderTimeout 覆盖"本地模型加载很慢"的可配置等待上限。
func TestHeaderTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond) // 故意比 HeaderTimeout 长
	}))
	t.Cleanup(server.Close)
	// 不传 HTTPClient：本包会用自己的 transport 并应用 HeaderTimeout。
	provider, err := New(Config{
		BaseURL:       server.URL + "/v1",
		DefaultModel:  "m",
		HeaderTimeout: 20 * time.Millisecond,
		MaxRetries:    -1,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	start := time.Now()
	_, err = provider.Stream(context.Background(), rig.Request{Model: "m"})
	if err == nil {
		t.Fatal("等待响应头超时必须报错")
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("HeaderTimeout 没生效，耗时 %s", elapsed)
	}
}

func TestConcurrentStreamsAreIndependent(t *testing.T) {
	provider := newTestProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		writeSSE(w, true, `{"choices":[{"delta":{"content":"`+body.Model+`"}}]}`)
	})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		model := "m" + string(rune('a'+i))
		wg.Add(1)
		go func() {
			defer wg.Done()
			stream, err := provider.Stream(context.Background(), rig.Request{Model: model})
			if err != nil {
				t.Errorf("Stream(%s): %v", model, err)
				return
			}
			if got := joinText(collect(t, stream)); got != model {
				t.Errorf("text = %q, want %q", got, model)
			}
		}()
	}
	wg.Wait()
}
