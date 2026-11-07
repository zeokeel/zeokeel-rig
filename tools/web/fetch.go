package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// fetchArgs 是 web_fetch 的参数对象。
type fetchArgs struct {
	URL      string `json:"url"`
	Format   string `json:"format"`
	MaxChars int    `json:"maxChars"`
}

// fetchConfig 是一个 fetcher 解析后的配置。
type fetchConfig struct {
	policy       Policy
	timeout      time.Duration
	maxBytes     int64
	maxChars     int
	userAgent    string
	transport    http.RoundTripper
	allowPrivate bool
}

func defaultFetchConfig() fetchConfig {
	return fetchConfig{
		timeout:   DefaultTimeout,
		maxBytes:  DefaultMaxBytes,
		maxChars:  DefaultMaxChars,
		userAgent: DefaultUserAgent,
	}
}

// FetchOption 用于定制 NewHTTPFetcher。
type FetchOption func(*fetchConfig)

// WithPolicy 限制 fetcher 可请求的 URL（初始 URL 与每一跳重定向）。nil policy 允许所有
// URL。
func WithPolicy(policy Policy) FetchOption {
	return func(config *fetchConfig) { config.policy = policy }
}

// WithTimeout 设置端到端请求超时（零或负数保留默认值）。
func WithTimeout(timeout time.Duration) FetchOption {
	return func(config *fetchConfig) {
		if timeout > 0 {
			config.timeout = timeout
		}
	}
}

// WithMaxBytes 设置响应体上限（零或负数保留默认值）。
func WithMaxBytes(maxBytes int64) FetchOption {
	return func(config *fetchConfig) {
		if maxBytes > 0 {
			config.maxBytes = maxBytes
		}
	}
}

// WithMaxChars 设置默认输出字符上限。
func WithMaxChars(maxChars int) FetchOption {
	return func(config *fetchConfig) {
		if maxChars > 0 {
			config.maxChars = maxChars
		}
	}
}

// WithUserAgent 覆盖 User-Agent 头（空值保留默认）。
func WithUserAgent(userAgent string) FetchOption {
	return func(config *fetchConfig) {
		if strings.TrimSpace(userAgent) != "" {
			config.userAgent = userAgent
		}
	}
}

// WithTransport 覆盖 HTTP transport（便于测试与代理）。客户端仍负责超时与重定向策略。
func WithTransport(transport http.RoundTripper) FetchOption {
	return func(config *fetchConfig) { config.transport = transport }
}

// WithAllowPrivateHosts 关闭内置的私有地址防护——该防护用于保护没有 HostResolver 的
// policy。它服务于刻意抓取回环/私网目标的宿主（本地镜像、集成测试）；处理不可信模型流量
// 时不要打开。实现 HostResolver 的 policy 无论如何都保留自己的校验，nil policy 从不安装
// 该防护。
func WithAllowPrivateHosts() FetchOption {
	return func(config *fetchConfig) { config.allowPrivate = true }
}

// fetchTool 是 web_fetch 的 rig.Tool 实现。
type fetchTool struct {
	client    *http.Client
	policy    Policy
	maxBytes  int64
	maxChars  int
	userAgent string
}

// NewHTTPFetcher 用 options 构建 web_fetch。HTTP 客户端在每一跳都执行总超时、重定向上限
// 与 policy 校验。
func NewHTTPFetcher(opts ...FetchOption) rig.Tool {
	config := defaultFetchConfig()
	for _, option := range opts {
		if option != nil {
			option(&config)
		}
	}
	transport := config.transport
	if transport == nil {
		transport = newPolicyTransport(config.policy, config.allowPrivate)
	}
	client := &http.Client{
		Timeout:   config.timeout,
		Transport: transport,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) > MaxRedirects {
				return fmt.Errorf("stopped after %d redirects", MaxRedirects)
			}
			if config.policy != nil {
				if err := config.policy.CheckURL(request.URL); err != nil {
					return fmt.Errorf("redirect blocked by policy: %w", err)
				}
			}
			return nil
		},
	}
	return &fetchTool{
		client:    client,
		policy:    config.policy,
		maxBytes:  config.maxBytes,
		maxChars:  config.maxChars,
		userAgent: config.userAgent,
	}
}

