package local

// 审计用例（已修）：注入自定义 HTTPClient 时，`Config.HeaderTimeout` 必须仍然生效，
// 或者至少**明确告知**它不归我们管——绝不能静默丢掉这道兜底。

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

// TestAuditCustomClientHeaderTimeoutStillApplies：自定义 client（Transport 是普通的
// *http.Transport 且没设 ResponseHeaderTimeout）+ HeaderTimeout=50ms + 服务端不回
// 响应头 → 必须约 50ms 就返回，而不是等到 ctx。
func TestAuditCustomClientHeaderTimeoutStillApplies(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
	}))
	server.Config.ErrorLog = nil
	t.Cleanup(func() {
		close(release)
		server.Close()
	})

	// 关键：注入自己的 client（Transport 没设 ResponseHeaderTimeout）。
	custom := &http.Client{Transport: &http.Transport{}}
	provider, err := New(Config{
		BaseURL:       server.URL + "/v1",
		DefaultModel:  "m",
		HTTPClient:    custom,
		HeaderTimeout: 50 * time.Millisecond,
		MaxRetries:    -1,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// ctx 给 3s：如果 HeaderTimeout 没生效，这里会等到 ctx 才返回（即 >=1s）。
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	_, err = provider.Stream(ctx, rig.Request{Model: "m"})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("服务端不回响应头却没有报错")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("HeaderTimeout=50ms 没有生效：等了 %v（错误: %v）", elapsed, err)
	}
	// 注意：http.Transport 的 header 超时错误本身也满足 errors.Is(ctx.DeadlineExceeded)，
	// 所以这里只能看**耗时**（50ms vs ctx 的 3s）与错误文本。
	if elapsed > 500*time.Millisecond {
		t.Fatalf("耗时 %v 说明 header 超时没生效（ctx 是 3s）", elapsed)
	}
	if !strings.Contains(err.Error(), "response headers") {
		t.Fatalf("错误不是 header 超时: %v", err)
	}
	t.Logf("自定义 client + HeaderTimeout=50ms → %v 返回: %v", elapsed, err)
}

// TestAuditCustomClientExplicitTimeoutIsRespected：调用方自己设了
// ResponseHeaderTimeout 时不能被覆盖（尊重显式配置）。
func TestAuditCustomClientExplicitTimeoutIsRespected(t *testing.T) {
	resolved := resolveHTTPClient(&http.Client{
		Transport: &http.Transport{ResponseHeaderTimeout: 123 * time.Millisecond},
	}, 9*time.Second)
	transport, ok := resolved.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport 类型 = %T", resolved.Transport)
	}
	if transport.ResponseHeaderTimeout != 123*time.Millisecond {
		t.Fatalf("覆盖了调用方显式设置的 ResponseHeaderTimeout: %v", transport.ResponseHeaderTimeout)
	}
}

// TestAuditCustomClientPreservesRedirectAndJar：补超时时必须保留 CheckRedirect/Jar/Timeout。
func TestAuditCustomClientPreservesRedirectAndJar(t *testing.T) {
	redirects := 0
	jar, _ := newCookieJar()
	original := &http.Client{
		Transport:     &http.Transport{},
		CheckRedirect: func(*http.Request, []*http.Request) error { redirects++; return nil },
		Jar:           jar,
		Timeout:       7 * time.Second,
	}
	resolved := resolveHTTPClient(original, 100*time.Millisecond)
	if resolved == original {
		t.Fatal("应当返回克隆后的 client，而不是原对象")
	}
	if resolved.CheckRedirect == nil {
		t.Fatal("CheckRedirect 丢了")
	}
	if err := resolved.CheckRedirect(nil, nil); err != nil || redirects != 1 {
		t.Fatalf("CheckRedirect 没被保留: redirects=%d err=%v", redirects, err)
	}
	if resolved.Jar != jar {
		t.Fatal("Jar 丢了")
	}
	if resolved.Timeout != 7*time.Second {
		t.Fatalf("Timeout 丢了: %v", resolved.Timeout)
	}
	transport, ok := resolved.Transport.(*http.Transport)
	if !ok || transport.ResponseHeaderTimeout != 100*time.Millisecond {
		t.Fatalf("没有补上 header 超时: %+v", resolved.Transport)
	}
	// 原对象不能被修改（并发使用同一个 client 的宿主会受影响）。
	if original.Transport.(*http.Transport).ResponseHeaderTimeout != 0 {
		t.Fatal("修改了调用方传入的 Transport（应当克隆）")
	}
}

// TestAuditCustomRoundTripperWarns：自定义 RoundTripper 无法设置 header 超时 →
// 必须用 slog.Warn 明确告知（不能静默）。
func TestAuditCustomRoundTripperWarns(t *testing.T) {
	var buffer bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buffer, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("not used")
	})}
	resolved := resolveHTTPClient(client, 40*time.Millisecond)
	if resolved != client {
		t.Fatal("自定义 RoundTripper 时应原样返回（无法替它设超时）")
	}
	text := buffer.String()
	if !strings.Contains(text, "RoundTripper") {
		t.Fatalf("告警里没有说明原因: %q", text)
	}
	if !strings.Contains(text, "ResponseHeaderTimeout") {
		t.Fatalf("告警里没有给出修法: %q", text)
	}
	t.Logf("告警内容: %s", strings.TrimSpace(text))
}

// TestAuditNilTransportUsesDefaultAndGetsTimeout：Transport 为 nil（等价
// DefaultTransport）时也要补上超时。
func TestAuditNilTransportUsesDefaultAndGetsTimeout(t *testing.T) {
	resolved := resolveHTTPClient(&http.Client{}, 250*time.Millisecond)
	transport, ok := resolved.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport 类型 = %T", resolved.Transport)
	}
	if transport.ResponseHeaderTimeout != 250*time.Millisecond {
		t.Fatalf("ResponseHeaderTimeout = %v", transport.ResponseHeaderTimeout)
	}
	if http.DefaultTransport.(*http.Transport).ResponseHeaderTimeout != 0 {
		t.Fatal("污染了全局 http.DefaultTransport")
	}
}

// roundTripperFunc 让函数满足 http.RoundTripper。
type roundTripperFunc func(*http.Request) (*http.Response, error)

// RoundTrip 实现 http.RoundTripper。
func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

// newCookieJar 造一个 cookie jar（测试用）。
func newCookieJar() (http.CookieJar, error) {
	// 用 net/http/cookiejar 需要额外 import；这里用一个最小实现避免依赖。
	return &noopJar{}, nil
}

// noopJar 是一个空实现的 cookie jar。
type noopJar struct{}

// SetCookies 实现 http.CookieJar。
func (j *noopJar) SetCookies(*url.URL, []*http.Cookie) {}

// Cookies 实现 http.CookieJar。
func (j *noopJar) Cookies(*url.URL) []*http.Cookie { return nil }
