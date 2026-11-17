package ollama

// 本文件补齐 ollama 包自身函数的分支覆盖（New/normalizeRoot/orDefault/Endpoint/
// modelHeaders/Models），以及用户可见的行为：非 SSE 响应、读流中断、ctx 取消、
// 自定义 finish_reason 别名、DisableStreamOptions、模型列表的各种形状与错误、
// 以及"连不上"提示的逐字文案。

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

func TestInstanceEndpointMatchesPackageEndpoint(t *testing.T) {
	provider, err := New(Config{BaseURL: "http://127.0.0.1:5555"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	endpoint := provider.Endpoint()
	if endpoint.Name != Name || endpoint.ModelsPath != ModelsPath {
		t.Fatalf("endpoint = %+v", endpoint)
	}
	if endpoint.DefaultBaseURL != "http://127.0.0.1:5555" {
		t.Fatalf("DefaultBaseURL = %q（应回显实际服务根）", endpoint.DefaultBaseURL)
	}
	if endpoint.AuthRequired {
		t.Fatal("无 key 时 AuthRequired 应为 false")
	}
	pkg := Endpoint()
	if pkg.Name != endpoint.Name || pkg.ModelsPath != endpoint.ModelsPath {
		t.Fatalf("包级与实例描述不一致: %+v vs %+v", pkg, endpoint)
	}
	// 配了 key（反代场景）时如实报告需要鉴权。
	withKey, err := New(Config{APIKey: "secret"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !withKey.Endpoint().AuthRequired {
		t.Fatal("配了 APIKey 时 AuthRequired 应为 true")
	}
	// 带路径前缀的地址也要回显正确。
	prefixed, err := New(Config{BaseURL: "http://gateway/ollama/v1"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := prefixed.Endpoint().DefaultBaseURL; got != "http://gateway/ollama" {
		t.Fatalf("DefaultBaseURL = %q", got)
	}
}

func TestNewErrorPrefixes(t *testing.T) {
	for _, base := range []string{"ftp://127.0.0.1", "http://", "http://%zz"} {
		base := base
		t.Run(base, func(t *testing.T) {
			_, err := New(Config{BaseURL: base})
			if err == nil {
				t.Fatalf("BaseURL=%q 必须构造失败", base)
			}
			if !strings.HasPrefix(err.Error(), "ollama:") {
				t.Fatalf("错误必须以 ollama: 开头: %v", err)
			}
		})
	}
}

func TestHeaderTimeoutDefaults(t *testing.T) {
	if DefaultHeaderTimeout != 5*time.Minute {
		t.Fatalf("DefaultHeaderTimeout = %s, want 5m（本地模型冷启动很慢）", DefaultHeaderTimeout)
	}
	if got := orDefault(0, DefaultHeaderTimeout); got != DefaultHeaderTimeout {
		t.Fatalf("orDefault(0) = %s", got)
	}
	if got := orDefault(-time.Second, DefaultHeaderTimeout); got != DefaultHeaderTimeout {
		t.Fatalf("orDefault(负) = %s", got)
	}
	if got := orDefault(3*time.Second, DefaultHeaderTimeout); got != 3*time.Second {
		t.Fatalf("orDefault(正值) = %s", got)
	}
	if _, err := New(Config{HeaderTimeout: time.Second}); err != nil {
		t.Fatalf("New: %v", err)
	}
}

func TestNormalizeRootErrors(t *testing.T) {
	for _, base := range []string{"ftp://127.0.0.1", "http://", "http://%zz"} {
		if _, err := normalizeRoot(base); err == nil {
			t.Fatalf("normalizeRoot(%q) 必须报错", base)
		}
	}
	if got, err := normalizeRoot("http://127.0.0.1:11434/v1/"); err != nil || got != "http://127.0.0.1:11434" {
		t.Fatalf("normalizeRoot = (%q,%v)", got, err)
	}
}

// TestUnreachableHintVerbatim 逐字断言"连不上"的提示文案。
func TestUnreachableHintVerbatim(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := server.URL
	server.Close()
	provider, err := New(Config{BaseURL: url, DefaultModel: "m", MaxRetries: -1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = provider.Stream(context.Background(), rig.Request{Model: "m"})
	if err == nil {
		t.Fatal("必须报错")
	}
	const want = "ollama 未启动？先运行 `ollama serve`（或设置 OLLAMA_HOST 指向已有服务）"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("提示文案不匹配:\n got: %v\nwant: %s", err, want)
	}
	if !strings.HasPrefix(err.Error(), "ollama:") {
		t.Fatalf("错误前缀 = %v", err)
	}
}

// TestModelsSendsAuthToNativeAPI 覆盖 modelHeaders 的鉴权分支（反代场景）。
func TestModelsSendsAuthToNativeAPI(t *testing.T) {
	var sawAuth atomic.Bool
	provider, _ := newTestProvider(t, func(cfg *Config) { cfg.APIKey = "proxy-token" }, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" && r.Header.Get("Authorization") == "Bearer proxy-token" {
			sawAuth.Store(true)
		}
		_, _ = io.WriteString(w, `{"models":[]}`)
	})
	if _, err := provider.Models(context.Background()); err != nil {
		t.Fatalf("Models: %v", err)
	}
	if !sawAuth.Load() {
		t.Fatal("配了 APIKey 时原生 /api/tags 也要带鉴权")
	}
}

// TestModelsCustomHeaders 覆盖 modelHeaders 的用户头部合并。
func TestModelsCustomHeaders(t *testing.T) {
	var sawTenant atomic.Bool
	provider, _ := newTestProvider(t, func(cfg *Config) {
		cfg.Headers = map[string]string{"X-Tenant": "acme"}
	}, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Tenant") == "acme" {
			sawTenant.Store(true)
		}
		_, _ = io.WriteString(w, `{"models":[]}`)
	})
	if _, err := provider.Models(context.Background()); err != nil {
		t.Fatalf("Models: %v", err)
	}
	if !sawTenant.Load() {
		t.Fatal("用户头部没有带到原生 API")
	}
}

func TestModelsShapes(t *testing.T) {
	t.Run("跳过无名条目与空列表", func(t *testing.T) {
		provider, _ := newTestProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"models":[{"name":"keep:1b"},{"name":"  "},{"model":""}]}`)
		})
		models, err := provider.Models(context.Background())
		if err != nil {
			t.Fatalf("Models: %v", err)
		}
		if len(models) != 1 || models[0].ID != "keep:1b" {
			t.Fatalf("models = %+v", models)
		}
	})
	t.Run("只有 model 字段", func(t *testing.T) {
		provider, _ := newTestProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"models":[{"model":"only-model:2b"}]}`)
		})
		models, err := provider.Models(context.Background())
		if err != nil {
			t.Fatalf("Models: %v", err)
		}
		if len(models) != 1 || models[0].ID != "only-model:2b" {
			t.Fatalf("models = %+v", models)
		}
	})
	t.Run("空列表", func(t *testing.T) {
		provider, _ := newTestProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"models":[]}`)
		})
		if models, err := provider.Models(context.Background()); err != nil || len(models) != 0 {
			t.Fatalf("models=%+v err=%v", models, err)
		}
	})
	t.Run("坏 JSON", func(t *testing.T) {
		provider, _ := newTestProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"models":[`)
		})
		if _, err := provider.Models(context.Background()); err == nil ||
			!strings.Contains(err.Error(), "decode /api/tags") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("坏 modified_at 不致命", func(t *testing.T) {
		provider, _ := newTestProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"models":[{"name":"x:1b","modified_at":"not-a-time"}]}`)
		})
		models, err := provider.Models(context.Background())
		if err != nil {
			t.Fatalf("Models: %v", err)
		}
		if len(models) != 1 || !models[0].Modified.IsZero() {
			t.Fatalf("models = %+v", models)
		}
	})
	t.Run("5xx 重试耗尽", func(t *testing.T) {
		var calls atomic.Int32
		provider, _ := newTestProvider(t, func(cfg *Config) {
			cfg.MaxRetries = 1
			cfg.RetryBackoff = time.Millisecond
		}, func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			http.Error(w, "down", http.StatusBadGateway)
		})
		if _, err := provider.Models(context.Background()); err == nil {
			t.Fatal("必须报错")
		}
		if got := calls.Load(); got != 2 {
			t.Fatalf("调用次数 = %d, want 2", got)
		}
	})
}

func TestStreamRejectsNonSSE(t *testing.T) {
	provider, _ := newTestProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hi"}}]}`)
	})
	if _, err := provider.Stream(context.Background(), rig.Request{Model: "m"}); err == nil ||
		!strings.Contains(err.Error(), "text/event-stream") {
		t.Fatalf("err = %v", err)
	}
}

