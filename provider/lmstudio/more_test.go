package lmstudio

// 本文件补齐 lmstudio 包自身函数的分支覆盖（New/normalizeRoot/orDefault/Endpoint），
// 以及它通过共享基座承担的用户可见行为：非 SSE 响应、读流中断、ctx 取消、
// 自定义 finish_reason 别名、DisableStreamOptions、模型列表的各种形状与错误、
// 以及"连不上"提示的逐字文案。厂商包很薄，但薄不等于可以不测。

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
	provider, err := New(Config{BaseURL: "http://127.0.0.1:4321"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	endpoint := provider.Endpoint()
	if endpoint.Name != Name || endpoint.ModelsPath != "/v1/models" {
		t.Fatalf("endpoint = %+v", endpoint)
	}
	if endpoint.DefaultBaseURL != "http://127.0.0.1:4321" {
		t.Fatalf("DefaultBaseURL = %q（应回显实际服务根）", endpoint.DefaultBaseURL)
	}
	pkg := Endpoint()
	if pkg.Name != endpoint.Name || pkg.ModelsPath != endpoint.ModelsPath || pkg.AuthRequired != endpoint.AuthRequired {
		t.Fatalf("包级与实例描述不一致: %+v vs %+v", pkg, endpoint)
	}
	// 配了 key 时实例描述要如实报告需要鉴权。
	withKey, err := New(Config{APIKey: "secret"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !withKey.Endpoint().AuthRequired {
		t.Fatal("配了 APIKey 时 AuthRequired 应为 true")
	}
	// 带路径前缀的地址也要回显正确。
	prefixed, err := New(Config{BaseURL: "http://gateway/lmstudio/v1"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := prefixed.Endpoint().DefaultBaseURL; got != "http://gateway/lmstudio" {
		t.Fatalf("DefaultBaseURL = %q", got)
	}
}

func TestNewErrorPrefixesAndWrapping(t *testing.T) {
	cases := map[string]string{
		"非法 scheme": "gopher://127.0.0.1",
		"缺 host":    "http://",
		"无法解析":      "http://%zz",
	}
	for name, base := range cases {
		base := base
		t.Run(name, func(t *testing.T) {
			_, err := New(Config{BaseURL: base})
			if err == nil {
				t.Fatalf("BaseURL=%q 必须构造失败", base)
			}
			if !strings.HasPrefix(err.Error(), "lmstudio:") {
				t.Fatalf("错误必须以 lmstudio: 开头（宿主据此归因）: %v", err)
			}
		})
	}
}

// TestHeaderTimeoutDefaults 覆盖 orDefault 的正值分支与默认值本身。
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
	if got := orDefault(7*time.Second, DefaultHeaderTimeout); got != 7*time.Second {
		t.Fatalf("orDefault(正值) = %s", got)
	}
	// 构造时正值被采信（不 panic、不报错）。
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
	if got, err := normalizeRoot("http://127.0.0.1:1234/v1/"); err != nil || got != "http://127.0.0.1:1234" {
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
	const want = "LM Studio 未启动？打开 LM Studio → Developer → 开启 Local Server（默认端口 1234）"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("提示文案不匹配:\n got: %v\nwant: %s", err, want)
	}
	if !strings.HasPrefix(err.Error(), "lmstudio:") {
		t.Fatalf("错误前缀 = %v", err)
	}
}

// TestStreamRejectsNonSSE 覆盖 2xx 但不是 SSE 的响应（本地服务误配时最常见）。
func TestStreamRejectsNonSSE(t *testing.T) {
	provider, _ := newTestProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hi"}}]}`)
	})
	_, err := provider.Stream(context.Background(), rig.Request{Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "text/event-stream") {
		t.Fatalf("err = %v", err)
	}
}

// TestStreamReadErrorMidway 覆盖读流被截断。
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

// TestStreamContextCancel 覆盖取消传播。
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
		cfg.Quirks = local.Quirks{FinishReasonAliases: map[string]string{"EOS_TOKEN": "stop"}}
	}, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, false, `{"choices":[{"delta":{"content":"x"},"finish_reason":"eos_token"}]}`)
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
		t.Fatalf("finish = %q, want stop", finish)
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

// TestModelsRetryExhausted 覆盖 5xx 重试耗尽后的错误。
func TestModelsRetryExhausted(t *testing.T) {
	var calls atomic.Int32
	provider, _ := newTestProvider(t, func(cfg *Config) {
		cfg.MaxRetries = 1
		cfg.RetryBackoff = time.Millisecond
	}, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "still down", http.StatusInternalServerError)
	})
	_, err := provider.Models(context.Background())
	if err == nil || !strings.Contains(err.Error(), "still down") {
		t.Fatalf("err = %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("调用次数 = %d, want 2（一次重试）", got)
	}
}

func TestModelsBadJSON(t *testing.T) {
	provider, _ := newTestProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":[`)
	})
	_, err := provider.Models(context.Background())
	if err == nil || !strings.Contains(err.Error(), "decode model list") {
		t.Fatalf("err = %v", err)
	}
}

// TestModelsShapes 覆盖 data / models 两种形状、空列表、缺失上下文字段。
func TestModelsShapes(t *testing.T) {
	t.Run("data 形状", func(t *testing.T) {
		provider, _ := newTestProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"data":[{"id":"a","owned_by":"me"},{"id":"b","max_context_length":4096}]}`)
		})
		models, err := provider.Models(context.Background())
		if err != nil {
			t.Fatalf("Models: %v", err)
		}
		if len(models) != 2 || models[0].Owner != "me" || models[1].ContextLength != 4096 {
			t.Fatalf("models = %+v", models)
		}
	})
	t.Run("models 形状", func(t *testing.T) {
		provider, _ := newTestProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"models":[{"model":"granite:2b","size":99}]}`)
		})
		models, err := provider.Models(context.Background())
		if err != nil {
			t.Fatalf("Models: %v", err)
		}
		if len(models) != 1 || models[0].ID != "granite:2b" || models[0].Size != 99 {
			t.Fatalf("models = %+v", models)
		}
	})
	t.Run("空列表", func(t *testing.T) {
		provider, _ := newTestProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"object":"list","data":[]}`)
		})
		models, err := provider.Models(context.Background())
		if err != nil || len(models) != 0 {
			t.Fatalf("models=%+v err=%v", models, err)
		}
	})
	t.Run("缺失上下文字段与无名条目", func(t *testing.T) {
		provider, _ := newTestProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"data":[{"id":"ok"},{"owned_by":"nobody"}]}`)
		})
		models, err := provider.Models(context.Background())
		if err != nil {
			t.Fatalf("Models: %v", err)
		}
		if len(models) != 1 || models[0].ID != "ok" || models[0].ContextLength != 0 {
			t.Fatalf("models = %+v（无名条目应被跳过）", models)
		}
	})
	t.Run("401 带响应体", func(t *testing.T) {
		provider, _ := newTestProvider(t, func(cfg *Config) { cfg.MaxRetries = -1 }, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		})
		if _, err := provider.Models(context.Background()); err == nil ||
			!strings.Contains(err.Error(), "unauthorized") {
			t.Fatalf("err = %v", err)
		}
	})
}