// newPolicyTransport 构建默认 transport：它总是自行解析要拨号的主机，使被校验地址在
// 校验与拨号之间无法改变（DNS rebinding）：
//
//   - 实现 HostResolver 的 policy 提供已验证 IP（例如 sandbox.Policy）；
//   - 其他任何非 nil policy 仍受保护：fetcher 自行解析域名并拒绝非公网地址，因此
//     web.PolicyFunc 与自定义 CheckURL 策略无法被诱骗去拨号回环、私网、链路本地或云
//     元数据目标；
//   - WithAllowPrivateHosts 会为刻意的私网目标关闭该兜底防护；
//   - nil policy 不安装任何防护（文档化的“允许所有 URL”退出方式），保留标准 dialer。
//
// 通过 WithTransport 传入的自定义 transport 会按原样使用，并由它自己负责地址校验。解析
// 并钉住 IP 的 dialer 也覆盖通过环境变量配置的 HTTP 代理，因此除非显式允许私网主机，回环
// 代理会被拒绝。
func newPolicyTransport(policy Policy, allowPrivate bool) *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if policy == nil {
		return transport
	}
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("web_fetch: invalid dial address %q: %w", addr, err)
		}
		ips, err := resolvePolicyHosts(ctx, policy, host, allowPrivate)
		if err != nil {
			return nil, err
		}
		var lastErr error
		for _, ip := range ips {
			conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if dialErr == nil {
				return conn, nil
			}
			lastErr = dialErr
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("no resolved address for %q", host)
		}
		return nil, lastErr
	}
	return transport
}

