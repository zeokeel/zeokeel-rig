package lmstudio

// 本文件是"个人用户真的能用它吗"的审计用例（只新增测试、不改实现）。
// LM Studio 与 ollama 共享 provider/local 基座，因此这里只钉住**它自己的**差异点：
// 默认地址、不可达提示文案、/v1/models 的空列表、以及 401 的可读性。

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/provider/local"
)

// closedPort 返回一个刚关闭的本机端口。
func closedPort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return addr
}

// 审计 1：默认地址必须是 LM Studio 的 1234，且提示要告诉用户在哪开 Local Server。
func TestAuditDefaultsAndUnreachableHint(t *testing.T) {
	if DefaultBaseURL != "http://127.0.0.1:1234" {
		t.Fatalf("DefaultBaseURL = %q", DefaultBaseURL)
	}
	provider, err := New(Config{BaseURL: "http://" + closedPort(t), MaxRetries: -1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stream, err := provider.Stream(context.Background(), rig.Request{Model: "qwen2.5-7b-instruct"})
	if err == nil {
		if closer, ok := stream.(io.Closer); ok {
			_ = closer.Close()
		}
		t.Fatal("连不上却成功")
	}
	text := err.Error()
	if !strings.HasPrefix(text, "lmstudio:") {
		t.Fatalf("错误前缀不是 lmstudio: %q", text)
	}
	if !strings.Contains(text, "Local Server") {
		t.Fatalf("错误里没有“打开 Local Server”的指引: %q", text)
	}
	if !strings.Contains(text, "1234") {
		t.Fatalf("错误里没有默认端口 1234: %q", text)
	}
	t.Logf("连不上时的错误文案: %s", text)
}

// 审计 2：/v1/models 返回空列表不是错误（LM Studio 没加载模型时就是这样）。
func TestAuditEmptyModelsIsNotAnError(t *testing.T) {
	provider, _ := newTestProvider(t, func(cfg *Config) { cfg.MaxRetries = -1 },
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[],"object":"list"}`))
		})
	models, err := provider.Models(context.Background())
	if err != nil {
		t.Fatalf("空列表被当成错误: %v", err)
	}
	if len(models) != 0 {
		t.Fatalf("空列表返回了 %d 项", len(models))
	}
}

// 审计 3：401（套了鉴权反代的 LM Studio）文案是否可读。
func TestAuditUnauthorizedMessage(t *testing.T) {
	provider, _ := newTestProvider(t, func(cfg *Config) { cfg.MaxRetries = -1 },
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
		})
	stream, err := provider.Stream(context.Background(), rig.Request{Model: "m"})
	if err == nil {
		if closer, ok := stream.(io.Closer); ok {
			_ = closer.Close()
		}
		t.Fatal("401 没有返回错误")
	}
	text := err.Error()
	if !strings.Contains(text, "401") || !strings.Contains(text, "invalid api key") {
		t.Fatalf("401 的错误缺少状态码或服务端原因: %q", text)
	}
	// 审计记录：目前只有状态码 + body，没有“检查 Config.APIKey / 反代”的提示。
	if !strings.Contains(text, "APIKey") {
		t.Logf("已知不足：401 没有提示检查 Config.APIKey: %s", text)
	}
}

// 审计 4：本地服务默认不发 Authorization（把用户的云端 key 发到 127.0.0.1 是泄漏）。
func TestAuditNoAuthorizationHeaderByDefault(t *testing.T) {
	var gotAuth string
	provider, _ := newTestProvider(t, nil, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n"))
	})
	stream, err := provider.Stream(context.Background(), rig.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if closer, ok := stream.(io.Closer); ok {
		defer func() { _ = closer.Close() }()
	}
	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}
	if gotAuth != "" {
		t.Fatalf("默认发送了 Authorization: %q（本地服务不需要，且可能泄漏云端 key）", gotAuth)
	}
}

// 审计 5：Endpoint() 的展示信息要与实际请求路径一致（provider 选择器靠它）。
func TestAuditEndpointMatchesRealPaths(t *testing.T) {
	provider, _ := newTestProvider(t, nil, func(w http.ResponseWriter, _ *http.Request) {})
	endpoint := provider.Endpoint()
	if endpoint.Name != Name {
		t.Fatalf("Endpoint.Name = %q", endpoint.Name)
	}
	if !strings.HasSuffix(endpoint.ModelsPath, "/models") {
		t.Fatalf("Endpoint.ModelsPath = %q", endpoint.ModelsPath)
	}
	if endpoint.AuthRequired {
		t.Fatal("本地 LM Studio 默认不需要鉴权，AuthRequired 应为 false")
	}
}

// 编译期：本包必须仍然实现 local.ModelLister（provider 选择器依赖它）。
var _ local.ModelLister = (*Provider)(nil)

// 防未使用告警（time 在审计里用于 HeaderTimeout 断言）。
var _ = time.Second

// 审计 6（已修，P1）：LM Studio 侧也要覆盖"自定义 HTTPClient 时 HeaderTimeout 仍生效"。
// 三个 provider（local/ollama/lmstudio）共用同一条修复路径，这里做端到端确认。
func TestAuditCustomClientHeaderTimeoutApplies(t *testing.T) {
	release := make(chan struct{})
	custom := &http.Client{Transport: &http.Transport{}}
	provider, _ := newTestProvider(t, func(cfg *Config) {
		cfg.MaxRetries = -1
		cfg.HeaderTimeout = 50 * time.Millisecond
		cfg.HTTPClient = custom
	}, func(w http.ResponseWriter, _ *http.Request) {
		<-release // 不写头，也不写 body
	})
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	_, err := provider.Stream(ctx, rig.Request{Model: "qwen2.5-7b-instruct"})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("服务端不回响应头却没有报错")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("HeaderTimeout=50ms 没有生效：等了 %v（错误: %v）", elapsed, err)
	}
	if !strings.Contains(err.Error(), "lmstudio:") {
		t.Fatalf("错误缺少 provider 前缀: %v", err)
	}
	t.Logf("自定义 client + HeaderTimeout=50ms → %v 返回: %v", elapsed, err)
}
