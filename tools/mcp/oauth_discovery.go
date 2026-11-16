package mcp

// OAuth 发现：从 401 的 WWW-Authenticate 或 RFC 9728 well-known 取得
// Protected Resource Metadata，再按 RFC 8414 / OIDC Discovery 取得 Authorization
// Server metadata，并做 resource/issuer/PKCE 的严格校验。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// protectedResourceMetadata 是 RFC 9728 文档中本客户端使用的字段。
type protectedResourceMetadata struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
	ScopesSupported      []string `json:"scopes_supported"`
}

// authorizationServerMetadata 是 RFC 8414 / OIDC Discovery 文档中本客户端
// 使用的字段。
type authorizationServerMetadata struct {
	Issuer                                     string   `json:"issuer"`
	AuthorizationEndpoint                      string   `json:"authorization_endpoint"`
	TokenEndpoint                              string   `json:"token_endpoint"`
	RegistrationEndpoint                       string   `json:"registration_endpoint"`
	RevocationEndpoint                         string   `json:"revocation_endpoint"`
	ScopesSupported                            []string `json:"scopes_supported"`
	CodeChallengeMethodsSupported              []string `json:"code_challenge_methods_supported"`
	AuthorizationResponseISSParameterSupported bool     `json:"authorization_response_iss_parameter_supported"`
}

// oauthHTTPStatusError 是发现请求的非 2xx 结果（用于按 URL 顺序回退）。
type oauthHTTPStatusError struct {
	status int
	url    string
	body   string
}

func (e *oauthHTTPStatusError) Error() string {
	message := fmt.Sprintf("mcp oauth: GET %s: status %d", e.url, e.status)
	if e.body != "" {
		message += ": " + e.body
	}
	return message
}

// discover 返回 AS metadata 与 PRM。401 challenge 的 resource_metadata 优先，
// 否则按规范回退 well-known（先路径、再根），AS metadata 再按 3 个 well-known
// 顺序尝试。
func (c *OAuthClient) discover(ctx context.Context) (*oauthMetadata, *protectedResourceMetadata, error) {
	resourceURL, err := url.Parse(c.cfg.Resource)
	if err != nil {
		return nil, nil, oauthWrap(OAuthErrInvalidConfig, err, "parse resource URL")
	}
	c.mu.Lock()
	challenge := c.challenge
	c.mu.Unlock()

	var prm *protectedResourceMetadata
	var prmErr error
	if challenge != nil && strings.TrimSpace(challenge.ResourceMetadata) != "" {
		prm, prmErr = c.fetchProtectedResourceMetadata(ctx, challenge.ResourceMetadata)
	}
	if prm == nil {
		wellKnown, wellKnownErr := c.discoverProtectedResourceMetadataWellKnown(ctx, resourceURL)
		if wellKnownErr == nil {
			prm = wellKnown
		} else if prmErr == nil {
			prmErr = wellKnownErr
		}
	}
	if prm == nil {
		return nil, nil, asOAuthError(prmErr)
	}
	if err := c.validateProtectedResourceMetadata(prm); err != nil {
		return nil, nil, err
	}

	asID := strings.TrimSpace(c.cfg.AuthorizationServer)
	if asID == "" {
		asID = strings.TrimSpace(prm.AuthorizationServers[0])
	}
	metadata, err := c.fetchAuthorizationServerMetadata(ctx, asID)
	if err != nil {
		return nil, nil, asOAuthError(err)
	}
	return metadata, prm, nil
}

// fetchProtectedResourceMetadata 读取一个明确的 PRM URL。
func (c *OAuthClient) fetchProtectedResourceMetadata(ctx context.Context, endpoint string) (*protectedResourceMetadata, error) {
	if err := ensureSecureEndpoint(endpoint); err != nil {
		return nil, oauthWrap(OAuthErrDiscovery, err, "protected resource metadata URL")
	}
	var document protectedResourceMetadata
	if err := c.getJSON(ctx, endpoint, &document); err != nil {
		return nil, oauthWrap(OAuthErrDiscovery, err, "read protected resource metadata")
	}
	return &document, nil
}