func TestStreamReadErrorMidway(t *testing.T) {
	provider, _ := newTestProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "1000")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
	})
	stream, err := provider.Stream(context.Background(), rig.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var lastErr error
	for {
		_, recvErr := stream.Recv()
		if recvErr != nil {
			lastErr = recvErr
			break
		}
	}
	if lastErr == nil || !strings.Contains(lastErr.Error(), "read stream") {
		t.Fatalf("err = %v", lastErr)
	}
}

func TestStreamContextCancel(t *testing.T) {
	provider, _ := newTestProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
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

func TestStreamCustomFinishReasonAlias(t *testing.T) {
	provider, _ := newTestProvider(t, func(cfg *Config) {
		cfg.Quirks = local.Quirks{FinishReasonAliases: map[string]string{"done_token": "stop"}}
	}, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, false, `{"choices":[{"delta":{"content":"x"},"finish_reason":"done_token"}]}`)
	})
	stream, err := provider.Stream(context.Background(), rig.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var finish string
	for _, chunk := range collect(t, stream) {
		if chunk.FinishReason != "" {
			finish = chunk.FinishReason
		}
	}
	if finish != "stop" {
		t.Fatalf("finish = %q", finish)
	}
}

func TestDisableStreamOptionsQuirk(t *testing.T) {
	var body map[string]any
	provider, _ := newTestProvider(t, func(cfg *Config) {
		cfg.Quirks = local.Quirks{DisableStreamOptions: true}
	}, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		writeSSE(w, true, `{"choices":[{"delta":{"content":"x"},"finish_reason":"stop"}]}`)
	})
	stream, err := provider.Stream(context.Background(), rig.Request{
		Model:    "m",
		Messages: []rig.Message{rig.NewMessage(rig.RoleUser, "hi")},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	collect(t, stream)
	if _, exists := body["stream_options"]; exists {
		t.Fatalf("DisableStreamOptions 未生效: %v", body["stream_options"])
	}
}

// TestStreamOllamaStyleToolCallObject 覆盖 ollama 兼容层可能给出的"对象型 arguments"。
func TestStreamOllamaStyleToolCallObject(t *testing.T) {
	provider, _ := newTestProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, true,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"get_weather","arguments":{"city":"北京"}}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		)
	})
	stream, err := provider.Stream(context.Background(), rig.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var arguments string
	for _, chunk := range collect(t, stream) {
		for _, call := range chunk.ToolCalls {
			arguments = call.Arguments
		}
	}
	if arguments != `{"city":"北京"}` {
		t.Fatalf("arguments = %q", arguments)
	}
}
