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

// fetchArgs is the web_fetch argument object.
type fetchArgs struct {
	URL      string `json:"url"`
	Format   string `json:"format"`
	MaxChars int    `json:"maxChars"`
}

// fetchConfig is the resolved configuration of one fetcher.
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

// FetchOption customizes NewHTTPFetcher.
type FetchOption func(*fetchConfig)

// WithPolicy bounds the URLs the fetcher may request (initial URL and every
// redirect hop). A nil policy allows every URL.
func WithPolicy(policy Policy) FetchOption {
	return func(config *fetchConfig) { config.policy = policy }
}

// WithTimeout sets the end-to-end request timeout (zero or negative keeps the
// default).
func WithTimeout(timeout time.Duration) FetchOption {
	return func(config *fetchConfig) {
		if timeout > 0 {
			config.timeout = timeout
		}
	}
}

// WithMaxBytes sets the response body cap (zero or negative keeps the default).
func WithMaxBytes(maxBytes int64) FetchOption {
	return func(config *fetchConfig) {
		if maxBytes > 0 {
			config.maxBytes = maxBytes
		}
	}
}

// WithMaxChars sets the default output character cap.
func WithMaxChars(maxChars int) FetchOption {
	return func(config *fetchConfig) {
		if maxChars > 0 {
			config.maxChars = maxChars
		}
	}
}

// WithUserAgent overrides the User-Agent header (empty keeps the default).
func WithUserAgent(userAgent string) FetchOption {
	return func(config *fetchConfig) {
		if strings.TrimSpace(userAgent) != "" {
			config.userAgent = userAgent
		}
	}
}

// WithTransport overrides the HTTP transport (useful for tests and proxies).
// The client still owns the timeout and the redirect policy.
func WithTransport(transport http.RoundTripper) FetchOption {
	return func(config *fetchConfig) { config.transport = transport }
}

// WithAllowPrivateHosts turns off the built-in private-address guard that
// protects policies without HostResolver. It exists for hosts that deliberately
// fetch loopback/private targets (local mirrors, integration tests); leave it
// off for untrusted model traffic. Policies implementing HostResolver keep
// their own validation either way, and a nil policy never installs the guard.
func WithAllowPrivateHosts() FetchOption {
	return func(config *fetchConfig) { config.allowPrivate = true }
}

// fetchTool is the rig.Tool implementation of web_fetch.
type fetchTool struct {
	client    *http.Client
	policy    Policy
	maxBytes  int64
	maxChars  int
	userAgent string
}

// NewHTTPFetcher builds web_fetch with options. The HTTP client enforces the
// total timeout, the redirect cap and the policy on every hop.
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

// newPolicyTransport builds the default transport, which always resolves the
// dialed host itself so the checked address cannot change between validation and
// dial (DNS rebinding):
//
//   - a policy implementing HostResolver supplies the validated IPs (for
//     example sandbox.Policy);
//   - any other non-nil policy is still protected: the fetcher resolves the
//     name and rejects non-public addresses itself, so web.PolicyFunc and
//     custom CheckURL policies cannot be tricked into dialing loopback,
//     private, link-local or cloud-metadata targets;
//   - WithAllowPrivateHosts disables that fallback guard for deliberate private
//     targets;
//   - a nil policy installs no guard at all (the documented opt-out for "allow
//     every URL") and keeps the standard dialer.
//
// A custom transport passed with WithTransport is used as-is and is responsible
// for its own address validation. The resolved-and-pinned dialer also covers an
// HTTP proxy configured through the environment, so a loopback proxy is refused
// unless private hosts were explicitly allowed.
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

// resolvePolicyHosts returns the IP addresses a request for host may dial.
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

// isPublicIP reports whether ip is a publicly routable unicast address. It
// rejects the address classes an SSRF guard must never dial: loopback, private,
// link-local, multicast, unspecified, CGNAT and reserved ranges.
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
		case v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127: // 100.64.0.0/10 CGNAT
			return false
		case v4[0] == 192 && v4[1] == 0 && v4[2] == 0: // 192.0.0.0/24 IETF
			return false
		case v4[0] == 198 && (v4[1] == 18 || v4[1] == 19): // 198.18.0.0/15 benchmark
			return false
		case v4[0] >= 240: // 240.0.0.0/4 reserved
			return false
		}
	}
	return true
}

// Name implements rig.Tool.
func (t *fetchTool) Name() string { return "web_fetch" }

// Description implements rig.Tool.
func (t *fetchTool) Description() string {
	return "Fetch a http(s) URL and return its content as text (HTML is converted to plain text) or, " +
		"with format=html, as raw HTML. Redirects are followed up to 5 hops, the request has a total timeout, " +
		"and both the response body and the output are capped. The URL must pass the host's policy."
}

// Schema implements rig.Tool.
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

// Invoke implements rig.Tool.
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
	if maxChars > MaxCharsLimit {
		maxChars = MaxCharsLimit
	}
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

// requestError maps an HTTP client error to either a cancelled-context error
// (the run itself was aborted) or a readable model-facing result.
func (t *fetchTool) requestError(ctx context.Context, target *url.URL, err error) (rig.ToolResult, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return rig.ToolResult{}, ctxErr
	}
	var netError net.Error
	timeout := errors.Is(err, context.DeadlineExceeded) ||
		(errors.As(err, &netError) && netError.Timeout())
	if timeout {
		return toolutil.ErrorResultf("web_fetch: request to %s timed out after %s", target.Redacted(), t.client.Timeout), nil
	}
	var urlError *url.Error
	if errors.As(err, &urlError) {
		err = urlError.Err
	}
	return toolutil.ErrorResultf("web_fetch: request to %s failed: %v", target.Redacted(), err), nil
}

// readLimited reads at most limit+1 bytes and reports whether the body was
// larger than limit.
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

// summarizeBody is a short, readable excerpt for error messages.
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
