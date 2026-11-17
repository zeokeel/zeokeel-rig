package openai

// 审计用例（已修）：`Config.IncludeUsage` 之前是**假配置**（设 false 也照样发
// `stream_options.include_usage`）。现在改成 *bool：
//   nil  → 发送（默认：用量对成本/上下文统计很重要）
//   &false → 不发送（给不认识该字段的严格网关）
//   &true  → 发送（显式）

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

// capturedRequest 返回一个 provider + 一个读取"最后一次请求体"的函数。
func capturedRequest(t *testing.T, includeUsage *bool) (rig.Provider, func() map[string]any) {
	t.Helper()
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body = nil
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	provider, err := New(Config{
		BaseURL:      server.URL + "/v1",
		APIKey:       "k",
		DefaultModel: "m",
		HTTPClient:   server.Client(),
		RetryBackoff: time.Millisecond,
		IncludeUsage: includeUsage,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return provider, func() map[string]any { return body }
}

// drain 把流读完（触发请求）。
func drain(t *testing.T, provider rig.Provider) {
	t.Helper()
	stream, err := provider.Stream(context.Background(), rig.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() {
		if closer, ok := stream.(io.Closer); ok {
			_ = closer.Close()
		}
	}()
	for {
		if _, err := stream.Recv(); err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			t.Fatalf("Recv: %v", err)
		}
	}
}

func TestAuditIncludeUsageSwitch(t *testing.T) {
	disabled := false
	enabled := true
	cases := []struct {
		name        string
		value       *bool
		wantPresent bool
	}{
		{"nil（默认发送）", nil, true},
		{"显式 false（不发）", &disabled, false},
		{"显式 true（发）", &enabled, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider, captured := capturedRequest(t, tc.value)
			drain(t, provider)
			body := captured()
			if body == nil {
				t.Fatal("没有捕获到请求体")
			}
			options, present := body["stream_options"]
			if present != tc.wantPresent {
				t.Fatalf("stream_options 存在性 = %v, want %v（请求体: %v）", present, tc.wantPresent, body)
			}
			if present {
				fields, _ := options.(map[string]any)
				if fields["include_usage"] != true {
					t.Fatalf("stream_options.include_usage = %v, want true", fields["include_usage"])
				}
			}
		})
	}
}
