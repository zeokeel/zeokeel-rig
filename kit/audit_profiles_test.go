package kit_test

// 配置档（profile）与 provider 选择器的守门用例。
//
// 定位：个人用户不写配置文件。他要的是"本地模型 + 不打扰我"或"云模型 + 每步问我"，
// 所以这里给的是命名档，而不是十几个需要理解的开关。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/kit"
)

// TestProfilesAreDiscoverable 断言三档都存在且带人话说明。
func TestProfilesAreDiscoverable(t *testing.T) {
	all := kit.Profiles()
	for _, name := range []string{kit.ProfileCareful, kit.ProfilePersonal, kit.ProfileLocal} {
		p, ok := all[name]
		if !ok {
			t.Fatalf("Profiles() 缺少 %q", name)
		}
		if p.Note == "" {
			t.Fatalf("档 %q 没有 Note（设置界面要显示一句人话）", name)
		}
		if p.MaxIterations <= 0 {
			t.Fatalf("档 %q 的 MaxIterations = %d, want > 0（不能让用户面对无限循环）", name, p.MaxIterations)
		}
	}
	if _, ok := kit.LookupProfile("nope"); ok {
		t.Fatalf("未知档名不该被接受")
	}
	if _, ok := kit.LookupProfile("  PERSONAL "); !ok {
		t.Fatalf("档名应当 trim + 忽略大小写")
	}
}

// TestProfileFillsOnlyUnsetFields 断言档只填"宿主没设置"的字段。
func TestProfileFillsOnlyUnsetFields(t *testing.T) {
	// 只给 local 档：应当变成 auto 审批 + ollama provider kind。
	local, err := kit.New(kit.Config{
		Model:   "llama3.2",
		Root:    t.TempDir(),
		Profile: kit.ProfileLocal,
		Provider: &scriptProvider{scripts: [][]kit.Chunk{
			textScript("ok"),
		}},
	})
	if err != nil {
		t.Fatalf("New(local): %v", err)
	}
	if _, _, err := local.RunText(context.Background(), "s", "hi", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}

	// 未知档名必须报错（而不是静默忽略，否则用户以为选了档）。
	if _, err := kit.New(kit.Config{Model: "m", Root: t.TempDir(), Profile: "turbo"}); err == nil ||
		!strings.Contains(err.Error(), "unknown profile") {
		t.Fatalf("未知档名 err = %v, want 'unknown profile'", err)
	}
}

// TestProviderKindBuildsLocalProvider 断言 kind=ollama 会真的把请求发到本地地址，
// 而且不需要宿主 import 具体 provider 包。
func TestProviderKindBuildsLocalProvider(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		if flusher != nil {
			defer flusher.Flush()
		}
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer server.Close()

	provider, err := kit.BuildProvider(kit.ProviderOllama, kit.Config{
		Model:   "llama3.2",
		BaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("BuildProvider: %v", err)
	}
	stream, err := provider.Stream(context.Background(), kit.Request{Model: "llama3.2"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var text strings.Builder
	for {
		chunk, err := stream.Recv()
		if err != nil {
			break
		}
		text.WriteString(chunk.Delta)
	}
	if text.String() != "hello" {
		t.Fatalf("文本 = %q, want hello", text.String())
	}
	if !strings.Contains(gotPath, "/chat/completions") {
		t.Fatalf("请求路径 = %q, want 兼容层 /v1/chat/completions", gotPath)
	}
	if _, err := kit.BuildProvider("bogus", kit.Config{}); err == nil {
		t.Fatalf("未知 kind 应当报错（否则用户以为切了 provider）")
	}
}

// TestAvailableModelsForLocalProvider 断言"启动即用"：能从本地服务列出模型，
// 并把第一个模型名直接作为 Config.Model 使用。
func TestAvailableModelsForLocalProvider(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]any{
				{"name": "llama3.2:latest", "size": 12345, "modified_at": "2026-01-01T00:00:00Z"},
			},
		})
	}))
	defer server.Close()

	models, listable, err := kit.AvailableModels(context.Background(), kit.ProviderOllama,
		kit.Config{BaseURL: server.URL})
	if err != nil {
		t.Fatalf("AvailableModels: %v", err)
	}
	if !listable {
		t.Fatalf("ollama 应当能列出模型")
	}
	if len(models) != 1 || models[0].ID != "llama3.2:latest" {
		t.Fatalf("models = %+v, want one llama3.2:latest", models)
	}

	// 服务不可达时应当返回错误（而不是空列表让用户以为"没有模型"）。
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer dead.Close()
	if _, _, err := kit.AvailableModels(context.Background(), kit.ProviderOllama,
		kit.Config{BaseURL: dead.URL}); err == nil {
		t.Fatalf("服务返回 500 时应当报错")
	}
}
