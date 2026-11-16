package mcp

// OAuth 传输层：给发往 MCP server 的每个请求注入 Bearer，并在 401 时单飞刷新
// 令牌后重试一次。元数据/令牌请求使用独立 client（c.base），不会递归经过这里。

import (
	"net/http"
	"strings"
)

// oauthTransport 是注入到 mcp.HTTPConfig.HTTPClient 的 RoundTripper。
type oauthTransport struct {
	client *OAuthClient
	base   http.RoundTripper
}

// RoundTrip 实现 http.RoundTripper。
func (t *oauthTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if t == nil || t.client == nil || request == nil {
		return nil, oauthErrorf(OAuthErrInvalidConfig, "oauth transport is not configured")
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	token := t.client.ensureAccessToken(request.Context())
	response, err := t.roundTripWithToken(base, request, token, false)
	if err != nil {
		return nil, err
	}
	switch response.StatusCode {
	case http.StatusUnauthorized:
		t.client.captureChallenge(response.Header.Values("WWW-Authenticate"))
		if token == "" {
			return response, nil
		}
		if !t.client.hasRefreshToken() {
			t.client.markReauthorizationRequired(
				"the server rejected the access token and no refresh token is available")
			return response, nil
		}
		refreshErr := t.client.refreshToken(request.Context())
		if !canReplayRequest(request) {
			// 请求体不可重放（没有 GetBody）：刷新仍然发生，但只能把原始 401
			// 交给上层；下一次请求会自动带上新令牌。
			_ = refreshErr
			return response, nil
		}
		if refreshErr != nil {
			return response, nil
		}
		_ = response.Body.Close()
		return t.roundTripWithToken(base, request, t.client.currentAccessToken(), true)
	case http.StatusForbidden:
		t.client.captureInsufficientScope(response.Header.Values("WWW-Authenticate"))
	}
	return response, nil
}

// roundTripWithToken 克隆请求并注入 Authorization；replay=true 时用 GetBody
// 重建请求体（首次尝试直接复用原请求体）。
func (t *oauthTransport) roundTripWithToken(base http.RoundTripper, request *http.Request,
	token string, replay bool) (*http.Response, error) {
	outgoing := request.Clone(request.Context())
	if token != "" {
		outgoing.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	}
	if replay && request.GetBody != nil {
		body, err := request.GetBody()
		if err != nil {
			return nil, err
		}
		outgoing.Body = body
		outgoing.GetBody = request.GetBody
	}
	return base.RoundTrip(outgoing)
}

// canReplayRequest 报告 401 重试能否重建请求体。
func canReplayRequest(request *http.Request) bool {
	if request == nil {
		return false
	}
	return request.Body == nil || request.GetBody != nil
}