// discoverProtectedResourceMetadataWellKnown 按 RFC 9728 顺序尝试：
// 先 资源路径插入，再根路径。
func (c *OAuthClient) discoverProtectedResourceMetadataWellKnown(ctx context.Context, resource *url.URL) (*protectedResourceMetadata, error) {
	var lastErr error
	for _, endpoint := range protectedResourceMetadataURLs(resource) {
		var document protectedResourceMetadata
		if err := c.getJSON(ctx, endpoint, &document); err != nil {
			lastErr = err
			continue
		}
		return &document, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no protected resource metadata endpoint")
	}
	return nil, oauthWrap(OAuthErrDiscovery, lastErr, "discover protected resource metadata")
}

// protectedResourceMetadataURLs 返回 RFC 9728 的候选地址（去重、保持顺序）。
func protectedResourceMetadataURLs(resource *url.URL) []string {
	base := &url.URL{Scheme: resource.Scheme, Host: resource.Host}
	var out []string
	path := strings.TrimSuffix(resource.Path, "/")
	if path != "" && path != "/" {
		inserted := *base
		inserted.Path = "/.well-known/oauth-protected-resource" + path
		out = append(out, inserted.String())
	}
	root := *base
	root.Path = "/.well-known/oauth-protected-resource"
	out = append(out, root.String())
	return dedupeStrings(out)
}

// validateProtectedResourceMetadata 校验 resource 与 authorization_servers。
func (c *OAuthClient) validateProtectedResourceMetadata(document *protectedResourceMetadata) *OAuthError {
	if document == nil {
		return oauthErrorf(OAuthErrDiscovery, "protected resource metadata is empty")
	}
	resource, err := canonicalResource(document.Resource)
	if err != nil {
		return oauthWrap(OAuthErrResourceMismatch, err, "protected resource metadata resource %q", document.Resource)
	}
	if resource != c.cfg.Resource {
		return oauthErrorf(OAuthErrResourceMismatch,
			"protected resource metadata resource %q does not match %q", resource, c.cfg.Resource)
	}
	if len(document.AuthorizationServers) == 0 {
		return oauthErrorf(OAuthErrDiscovery, "protected resource metadata has no authorization_servers")
	}
	return nil
}

// fetchAuthorizationServerMetadata 按 RFC 8414/OIDC 的路径规则尝试 AS metadata。
func (c *OAuthClient) fetchAuthorizationServerMetadata(ctx context.Context, issuer string) (*oauthMetadata, error) {
	issuer = strings.TrimSpace(issuer)
	if issuer == "" {
		return nil, oauthErrorf(OAuthErrDiscovery, "no authorization server identified")
	}
	if err := ensureSecureEndpoint(issuer); err != nil {
		return nil, oauthWrap(OAuthErrDiscovery, err, "authorization server %q", issuer)
	}
	var lastErr error
	for _, endpoint := range authorizationServerMetadataURLs(issuer) {
		var document authorizationServerMetadata
		if err := c.getJSON(ctx, endpoint, &document); err != nil {
			lastErr = err
			continue
		}
		// RFC 8414 §3.3 / OIDC Discovery §4.3：issuer 必须与用于构造
		// well-known URL 的标识完全一致。
		if strings.TrimSpace(document.Issuer) != issuer {
			lastErr = oauthErrorf(OAuthErrIssuerMismatch,
				"authorization server metadata issuer %q does not match %q", document.Issuer, issuer)
			continue
		}
		metadata, err := c.normalizeAuthorizationServerMetadata(&document)
		if err != nil {
			return nil, err
		}
		return metadata, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no authorization server metadata endpoint")
	}
	var oauthErr *OAuthError
	if errors.As(lastErr, &oauthErr) && oauthErr.Code == OAuthErrIssuerMismatch {
		return nil, oauthErr
	}
	return nil, oauthWrap(OAuthErrASMetadata, lastErr, "discover authorization server metadata")
}

