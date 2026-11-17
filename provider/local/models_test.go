package local

// 本文件覆盖模型列表探测（GET /v1/models）：解析、字段兼容、错误分类、context 取消。

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestModelsParsesOpenAIList(t *testing.T) {
	var sawPath, sawAccept string
	provider := newTestProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		sawAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[
			{"id":"llama3.2:latest","object":"model","owned_by":"library","created":1700000000},
			{"id":"qwen2.5-coder:7b","object":"model","owned_by":"library"}
		]}`)
	})
	models, err := provider.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if sawPath != "/v1/models" {
		t.Fatalf("path = %q, want /v1/models", sawPath)
	}
	if !strings.Contains(sawAccept, "application/json") {
		t.Fatalf("Accept = %q（模型列表不该要求 SSE）", sawAccept)
	}
	if len(models) != 2 {
		t.Fatalf("模型数 = %d", len(models))
	}
	// 按 ID 排序。
	if models[0].ID != "llama3.2:latest" || models[1].ID != "qwen2.5-coder:7b" {
		t.Fatalf("排序不对: %+v", models)
	}
	if models[0].Owner != "library" {
		t.Fatalf("Owner = %q", models[0].Owner)
	}
	if models[0].Modified.IsZero() || models[0].Modified.Unix() != 1700000000 {
		t.Fatalf("Modified = %v", models[0].Modified)
	}
	if models[0].Name != "llama3.2:latest" {
		t.Fatalf("Name 应回退到 ID: %q", models[0].Name)
	}
}

func TestModelsAcceptsModelsKey(t *testing.T) {
	provider := newTestProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"models":[{"model":"granite3.1-dense:2b","size":1234,"modified_at":"2025-01-02T03:04:05Z"}]}`)
	})
	models, err := provider.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 1 || models[0].ID != "granite3.1-dense:2b" {
		t.Fatalf("models = %+v", models)
	}
	if models[0].Size != 1234 {
		t.Fatalf("Size = %d", models[0].Size)
	}
	if got := models[0].Modified.Format(time.RFC3339); got != "2025-01-02T03:04:05Z" {
		t.Fatalf("Modified = %q", got)
	}
}

func TestModelsContextLength(t *testing.T) {
	provider := newTestProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":[
			{"id":"a","context_length":8192},
			{"id":"b","max_context_length":32768},
			{"id":"c"}
		]}`)
	})
	models, err := provider.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	byID := map[string]ModelInfo{}
	for _, model := range models {
		byID[model.ID] = model
	}
	if byID["a"].ContextLength != 8192 || byID["b"].ContextLength != 32768 || byID["c"].ContextLength != 0 {
		t.Fatalf("ContextLength = %+v", byID)
	}
}

func TestModelsEmptyListIsNotAnError(t *testing.T) {
	provider := newTestProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"object":"list","data":[]}`)
	})
	models, err := provider.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 0 {
		t.Fatalf("models = %+v", models)
	}
}

func TestModelsStatusErrorIncludesBody(t *testing.T) {
	provider := newTestProvider(t, func(cfg *Config) { cfg.MaxRetries = -1 }, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
	})
	_, err := provider.Models(context.Background())
	if err == nil {
		t.Fatal("必须返回错误")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("err = %v", err)
	}
}

func TestModelsRetriesOnServerError(t *testing.T) {
	calls := 0
	provider := newTestProvider(t, func(cfg *Config) {
		cfg.MaxRetries = 2
		cfg.RetryBackoff = time.Millisecond
	}, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			http.Error(w, "overloaded", http.StatusBadGateway)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"m"}]}`)
	})
	models, err := provider.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 1 || calls != 3 {
		t.Fatalf("models=%v calls=%d", models, calls)
	}
}

func TestModelsContextCanceled(t *testing.T) {
	provider := newTestProvider(t, func(cfg *Config) { cfg.MaxRetries = -1 }, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := provider.Models(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestModelsRejectsMalformedBody(t *testing.T) {
	provider := newTestProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `not json`)
	})
	if _, err := provider.Models(context.Background()); err == nil {
		t.Fatal("坏 JSON 必须报错")
	}
}

func TestEndpointDescriptor(t *testing.T) {
	provider, err := New(Config{BaseURL: "http://127.0.0.1:9000/v1", Name: "custom"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	endpoint := provider.Endpoint()
	if endpoint.Name != "custom" || endpoint.DefaultBaseURL != "http://127.0.0.1:9000/v1" {
		t.Fatalf("endpoint = %+v", endpoint)
	}
	if endpoint.AuthRequired {
		t.Fatal("无 key 时 AuthRequired 应为 false")
	}
	if endpoint.ModelsPath != "/models" {
		t.Fatalf("ModelsPath = %q", endpoint.ModelsPath)
	}
	// ModelLister 接口可用（kit 通过它做模型预设）。
	var _ ModelLister = provider
}
