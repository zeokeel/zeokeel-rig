package local

// 审计用例（已修）：非 2xx 错误必须给出"哪个端点 + 该怎么办"，而不只是状态码。

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
)

// 401 必须带请求方法+URL，以及"检查 APIKey"的提示。
func TestAuditErrorCarriesEndpointAndAuthHint(t *testing.T) {
	provider := newTestProvider(t, func(cfg *Config) { cfg.MaxRetries = -1 },
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
		})
	_, err := provider.Stream(context.Background(), rig.Request{Model: "m"})
	if err == nil {
		t.Fatal("401 没有返回错误")
	}
	text := err.Error()
	for _, want := range []string{
		"local:",
		"POST",
		"/chat/completions", // URL 必须出现，否则用户不知道哪个端点
		"401",
		"invalid api key", // 服务端原因
		"APIKey",          // 可执行提示
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("401 错误缺少 %q: %s", want, text)
		}
	}
	t.Logf("401 文案: %s", text)
}

// 500 必须提示"可稍后重试 / 看服务日志"。
func TestAuditErrorCarriesServerHint(t *testing.T) {
	provider := newTestProvider(t, func(cfg *Config) { cfg.MaxRetries = -1 },
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
		})
	_, err := provider.Stream(context.Background(), rig.Request{Model: "m"})
	if err == nil {
		t.Fatal("500 没有返回错误")
	}
	text := err.Error()
	if !strings.Contains(text, "500") || !strings.Contains(text, "重试") {
		t.Fatalf("500 错误缺少状态码或重试提示: %s", text)
	}
	t.Logf("500 文案: %s", text)
}

// 404 必须带路径：本地服务被反代改写路径时，只有状态码无法定位。
func TestAuditErrorCarriesPathOn404(t *testing.T) {
	provider := newTestProvider(t, func(cfg *Config) { cfg.MaxRetries = -1 },
		func(w http.ResponseWriter, _ *http.Request) {
			http.NotFound(w, nil)
		})
	_, err := provider.Stream(context.Background(), rig.Request{Model: "m"})
	if err == nil {
		t.Fatal("404 没有返回错误")
	}
	text := err.Error()
	if !strings.Contains(text, "/chat/completions") {
		t.Fatalf("404 错误里没有请求路径: %s", text)
	}
	if !strings.Contains(text, "端点不存在") {
		t.Fatalf("404 错误里没有可执行提示: %s", text)
	}
}

// 400（本地模型不支持 tools 时最常见）也要有线索。
func TestAuditErrorCarriesBadRequestBodyHint(t *testing.T) {
	provider := newTestProvider(t, func(cfg *Config) { cfg.MaxRetries = -1 },
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"unknown field tools"}`))
		})
	_, err := provider.Stream(context.Background(), rig.Request{Model: "m"})
	if err == nil {
		t.Fatal("400 没有返回错误")
	}
	text := err.Error()
	if !strings.Contains(text, "unknown field tools") || !strings.Contains(text, "不支持") {
		t.Fatalf("400 错误缺服务端原因或提示: %s", text)
	}
}