// normalizeAuthorizationServerMetadata 校验端点、PKCE 与 iss 能力。
func (c *OAuthClient) normalizeAuthorizationServerMetadata(document *authorizationServerMetadata) (*oauthMetadata, *OAuthError) {
	for _, endpoint := range []string{document.AuthorizationEndpoint, document.TokenEndpoint} {
		if strings.TrimSpace(endpoint) == "" {
			return nil, oauthErrorf(OAuthErrASMetadata, "authorization server metadata is missing an endpoint")
		}
		if err := ensureSecureEndpoint(endpoint); err != nil {
			return nil, oauthWrap(OAuthErrASMetadata, err, "authorization server endpoint")
		}
	}
	for _, endpoint := range []string{document.RegistrationEndpoint, document.RevocationEndpoint} {
		if strings.TrimSpace(endpoint) == "" {
			continue
		}
		if err := ensureSecureEndpoint(endpoint); err != nil {
			return nil, oauthWrap(OAuthErrASMetadata, err, "authorization server endpoint")
		}
	}
	// 安全要求：AS 必须声明 S256；缺失即拒绝，不做不安全降级。
	supportsS256 := false
	for _, method := range document.CodeChallengeMethodsSupported {
		if strings.TrimSpace(method) == "S256" {
			supportsS256 = true
			break
		}
	}
	if !supportsS256 {
		return nil, oauthErrorf(OAuthErrPKCEUnsupported,
			"authorization server does not advertise code_challenge_methods_supported with S256")
	}
	return &oauthMetadata{
		issuer:                document.Issuer,
		authorizationEndpoint: document.AuthorizationEndpoint,
		tokenEndpoint:         document.TokenEndpoint,
		registrationEndpoint:  document.RegistrationEndpoint,
		revocationEndpoint:    document.RevocationEndpoint,
		scopesSupported:       normalizedScopes(document.ScopesSupported),
		codeChallengeMethods:  append([]string(nil), document.CodeChallengeMethodsSupported...),
		issParameterSupported: document.AuthorizationResponseISSParameterSupported,
	}, nil
}

// authorizationServerMetadataURLs 返回 RFC 8414 §3.1 与 OIDC Discovery 的候选
// 顺序：OAuth 路径插入 → OIDC 路径插入 → OIDC 路径追加。
func authorizationServerMetadataURLs(issuer string) []string {
	parsed, err := url.Parse(strings.TrimSpace(issuer))
	if err != nil || parsed.Host == "" {
		return nil
	}
	base := &url.URL{Scheme: parsed.Scheme, Host: parsed.Host}
	path := strings.TrimSuffix(parsed.Path, "/")
	var out []string
	appendPath := func(pattern string) {
		target := *base
		target.Path = pattern + path
		out = append(out, target.String())
	}
	appendPath("/.well-known/oauth-authorization-server")
	appendPath("/.well-known/openid-configuration")
	if path != "" {
		target := *base
		target.Path = path + "/.well-known/openid-configuration"
		out = append(out, target.String())
	}
	return dedupeStrings(out)
}

// getJSON GET 一个 JSON 文档（有大小与超时上限）。
func (c *OAuthClient) getJSON(ctx context.Context, endpoint string, out any) error {
	requestCtx, cancel := c.withTimeout(ctx)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	response, err := c.base.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := limitedBody(response.Body)
		return &oauthHTTPStatusError{status: response.StatusCode, url: endpoint, body: safeSnippet(body, 200)}
	}
	body, err := limitedBody(response.Body)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode %s: %w", endpoint, err)
	}
	return nil
}

// ensureSecureEndpoint 要求远程端点为 https；回环地址允许 http（本地测试与
// 本机授权服务器）。
func ensureSecureEndpoint(endpoint string) error {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return err
	}
	if parsed.Host == "" {
		return fmt.Errorf("endpoint %q has no host", endpoint)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https":
		return nil
	case "http":
		if isLoopbackHost(parsed.Hostname()) {
			return nil
		}
		return fmt.Errorf("endpoint %q must use https", endpoint)
	default:
		return fmt.Errorf("endpoint %q must use http(s)", endpoint)
	}
}

// canonicalResource 返回 RFC 8707 资源标识的规范形式（小写 scheme/host、
// 去掉根路径尾斜杠、拒绝 fragment / 非 http(s) / 非回环 http）。
func canonicalResource(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	if parsed.Fragment != "" {
		return "", errors.New("resource must not contain a fragment")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("resource %q must use http(s)", raw)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("resource %q has no host", raw)
	}
	if scheme == "http" && !isLoopbackHost(parsed.Hostname()) {
		return "", fmt.Errorf("resource %q must use https unless it is a loopback address", raw)
	}
	parsed.Scheme = scheme
	parsed.Host = strings.ToLower(parsed.Host)
	if parsed.Path == "/" {
		parsed.Path = ""
	}
	return parsed.String(), nil
}

