package openai

// P3-10 reasoning effort 的测试：Config.DefaultReasoningEffort 的校验、请求 JSON
// 里的 reasoning_effort、以及显式 Request.Extra 对默认值的覆盖。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

// newReasoningProvider 启动一个捕获请求体的测试服务器，返回指向它的 provider 与
// 读取最近一次请求体的函数。请求体只能在 Stream 返回且服务器处理完成后读取。
func newReasoningProvider(t *testing.T, effort string) (*Provider, func() map[string]any) {
	t.Helper()
	var body map[string]any
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		writeSSE(w)
		close(done)
	}))
	t.Cleanup(server.Close)
	provider, err := New(Config{
		BaseURL:                server.URL,
		APIKey:                 "test-key",
		DefaultModel:           "test-model",
		HTTPClient:             server.Client(),
		RetryBackoff:           time.Millisecond,
		DefaultReasoningEffort: effort,
	})
	if err != nil {
		t.Fatalf("New(DefaultReasoningEffort=%q): %v", effort, err)
	}
	return provider, func() map[string]any {
		<-done
		return body
	}
}

// streamOnce 发起一次最小请求；测试只关心请求体，不消费响应流。
func streamOnce(t *testing.T, provider *Provider, extra map[string]any) {
	t.Helper()
	if _, err := provider.Stream(context.Background(), rig.Request{
		Messages: []rig.Message{rig.NewMessage(rig.RoleUser, "hi")},
		Extra:    extra,
	}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
}

func TestDefaultReasoningEffortInRequestJSON(t *testing.T) {
	provider, body := newReasoningProvider(t, "high")
	streamOnce(t, provider, nil)
	got := body()
	if effort, ok := got["reasoning_effort"]; !ok || effort != "high" {
		t.Fatalf("reasoning_effort = %v (present=%v), want high", effort, ok)
	}
	if got["model"] != "test-model" {
		t.Fatalf("model = %v, want test-model", got["model"])
	}
	if _, ok := got["messages"]; !ok {
		t.Fatal("request body has no messages")
	}
}

func TestExplicitExtraOverridesDefaultReasoningEffort(t *testing.T) {
	provider, body := newReasoningProvider(t, "high")
	extra := map[string]any{"reasoning_effort": "low", "custom_option": 7}
	streamOnce(t, provider, extra)
	got := body()
	if effort := got["reasoning_effort"]; effort != "low" {
		t.Fatalf("reasoning_effort = %v, want explicit low override", effort)
	}
	if value := got["custom_option"]; value != float64(7) {
		t.Fatalf("custom_option = %v, want 7", value)
	}
	if len(extra) != 2 {
		t.Fatalf("caller's Extra map was mutated: %#v", extra)
	}
}

func TestNoDefaultReasoningEffortOmitsField(t *testing.T) {
	provider, body := newReasoningProvider(t, "")
	streamOnce(t, provider, nil)
	got := body()
	if effort, ok := got["reasoning_effort"]; ok {
		t.Fatalf("reasoning_effort = %v, want field omitted when default is empty", effort)
	}
}

func TestDefaultReasoningEffortDoesNotMutateRequestExtra(t *testing.T) {
	provider, body := newReasoningProvider(t, "medium")
	extra := map[string]any{"custom_option": "x"}
	streamOnce(t, provider, extra)
	got := body()
	if effort := got["reasoning_effort"]; effort != "medium" {
		t.Fatalf("reasoning_effort = %v, want medium", effort)
	}
	if len(extra) != 1 {
		t.Fatalf("caller's Extra map was mutated: %#v", extra)
	}
	if _, injected := extra["reasoning_effort"]; injected {
		t.Fatalf("default effort leaked into caller's Extra: %#v", extra)
	}
}

func TestReasoningEffortNormalization(t *testing.T) {
	provider, body := newReasoningProvider(t, "  HIGH  ")
	streamOnce(t, provider, nil)
	if effort := body()["reasoning_effort"]; effort != "high" {
		t.Fatalf("normalized reasoning_effort = %v, want high", effort)
	}
}

func TestReasoningEffortValidation(t *testing.T) {
	for _, value := range []string{"", "low", "medium", "high", " HIGH ", "Medium"} {
		if _, err := New(Config{DefaultReasoningEffort: value}); err != nil {
			t.Errorf("New(DefaultReasoningEffort=%q) err = %v, want nil", value, err)
		}
	}
	for _, value := range []string{"xhigh", "none", "low,high", "low high"} {
		if _, err := New(Config{DefaultReasoningEffort: value}); err == nil {
			t.Errorf("New(DefaultReasoningEffort=%q) err = nil, want validation error", value)
		}
	}
}
