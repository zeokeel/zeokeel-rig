package ollama

// 审计用例：`Config.Headers` / `Config.APIKey` 对**两类请求**（对话兼容层与原生
// /api/tags）都生效——包文档现在就是这么写的，这里把它钉住。

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
)

func TestAuditHeadersApplyToChatAndTags(t *testing.T) {
	seen := map[string]http.Header{}
	provider, _ := newTestProvider(t, func(cfg *Config) {
		cfg.APIKey = "secret-key"
		cfg.Headers = map[string]string{"X-Org-Id": "org-42"}
		cfg.MaxRetries = -1
	}, func(w http.ResponseWriter, r *http.Request) {
		seen[r.URL.Path] = r.Header.Clone()
		switch r.URL.Path {
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
		case "/api/tags":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"models":[{"name":"llama3.2"}]}`)
		default:
			http.NotFound(w, r)
		}
	})

	// 对话请求。
	stream, err := provider.Stream(context.Background(), rig.Request{Model: "llama3.2"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for {
		if _, err := stream.Recv(); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("Recv: %v", err)
		}
	}
	if closer, ok := stream.(io.Closer); ok {
		_ = closer.Close()
	}

	// 列表请求。
	if _, err := provider.Models(context.Background()); err != nil {
		t.Fatalf("Models: %v", err)
	}

	for _, path := range []string{"/v1/chat/completions", "/api/tags"} {
		header, ok := seen[path]
		if !ok {
			t.Fatalf("没有收到 %s 请求", path)
		}
		if got := header.Get("X-Org-Id"); got != "org-42" {
			t.Fatalf("%s 缺少自定义头: %q", path, got)
		}
		if got := header.Get("Authorization"); got != "Bearer secret-key" {
			t.Fatalf("%s 的 Authorization = %q, want Bearer secret-key", path, got)
		}
	}
}