// resolvePolicyHosts 返回 host 的请求可以拨号的 IP 地址。
func resolvePolicyHosts(ctx context.Context, policy Policy, host string, allowPrivate bool) ([]net.IP, error) {
	if resolver, ok := policy.(HostResolver); ok {
		ips, err := resolver.ResolveHost(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("web_fetch: %w", err)
		}
		return ips, nil
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("web_fetch: resolve %q: %w", host, err)
	}
	if allowPrivate {
		ips := make([]net.IP, 0, len(addrs))
		for _, address := range addrs {
			ips = append(ips, address.IP)
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("web_fetch: no address for %q", host)
		}
		return ips, nil
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, address := range addrs {
		if isPublicIP(address.IP) {
			ips = append(ips, address.IP)
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf(
			"web_fetch: %q resolves only to non-public addresses; blocked to prevent SSRF (pass web.WithAllowPrivateHosts() to allow private targets)",
			host)
	}
	return ips, nil
}

// isPublicIP 判断 ip 是否为可公网路由的单播地址。它拒绝 SSRF 防护绝不能拨号的地址类别：
// 回环、私网、链路本地、组播、未指定、CGNAT 与保留段。
func isPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	if !ip.IsGlobalUnicast() {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		switch {
		case v4[0] == 0:
			return false
		case v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127: // 100.64.0.0/10 CGNAT 段
			return false
		case v4[0] == 192 && v4[1] == 0 && v4[2] == 0: // 192.0.0.0/24 IETF 段
			return false
		case v4[0] == 198 && (v4[1] == 18 || v4[1] == 19): // 198.18.0.0/15 基准测试段
			return false
		case v4[0] >= 240: // 240.0.0.0/4 保留段
			return false
		}
	}
	return true
}

// Name 实现 rig.Tool。
func (t *fetchTool) Name() string { return "web_fetch" }

// Description 实现 rig.Tool。
func (t *fetchTool) Description() string {
	return "Fetch a http(s) URL and return its content as text (HTML is converted to plain text) or, " +
		"with format=html, as raw HTML. Redirects are followed up to 5 hops, the request has a total timeout, " +
		"and both the response body and the output are capped. The URL must pass the host's policy."
}

// Schema 实现 rig.Tool。
func (t *fetchTool) Schema() map[string]any {
	return toolutil.ObjectSchema(map[string]any{
		"url": toolutil.StringProp("Absolute http(s) URL to fetch."),
		"format": map[string]any{
			"type":        "string",
			"enum":        []any{"text", "html"},
			"description": "text (default) converts HTML to readable plain text; html returns the raw HTML.",
		},
		"maxChars": toolutil.IntegerProp(fmt.Sprintf(
			"Maximum characters of output to return (default %d, maximum %d).", DefaultMaxChars, MaxCharsLimit)),
	}, "url")
}

// Invoke 实现 rig.Tool。
func (t *fetchTool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	var args fetchArgs
	if err := toolutil.DecodeArgs(call, &args); err != nil {
		return rig.ToolResult{}, err
	}
	rawURL := strings.TrimSpace(args.URL)
	if rawURL == "" {
		return toolutil.ErrorResultf("web_fetch: url is required"), nil
	}
	format := strings.ToLower(strings.TrimSpace(args.Format))
	if format == "" {
		format = "text"
	}
	if format != "text" && format != "html" {
		return toolutil.ErrorResultf("web_fetch: unsupported format %q (want text or html)", args.Format), nil
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return toolutil.ErrorResultf("web_fetch: invalid URL: %v", err), nil
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return toolutil.ErrorResultf("web_fetch: only http(s) URLs are supported, got scheme %q", parsed.Scheme), nil
	}
	if parsed.Host == "" {
		return toolutil.ErrorResultf("web_fetch: URL has no host"), nil
	}
	if t.policy != nil {
		if err := t.policy.CheckURL(parsed); err != nil {
			return toolutil.ErrorResultf("web_fetch: URL blocked by policy: %v", err), nil
		}
	}
	if err := ctx.Err(); err != nil {
		return rig.ToolResult{}, err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return toolutil.ErrorResultf("web_fetch: build request: %v", err), nil
	}
	request.Header.Set("User-Agent", t.userAgent)
	request.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain;q=0.9,*/*;q=0.5")

	response, err := t.client.Do(request)
	if err != nil {
		return t.requestError(ctx, parsed, err)
	}
	defer response.Body.Close()

	body, truncatedBytes, err := readLimited(response.Body, t.maxBytes)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return rig.ToolResult{}, ctxErr
		}
		return toolutil.ErrorResultf("web_fetch: reading response: %v", err), nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := fmt.Sprintf("web_fetch: %s returned HTTP %d %s",
			parsed.Redacted(), response.StatusCode, http.StatusText(response.StatusCode))
		if snippet := summarizeBody(body, response.Header.Get("Content-Type")); snippet != "" {
			message += "\n" + snippet
		}
		return toolutil.ErrorResultf("%s", message), nil
	}

	contentType := response.Header.Get("Content-Type")
	content := string(body)
	if format == "text" {
		if isHTML(contentType, content) {
			content = HTMLToText(content)
		} else {
			content = strings.ToValidUTF8(content, "\uFFFD")
		}
	}
	content = strings.TrimRight(content, "\n")

	maxChars := t.maxChars
	if args.MaxChars > 0 {
		maxChars = args.MaxChars
	}
	maxChars = min(maxChars, MaxCharsLimit)
	if maxChars <= 0 {
		maxChars = DefaultMaxChars
	}
	content, truncatedChars := truncateRunes(content, maxChars)
	notices := make([]string, 0, 2)
	if truncatedBytes {
		notices = append(notices, fmt.Sprintf("response body exceeded %d bytes", t.maxBytes))
	}
	if truncatedChars {
		notices = append(notices, fmt.Sprintf("showing the first %d characters", maxChars))
	}
	if len(notices) > 0 {
		content += "\n[truncated: " + strings.Join(notices, "; ") + "]"
	}
	return toolutil.TextResult(content), nil
}

// requestError 把 HTTP 客户端错误映射为取消上下文错误（run 本身被中止）或可读的
// 模型可见结果。
func (t *fetchTool) requestError(ctx context.Context, target *url.URL, err error) (rig.ToolResult, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return rig.ToolResult{}, ctxErr
	}
	netError, netOK := errors.AsType[net.Error](err)
	timeout := errors.Is(err, context.DeadlineExceeded) || (netOK && netError.Timeout())
	if timeout {
		return toolutil.ErrorResultf("web_fetch: request to %s timed out after %s", target.Redacted(), t.client.Timeout), nil
	}
	if urlError, ok := errors.AsType[*url.Error](err); ok {
		err = urlError.Err
	}
	return toolutil.ErrorResultf("web_fetch: request to %s failed: %v", target.Redacted(), err), nil
}

// readLimited 最多读取 limit+1 字节，并报告响应体是否超过 limit。
func readLimited(body io.Reader, limit int64) ([]byte, bool, error) {
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > limit {
		return data[:limit], true, nil
	}
	return data, false, nil
}

// summarizeBody 是用于错误消息的简短可读摘录。
func summarizeBody(body []byte, contentType string) string {
	content := string(body)
	if isHTML(contentType, content) {
		content = HTMLToText(content)
	} else {
		content = strings.ToValidUTF8(content, "\uFFFD")
	}
	content = collapseWhitespace(content)
	if len(content) > 300 {
		content = content[:300] + "…"
	}
	return content
}
