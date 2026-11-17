package ollama

// 本文件是"个人用户真的能用它吗"的审计用例（只新增测试、不改实现）：
// 连不上时的可执行提示、HTTP 状态码的可读性、模型列表为空是否被误当错误、
// 以及"模型不响应"时 ctx 取消能否打断（HeaderTimeout 默认 5 分钟会不会静默挂死）。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

// closedPort 返回一个"刚才还在、现在一定连不上"的本机地址。
func closedPort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return addr
}

// 审计 1：连不上时必须告诉用户"先启动 ollama"，而不是只甩 connection refused。
func TestAuditUnreachableHintIsActionable(t *testing.T) {
	provider, err := New(Config{
		BaseURL:      "http://" + closedPort(t),
		DefaultModel: "llama3.2",
		MaxRetries:   -1, // 审计只看文案，不重试
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = provider.Stream(context.Background(), rig.Request{Model: "llama3.2"})
	if err == nil {
		t.Fatal("连不上却返回了 nil 错误")
	}
	text := err.Error()
	// 逐字检查：前缀是 provider 名，且带可执行提示。
	if !strings.HasPrefix(text, "ollama:") {
		t.Fatalf("错误前缀不是 ollama: %q", text)
	}
	if !strings.Contains(text, "ollama serve") {
		t.Fatalf("错误里没有 `ollama serve` 提示（用户不知道该做什么）: %q", text)
	}
	if !strings.Contains(text, "未启动") {
		t.Fatalf("错误里没有「未启动」的提示: %q", text)
	}
	t.Logf("连不上时的错误文案: %s", text)
}

// 审计 2：Models 走原生 /api/tags，连不上时也要有同样的提示。
func TestAuditUnreachableHintOnModels(t *testing.T) {
	provider, err := New(Config{BaseURL: "http://" + closedPort(t), MaxRetries: -1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = provider.Models(context.Background())
	if err == nil {
		t.Fatal("Models 连不上却返回 nil 错误")
	}
	if !strings.Contains(err.Error(), "ollama serve") {
		t.Fatalf("Models 的错误缺少提示: %q", err)
	}
}

// 审计 3：401 / 403 / 500 是否可区分（用户要据此判断"是 key 错了还是服务挂了"）。
func TestAuditHTTPStatusMessages(t *testing.T) {
	cases := []struct {
		code int
		body string
		want string
	}{
		{http.StatusUnauthorized, `{"error":"unauthorized"}`, "401"},
		{http.StatusForbidden, `{"error":"forbidden"}`, "403"},
		{http.StatusInternalServerError, `{"error":"boom"}`, "500"},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%d", tc.code), func(t *testing.T) {
			provider, _ := newTestProvider(t, func(cfg *Config) {
				cfg.MaxRetries = -1
			}, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(tc.body))
			})
			_, err := provider.Stream(context.Background(), rig.Request{Model: "llama3.2"})
			if err == nil {
				t.Fatalf("%d 没有返回错误", tc.code)
			}
			text := err.Error()
			if !strings.Contains(text, tc.want) {
				t.Fatalf("错误里看不出状态码 %d: %q", tc.code, text)
			}
			if !strings.Contains(text, "boom") && !strings.Contains(text, "unauthorized") &&
				!strings.Contains(text, "forbidden") {
				t.Fatalf("错误里没有服务端响应体（用户无从判断原因）: %q", text)
			}
			// 已修：401/403 必须给出"检查 APIKey / 反代鉴权"的提示，5xx 提示可重试。
			switch tc.code {
			case http.StatusUnauthorized, http.StatusForbidden:
				if !strings.Contains(text, "APIKey") {
					t.Fatalf("%d 缺少检查 APIKey 的提示: %s", tc.code, text)
				}
			case http.StatusInternalServerError:
				if !strings.Contains(text, "重试") {
					t.Fatalf("500 缺少重试提示: %s", text)
				}
			}
			if !strings.Contains(text, "GET") && !strings.Contains(text, "POST") {
				t.Fatalf("错误里没有请求方法: %s", text)
			}
		})
	}
}

