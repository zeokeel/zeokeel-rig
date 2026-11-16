package mcp

// OAuth 回环回调：只监听 127.0.0.1 的随机（或配置）端口，接收授权码并交给
// Wait；响应页只用于告诉用户“可以关闭浏览器”。

import (
	"context"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// oauthLoopback 是一次授权流程的本地回调监听。
type oauthLoopback struct {
	redirectURI string
	path        string
	listener    net.Listener
	server      *http.Server
	result      chan oauthCallbackResult
	closeOnce   sync.Once
}

// oauthCallbackResult 是回调解析结果（不包含令牌，只有 code/state/iss）。
type oauthCallbackResult struct {
	code  string
	state string
	iss   string
	err   *OAuthError
	// noCallback 标记本地等待失败（超时/取消），不是授权服务器发回的回调错误。
	noCallback bool
}

// startLoopback 监听回环地址。redirectURI 为空时使用 127.0.0.1:0；非空时必须是
// 127.0.0.1 的 http 地址（端口必须固定）。
func startLoopback(redirectURI string) (*oauthLoopback, error) {
	var (
		listener net.Listener
		err      error
		path     = "/callback"
	)
	if strings.TrimSpace(redirectURI) == "" {
		listener, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		redirectURI = "http://" + listener.Addr().String() + path
	} else {
		parsed, parseErr := parseLoopbackRedirect(redirectURI)
		if parseErr != nil {
			return nil, parseErr
		}
		listener, err = net.Listen("tcp", net.JoinHostPort(parsed.Hostname(), parsed.Port()))
		if err != nil {
			return nil, err
		}
		path = parsed.Path
		if path == "" {
			path = "/"
		}
	}
	loop := &oauthLoopback{
		redirectURI: redirectURI,
		path:        path,
		listener:    listener,
		result:      make(chan oauthCallbackResult, 1),
	}
	mux := http.NewServeMux()
	mux.HandleFunc(path, loop.handleCallback)
	loop.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Second,
	}
	go func() {
		_ = loop.server.Serve(listener)
	}()
	return loop, nil
}

// handleCallback 解析 code/state/iss/error。
func (l *oauthLoopback) handleCallback(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writer.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	query := request.URL.Query()
	result := oauthCallbackResult{
		code:  query.Get("code"),
		state: query.Get("state"),
		iss:   query.Get("iss"),
	}
	switch {
	case query.Get("error") != "":
		description := safeSnippet([]byte(query.Get("error_description")), 200)
		result.err = oauthErrorf(OAuthErrAccessDenied, "authorization server returned %q: %s",
			query.Get("error"), description)
	case result.code == "":
		result.err = oauthErrorf(OAuthErrTokenExchange, "authorization callback has no code")
	}
	select {
	case l.result <- result:
	default:
		// 只接受第一个回调；重复回调（浏览器重试）不阻塞 handler。
	}
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	status := http.StatusOK
	title := "Authorization complete"
	message := "You can close this window and return to ZeoKeel."
	if result.err != nil {
		status = http.StatusBadRequest
		title = "Authorization failed"
		message = "Return to ZeoKeel and try again. " + html.EscapeString(result.err.Message)
	}
	writer.WriteHeader(status)
	_, _ = writer.Write([]byte(callbackPage(title, message)))
}

// callbackPage 返回一个最小、无脚本的回调结果页。
func callbackPage(title, message string) string {
	return `<!doctype html><html lang="en"><meta charset="utf-8">` +
		`<title>` + html.EscapeString(title) + `</title>` +
		`<body style="font-family:system-ui,sans-serif;max-width:32rem;margin:12vh auto;padding:0 1rem;color:#111">` +
		`<h1 style="font-size:1.25rem">` + html.EscapeString(title) + `</h1>` +
		`<p style="color:#555;line-height:1.5">` + html.EscapeString(message) + `</p>` +
		`</body></html>`
}

// wait 等待回调、取消或超时。
func (l *oauthLoopback) wait(ctx context.Context, timeout time.Duration) oauthCallbackResult {
	if timeout <= 0 {
		return oauthCallbackResult{err: oauthErrorf(OAuthErrCallbackTimeout,
			"the authorization flow has expired; start a new authorization")}
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case result := <-l.result:
		return result
	case <-ctx.Done():
		return oauthCallbackResult{
			err:        oauthWrap(OAuthErrCallbackTimeout, ctx.Err(), "authorization callback cancelled"),
			noCallback: true,
		}
	case <-timer.C:
		return oauthCallbackResult{
			err:        oauthErrorf(OAuthErrCallbackTimeout, "timed out waiting for the authorization callback"),
			noCallback: true,
		}
	}
}

// cancel 通知等待者流程已被取消，然后关闭监听（幂等）。
func (l *oauthLoopback) cancel() {
	if l == nil {
		return
	}
	select {
	case l.result <- oauthCallbackResult{
		err:        oauthErrorf(OAuthErrCallbackTimeout, "authorization cancelled"),
		noCallback: true,
	}:
	default:
	}
	_ = l.close()
}

// close 关闭监听与服务（幂等）。
func (l *oauthLoopback) close() error {
	if l == nil {
		return nil
	}
	l.closeOnce.Do(func() {
		if l.server != nil {
			_ = l.server.Close()
		}
		if l.listener != nil {
			_ = l.listener.Close()
		}
	})
	return nil
}

// parseLoopbackRedirect 校验并解析固定的回环 redirect URI：
// 只允许 http://127.0.0.1:<port><path>。
func parseLoopbackRedirect(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, err
	}
	if strings.ToLower(parsed.Scheme) != "http" {
		return nil, fmt.Errorf("loopback redirect URI %q must use http", raw)
	}
	if parsed.Hostname() != "127.0.0.1" {
		return nil, fmt.Errorf("loopback redirect URI %q must use 127.0.0.1", raw)
	}
	if parsed.Port() == "" {
		return nil, fmt.Errorf("loopback redirect URI %q must include an explicit port", raw)
	}
	if parsed.Fragment != "" || parsed.RawQuery != "" {
		return nil, fmt.Errorf("loopback redirect URI %q must not contain a query or fragment", raw)
	}
	return parsed, nil
}