// isLoopbackHost 报告 hostname 是否是本机回环地址。
func isLoopbackHost(hostname string) bool {
	switch strings.ToLower(strings.Trim(hostname, "[]")) {
	case "127.0.0.1", "::1", "localhost":
		return true
	default:
		return false
	}
}

// parseWWWAuthenticate 解析 401/403 响应中的 Bearer challenge，返回第一个带
// resource_metadata / scope / error 的 challenge。
func parseWWWAuthenticate(values []string) *oauthChallenge {
	for _, value := range values {
		challenge := parseBearerChallenge(value)
		if challenge == nil {
			continue
		}
		if challenge.ResourceMetadata != "" || challenge.Scope != "" || challenge.Error != "" {
			return challenge
		}
	}
	return nil
}

// parseBearerChallenge 从一条 header 里提取 Bearer 方案参数。
func parseBearerChallenge(header string) *oauthChallenge {
	lower := strings.ToLower(header)
	index := strings.Index(lower, "bearer")
	if index < 0 {
		return nil
	}
	rest := header[index+len("bearer"):]
	params := parseAuthParams(rest)
	if len(params) == 0 {
		return nil
	}
	return &oauthChallenge{
		ResourceMetadata: params["resource_metadata"],
		Scope:            params["scope"],
		Error:            params["error"],
		ErrorDescription: params["error_description"],
	}
}

// parseAuthParams 解析逗号分隔的 key=value（值可为 quoted-string）。
func parseAuthParams(raw string) map[string]string {
	params := make(map[string]string)
	for _, part := range splitAuthParams(raw) {
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		if key == "" {
			continue
		}
		params[key] = unquoteAuthValue(strings.TrimSpace(value))
	}
	return params
}

// splitAuthParams 按引号外的逗号切分（quoted-string 内的逗号保留）。
func splitAuthParams(raw string) []string {
	var out []string
	var builder strings.Builder
	inQuotes := false
	escaped := false
	for _, r := range raw {
		switch {
		case escaped:
			builder.WriteRune(r)
			escaped = false
		case r == '\\' && inQuotes:
			builder.WriteRune(r)
			escaped = true
		case r == '"':
			inQuotes = !inQuotes
			builder.WriteRune(r)
		case r == ',' && !inQuotes:
			out = append(out, builder.String())
			builder.Reset()
		default:
			builder.WriteRune(r)
		}
	}
	if builder.Len() > 0 {
		out = append(out, builder.String())
	}
	return out
}

// unquoteAuthValue 去掉 quoted-string 的引号并处理转义。
func unquoteAuthValue(value string) string {
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		if unquoted, err := strconv.Unquote(value); err == nil {
			return unquoted
		}
		return strings.Trim(value, `"`)
	}
	return value
}

// validateAuthorizationISS 按 RFC 9207 校验回调里的 iss 参数。
func validateAuthorizationISS(metadata *oauthMetadata, iss string) *OAuthError {
	if strings.TrimSpace(iss) == "" {
		if metadata != nil && metadata.issParameterSupported {
			return oauthErrorf(OAuthErrISSMismatch,
				"authorization response is missing iss but the server advertises authorization_response_iss_parameter_supported")
		}
		return nil
	}
	if metadata == nil || iss != metadata.issuer {
		return oauthErrorf(OAuthErrISSMismatch,
			"authorization response iss %q does not match %q", iss, issuerOf(metadata))
	}
	return nil
}

func issuerOf(metadata *oauthMetadata) string {
	if metadata == nil {
		return ""
	}
	return metadata.issuer
}

// safeSnippet 把响应体截断成可放进错误信息的片段。
func safeSnippet(body []byte, limit int) string {
	text := strings.TrimSpace(string(body))
	if limit > 0 {
		runes := []rune(text)
		if len(runes) > limit {
			text = string(runes[:limit]) + "…"
		}
	}
	return text
}

// dedupeStrings 去重并保持顺序。
func dedupeStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}