// 审计 4：模型列表为空**不是**错误（LM Studio / ollama 一个模型都没装时很常见）。
func TestAuditEmptyModelListIsNotAnError(t *testing.T) {
	provider, _ := newTestProvider(t, func(cfg *Config) { cfg.MaxRetries = -1 },
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[]}`))
		})
	models, err := provider.Models(context.Background())
	if err != nil {
		t.Fatalf("空列表被当成错误: %v", err)
	}
	if len(models) != 0 {
		t.Fatalf("空列表返回了 %d 项", len(models))
	}
}

// 审计 4b：/api/tags 返回 404（老版本 ollama 或路径被反代改写）时的错误是否可诊断。
func TestAuditModels404Diagnosable(t *testing.T) {
	provider, _ := newTestProvider(t, func(cfg *Config) { cfg.MaxRetries = -1 },
		func(w http.ResponseWriter, _ *http.Request) {
			http.NotFound(w, nil)
		})
	_, err := provider.Models(context.Background())
	if err == nil {
		t.Fatal("404 没有返回错误")
	}
	text := err.Error()
	if !strings.Contains(text, "404") {
		t.Fatalf("错误里没有 404: %q", text)
	}
	// 已修：错误里必须带请求路径，否则用户无法判断是 /api/tags 缺还是反代改写。
	if !strings.Contains(text, "/api/tags") {
		t.Fatalf("404 错误里没有请求路径: %s", text)
	}
	if !strings.Contains(text, "端点不存在") {
		t.Fatalf("404 错误里没有可执行提示: %s", text)
	}
	t.Logf("404 文案: %s", text)
}

// 审计 5：默认 HeaderTimeout 是 5 分钟（冷启动加载权重确实可能很慢）。
// 这里把"默认值"与"机制真的生效"分开验证。
func TestAuditHeaderTimeoutDefault(t *testing.T) {
	if DefaultHeaderTimeout != 5*time.Minute {
		t.Fatalf("DefaultHeaderTimeout = %v, want 5m", DefaultHeaderTimeout)
	}
}

// 审计 5b：服务端**只回响应头、不回 body** 时，ctx 取消必须能打断 Recv——
// 否则用户看到的就是"永远转圈"。
func TestAuditHeaderOnlyServerIsInterruptible(t *testing.T) {
	release := make(chan struct{})
	provider, _ := newTestProvider(t, func(cfg *Config) { cfg.MaxRetries = -1 },
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush() // 头已送达，body 永远不来
			}
			<-release
		})
	// cleanup 是 LIFO：这里后注册，因此先于 server.Close 执行，handler 才能退出。
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	stream, err := provider.Stream(ctx, rig.Request{Model: "llama3.2"})
	if err != nil {
		t.Fatalf("Stream（应当先拿到头）: %v", err)
	}
	defer func() {
		if closer, ok := stream.(io.Closer); ok {
			_ = closer.Close()
		}
	}()

	start := time.Now()
	_, err = stream.Recv()
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("只回头不回 body 时 Recv 返回了 nil 错误（会永远挂住）")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("错误不是 ctx 取消（errors.Is 失效，宿主无法分类）: %v", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("取消后 %v 才返回，太慢（用户会以为卡死）", elapsed)
	}
	t.Logf("ctx 取消在 %v 内打断（错误: %v）", elapsed, err)
}

// 审计 5c：**连响应头都不回** 的服务器，走默认 client 时 HeaderTimeout 生效。
func TestAuditNoHeaderServerTimesOut(t *testing.T) {
	release := make(chan struct{})
	provider, _ := newTestProvider(t, func(cfg *Config) {
		cfg.MaxRetries = -1
		cfg.HeaderTimeout = 120 * time.Millisecond
		cfg.HTTPClient = nil // 用本包自建的 client，HeaderTimeout 才生效
	}, func(w http.ResponseWriter, _ *http.Request) {
		<-release // 不写头，也不写 body
	})
	t.Cleanup(func() { close(release) })

	start := time.Now()
	_, err := provider.Stream(context.Background(), rig.Request{Model: "llama3.2"})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("不回头的服务器没有超时")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("HeaderTimeout=120ms 却等了 %v", elapsed)
	}
	if !strings.Contains(err.Error(), "ollama:") {
		t.Fatalf("错误缺少 provider 前缀: %q", err)
	}
	t.Logf("无响应头时的错误（%v）: %v", elapsed, err)
}

// 审计 5d（已修，P1）：传了自定义 `Config.HTTPClient` 时 `HeaderTimeout` 也必须生效。
// 修复前它被静默忽略（只有自建 client 才设 ResponseHeaderTimeout），服务端不回响应
// 头时会一直挂到宿主 ctx 过期。
func TestAuditCustomClientHeaderTimeoutApplies(t *testing.T) {
	release := make(chan struct{})
	provider, _ := newTestProvider(t, func(cfg *Config) {
		cfg.MaxRetries = -1
		cfg.HeaderTimeout = 50 * time.Millisecond
	}, func(w http.ResponseWriter, _ *http.Request) {
		<-release // 不写头
	})
	t.Cleanup(func() { close(release) })

	// ctx 给 3s：若 HeaderTimeout 没生效就会等到 3s。
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	_, err := provider.Stream(ctx, rig.Request{Model: "llama3.2"})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("服务端不回响应头却没有报错")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("HeaderTimeout=50ms 没有生效：等了 %v（错误: %v）", elapsed, err)
	}
	if !strings.Contains(err.Error(), "ollama:") {
		t.Fatalf("错误缺少 provider 前缀: %v", err)
	}
	t.Logf("自定义 client + HeaderTimeout=50ms → %v 返回: %v", elapsed, err)
}

// 审计 6：走**真实 ollama 兼容层路径**（/v1/chat/completions）跑一次完整 Stream，
// 覆盖 message 尾帧 + 对象型 arguments + 无 [DONE] 的组合（这是 ollama 最常见的
// 真实形态）。断言最终装配出的文本与工具调用。
func TestAuditEndToEndOllamaShapes(t *testing.T) {
	provider, _ := newTestProvider(t, func(cfg *Config) { cfg.MaxRetries = -1 },
		func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/chat/completions" {
				t.Errorf("请求路径 = %q, want /v1/chat/completions", r.URL.Path)
			}
			if got := r.Header.Get("Authorization"); got != "" {
				t.Errorf("默认不该发 Authorization，实际 %q", got)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			// 第一帧：正文；最后一帧：整条 message + 对象型 tool_calls；没有 [DONE]。
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"让我看看\"}}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"message\":{\"role\":\"assistant\",\"content\":\"这个文件。\",\"tool_calls\":[{\"id\":\"call_9\",\"type\":\"function\",\"function\":{\"name\":\"read_file\",\"arguments\":{\"path\":\"main.go\"}}}]},\"finish_reason\":\"tool_calls\"}]}\n\n")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		})

	stream, err := provider.Stream(context.Background(), rig.Request{Model: "llama3.2"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() {
		if closer, ok := stream.(io.Closer); ok {
			_ = closer.Close()
		}
	}()

	var text strings.Builder
	var calls []rig.ToolCallDelta
	var finish string
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		text.WriteString(chunk.Delta)
		calls = append(calls, chunk.ToolCalls...)
		if chunk.FinishReason != "" {
			finish = chunk.FinishReason
		}
	}
	if got := text.String(); got != "让我看看这个文件。" {
		t.Fatalf("正文 = %q, want 拼接后的完整文本", got)
	}
	if len(calls) != 1 || calls[0].Name != "read_file" {
		t.Fatalf("工具调用 = %+v", calls)
	}
	if calls[0].Arguments != `{"path":"main.go"}` {
		t.Fatalf("对象型 arguments 归一化 = %q", calls[0].Arguments)
	}
	if finish != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls（tool_call 应被归一）", finish)
	}
}
