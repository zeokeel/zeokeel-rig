package mcp

// OAuth 令牌：授权码换令牌、刷新（单飞 + 轮换安全）、撤销与持久化。
// 令牌记录由本包序列化（版本化 JSON）；宿主 TokenStore 只存不透明字节。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// oauthTokenRecord 是持久化的令牌记录（version 1）。
type oauthTokenRecord struct {
	Version   int               `json:"version"`
	Server    string            `json:"server"`
	Issuer    string            `json:"issuer"`
	Resource  string            `json:"resource"`
	Endpoints oauthEndpoints    `json:"endpoints"`
	Client    oauthClientRecord `json:"client"`
	Token     oauthToken        `json:"token"`
	UpdatedAt time.Time         `json:"updatedAt"`
}

type oauthEndpoints struct {
	Token      string `json:"token"`
	Revocation string `json:"revocation,omitempty"`
}

type oauthToken struct {
	AccessToken  string    `json:"accessToken"`
	TokenType    string    `json:"tokenType"`
	ExpiresAt    time.Time `json:"expiresAt,omitempty"`
	RefreshToken string    `json:"refreshToken,omitempty"`
	Scope        string    `json:"scope,omitempty"`
	ObtainedAt   time.Time `json:"obtainedAt,omitempty"`
}

// oauthTokenResponse 是令牌端点的成功/错误响应（RFC 6749 §5.1/§5.2）。
type oauthTokenResponse struct {
	AccessToken      string `json:"access_token"`
	TokenType        string `json:"token_type"`
	ExpiresIn        int64  `json:"expires_in"`
	RefreshToken     string `json:"refresh_token"`
	Scope            string `json:"scope"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// loadStoredToken 在构造时读取持久化令牌。损坏或与当前配置不一致的记录被忽略
// （不删除），下次授权会覆盖它。
func (c *OAuthClient) loadStoredToken(ctx context.Context) {
	data, found, err := c.cfg.TokenStore.Load(ctx, c.cfg.ServerName)
	if err != nil {
		c.mu.Lock()
		c.lastErr = oauthWrap(OAuthErrStorage, err, "load stored OAuth token")
		c.state = OAuthStateError
		c.mu.Unlock()
		c.logf("oauth %q: load stored token: %v", c.cfg.ServerName, err)
		return
	}
	if !found || len(data) == 0 {
		return
	}
	var record oauthTokenRecord
	if err := json.Unmarshal(data, &record); err != nil {
		c.logf("oauth %q: stored token is not valid JSON (ignored)", c.cfg.ServerName)
		return
	}
	if !c.recordMatchesConfig(&record) {
		c.logf("oauth %q: stored token does not match the current issuer/resource (ignored)", c.cfg.ServerName)
		return
	}
	c.mu.Lock()
	c.token = &record
	c.state = OAuthStateAuthorized
	c.metadata = &oauthMetadata{
		issuer:             record.Issuer,
		tokenEndpoint:      record.Endpoints.Token,
		revocationEndpoint: record.Endpoints.Revocation,
		scopesSupported:    splitScope(record.Token.Scope),
	}
	c.mu.Unlock()
}

// recordMatchesConfig 校验持久化记录与当前配置是否同一 server/resource。
func (c *OAuthClient) recordMatchesConfig(record *oauthTokenRecord) bool {
	if record == nil || record.Version != 1 || record.Token.AccessToken == "" {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(record.Server), c.cfg.ServerName) {
		return false
	}
	resource, err := canonicalResource(record.Resource)
	if err != nil || resource != c.cfg.Resource {
		return false
	}
	return strings.TrimSpace(record.Issuer) != ""
}

// storeRecord 序列化并写穿到宿主 TokenStore。
func (c *OAuthClient) storeRecord(ctx context.Context, record *oauthTokenRecord) *OAuthError {
	if record == nil {
		return oauthErrorf(OAuthErrStorage, "no token record to store")
	}
	record.Version = 1
	record.Server = c.cfg.ServerName
	record.Resource = c.cfg.Resource
	record.UpdatedAt = c.now()
	data, err := json.Marshal(record)
	if err != nil {
		return oauthWrap(OAuthErrStorage, err, "encode OAuth token record")
	}
	if err := c.cfg.TokenStore.Save(ctx, c.cfg.ServerName, data); err != nil {
		return oauthWrap(OAuthErrStorage, err, "save OAuth token record")
	}
	return nil
}

// deleteStoredToken 删除持久化记录（撤销/失效时调用；错误只记录）。
func (c *OAuthClient) deleteStoredToken(ctx context.Context) error {
	if err := c.cfg.TokenStore.Delete(ctx, c.cfg.ServerName); err != nil {
		c.logf("oauth %q: delete stored token: %v", c.cfg.ServerName, err)
		return err
	}
	return nil
}

// exchangeCode 用授权码换令牌并构造持久化记录。
func (c *OAuthClient) exchangeCode(ctx context.Context, flow *oauthFlowState, code string) (*oauthTokenRecord, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {flow.redirectURI},
		"code_verifier": {flow.verifier},
		"resource":      {c.cfg.Resource},
	}
	response, err := c.tokenRequest(ctx, flow.tokenEndpoint, form, flow.client, false)
	if err != nil {
		return nil, err
	}
	obtainedAt := c.now()
	record := &oauthTokenRecord{
		Version:  1,
		Server:   c.cfg.ServerName,
		Issuer:   flow.issuer,
		Resource: c.cfg.Resource,
		Endpoints: oauthEndpoints{
			Token:      flow.tokenEndpoint,
			Revocation: flow.revocationEndpoint,
		},
		Client: flow.client,
		Token: oauthToken{
			AccessToken:  response.AccessToken,
			TokenType:    "Bearer",
			RefreshToken: response.RefreshToken,
			Scope:        firstNonEmptyString(response.Scope, strings.Join(flow.scopes, " ")),
			ObtainedAt:   obtainedAt,
		},
	}
	if response.ExpiresIn > 0 {
		record.Token.ExpiresAt = obtainedAt.Add(time.Duration(response.ExpiresIn) * time.Second)
	}
	return record, nil
}

// tokenRequest POST 到令牌端点；public client 只发 client_id，有 secret 时用
// HTTP Basic。错误信息不回显令牌/code/verifier。
func (c *OAuthClient) tokenRequest(ctx context.Context, endpoint string, form url.Values,
	client oauthClientRecord, refreshing bool) (*oauthTokenResponse, *OAuthError) {
	if err := ensureSecureEndpoint(endpoint); err != nil {
		return nil, oauthWrap(tokenErrorCode(refreshing, false), err, "token endpoint")
	}
	if client.usesSecret() {
		// 凭据通过 Basic 头发送，不放进 form。
	} else {
		form.Set("client_id", client.ID)
	}
	requestCtx, cancel := c.withTimeout(ctx)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, oauthWrap(OAuthErrHTTP, err, "build token request")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	if client.usesSecret() {
		request.SetBasicAuth(client.ID, client.Secret)
	}
	response, err := c.base.Do(request)
	if err != nil {
		return nil, oauthWrap(tokenErrorCode(refreshing, true), err, "token request failed")
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, oauthMaxBodyBytes))
	if readErr != nil {
		return nil, oauthWrap(tokenErrorCode(refreshing, true), readErr, "read token response")
	}
	var decoded oauthTokenResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, oauthWrap(tokenErrorCode(refreshing, true), err,
			"decode token response (status %d)", response.StatusCode)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, tokenResponseError(decoded, response.StatusCode, refreshing)
	}
	if strings.TrimSpace(decoded.AccessToken) == "" {
		return nil, oauthErrorf(tokenErrorCode(refreshing, false), "token response has no access_token")
	}
	if tokenType := strings.TrimSpace(decoded.TokenType); tokenType != "" && !strings.EqualFold(tokenType, "Bearer") {
		return nil, oauthErrorf(tokenErrorCode(refreshing, false),
			"token response token_type %q is not Bearer", decoded.TokenType)
	}
	return &decoded, nil
}

// tokenResponseError 把 RFC 6749 错误码映射成稳定分类。
func tokenResponseError(decoded oauthTokenResponse, status int, refreshing bool) *OAuthError {
	code := strings.TrimSpace(decoded.Error)
	description := safeSnippet([]byte(decoded.ErrorDescription), 200)
	message := fmt.Sprintf("token endpoint returned %q (status %d)", code, status)
	if description != "" {
		message += ": " + description
	}
	switch code {
	case "invalid_grant", "invalid_client":
		return &OAuthError{Code: tokenErrorCode(refreshing, false), Message: message}
	case "access_denied":
		return &OAuthError{Code: OAuthErrAccessDenied, Message: message}
	case "invalid_scope":
		return &OAuthError{Code: OAuthErrInsufficientScope, Message: message}
	default:
		return &OAuthError{Code: tokenErrorCode(refreshing, status >= 500), Message: message}
	}
}

func tokenErrorCode(refreshing, retryable bool) OAuthErrorCode {
	if refreshing {
		return OAuthErrRefresh
	}
	if retryable {
		return OAuthErrHTTP
	}
	return OAuthErrTokenExchange
}

// Refresh 主动刷新一次令牌（用户点击“重试”或宿主定时任务使用）。
func (c *OAuthClient) Refresh(ctx context.Context) (OAuthStatus, error) {
	if c == nil {
		return OAuthStatus{}, oauthErrorf(OAuthErrInvalidConfig, "nil OAuthClient")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	err := c.refreshToken(ctx)
	return c.Status(), err
}

// refreshToken 强刷令牌。单飞：并发 401 共享同一次 refresh 请求与结果。
func (c *OAuthClient) refreshToken(ctx context.Context) error {
	c.mu.Lock()
	if c.refreshing {
		wait := c.refreshWait
		c.mu.Unlock()
		<-wait
		c.mu.Lock()
		err := c.lastRefreshErr
		c.mu.Unlock()
		return err
	}
	c.refreshing = true
	c.refreshWait = make(chan struct{})
	c.mu.Unlock()

	err := c.doRefresh(ctx)

	c.mu.Lock()
	c.refreshing = false
	c.lastRefreshErr = err
	if wait := c.refreshWait; wait != nil {
		close(wait)
	}
	c.refreshWait = nil
	c.mu.Unlock()
	return err
}

// doRefresh 执行一次真实的令牌刷新（由 refreshToken 单飞包装）。
func (c *OAuthClient) doRefresh(ctx context.Context) error {
	c.mu.Lock()
	record := c.token
	c.mu.Unlock()
	if record == nil || strings.TrimSpace(record.Token.RefreshToken) == "" {
		return oauthErrorf(OAuthErrNeedsAuthorization, "no refresh token available")
	}
	endpoint := firstNonEmptyString(record.Endpoints.Token, c.metadataTokenEndpoint())
	if endpoint == "" {
		return oauthErrorf(OAuthErrRefresh, "no token endpoint in the stored authorization")
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {record.Token.RefreshToken},
		"resource":      {c.cfg.Resource},
	}
	if record.Token.Scope != "" {
		form.Set("scope", record.Token.Scope)
	}
	response, err := c.tokenRequest(ctx, endpoint, form, record.Client, true)
	if err != nil {
		if isInvalidGrant(err) {
			c.invalidateToken(ctx, err)
			return err
		}
		// 网络/服务端错误：保留旧令牌，允许下一次请求或用户重试。
		c.mu.Lock()
		c.lastErr = err
		if c.state != OAuthStateExpired {
			c.state = OAuthStateError
		}
		c.mu.Unlock()
		return err
	}
	updated := *record
	updated.Token.AccessToken = response.AccessToken
	updated.Token.TokenType = "Bearer"
	updated.Token.ObtainedAt = c.now()
	updated.Token.ExpiresAt = time.Time{}
	if response.ExpiresIn > 0 {
		updated.Token.ExpiresAt = updated.Token.ObtainedAt.Add(time.Duration(response.ExpiresIn) * time.Second)
	}
	if response.RefreshToken != "" {
		updated.Token.RefreshToken = response.RefreshToken
	}
	if response.Scope != "" {
		updated.Token.Scope = response.Scope
	}
	if updated.Endpoints.Token == "" {
		updated.Endpoints.Token = endpoint
	}
	if storeErr := c.storeRecord(ctx, &updated); storeErr != nil {
		// 刷新令牌可能已经轮换；持久化失败时旧值不可再信任，清空并要求重新授权。
		_ = c.deleteStoredToken(ctx)
		c.mu.Lock()
		c.token = nil
		c.state = OAuthStateExpired
		c.lastErr = storeErr
		c.mu.Unlock()
		return storeErr
	}
	c.mu.Lock()
	c.token = &updated
	c.state = OAuthStateAuthorized
	c.lastErr = nil
	c.mu.Unlock()
	c.logf("oauth %q: refreshed access token", c.cfg.ServerName)
	return nil
}

// isInvalidGrant 判断刷新是否被授权服务器永久拒绝。
func isInvalidGrant(err error) bool {
	var oauthErr *OAuthError
	if !errors.As(err, &oauthErr) {
		return false
	}
	if oauthErr.Code != OAuthErrRefresh {
		return false
	}
	message := strings.ToLower(oauthErr.Message)
	return strings.Contains(message, "invalid_grant") || strings.Contains(message, "invalid_client") ||
		strings.Contains(message, "expired") || strings.Contains(message, "revoked")
}

// invalidateToken 清空本地与持久化令牌并把状态置为 expired。
func (c *OAuthClient) invalidateToken(ctx context.Context, cause *OAuthError) {
	_ = c.deleteStoredToken(ctx)
	c.mu.Lock()
	c.token = nil
	c.state = OAuthStateExpired
	if cause == nil {
		cause = oauthErrorf(OAuthErrRefresh, "authorization is no longer valid")
	}
	c.lastErr = cause
	c.mu.Unlock()
	c.logf("oauth %q: refresh rejected; re-authorization required", c.cfg.ServerName)
}

// Revoke 尽力调用 revocation_endpoint，并删除本地令牌。
func (c *OAuthClient) Revoke(ctx context.Context) error {
	if c == nil {
		return oauthErrorf(OAuthErrInvalidConfig, "nil OAuthClient")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	record := c.token
	metadata := c.metadata
	c.mu.Unlock()

	var networkErr error
	if record != nil {
		endpoint := firstNonEmptyString(record.Endpoints.Revocation, revocationEndpointOf(metadata))
		if endpoint != "" {
			form := url.Values{
				"token":           {firstNonEmptyString(record.Token.RefreshToken, record.Token.AccessToken)},
				"token_type_hint": {"refresh_token"},
			}
			networkErr = c.postRevocation(ctx, endpoint, form, record.Client)
		}
	}
	if err := c.deleteStoredToken(ctx); err != nil && networkErr == nil {
		networkErr = err
	}
	c.mu.Lock()
	c.token = nil
	c.challenge = nil
	c.state = OAuthStateNone
	c.lastErr = nil
	c.mu.Unlock()
	c.logf("oauth %q: revoked local authorization", c.cfg.ServerName)
	if networkErr != nil {
		return oauthWrap(OAuthErrRevoked, networkErr, "revocation endpoint failed")
	}
	return nil
}

// postRevocation 尽力通知 AS 撤销令牌。
func (c *OAuthClient) postRevocation(ctx context.Context, endpoint string, form url.Values, client oauthClientRecord) error {
	if err := ensureSecureEndpoint(endpoint); err != nil {
		return err
	}
	if !client.usesSecret() {
		form.Set("client_id", client.ID)
	}
	requestCtx, cancel := c.withTimeout(ctx)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if client.usesSecret() {
		request.SetBasicAuth(client.ID, client.Secret)
	}
	response, err := c.base.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1024))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("revocation endpoint returned status %d", response.StatusCode)
	}
	return nil
}

// ensureAccessToken 返回应当注入请求的 access token；临近过期时先刷新。
func (c *OAuthClient) ensureAccessToken(ctx context.Context) string {
	c.mu.Lock()
	record := c.token
	c.mu.Unlock()
	if record == nil {
		return ""
	}
	if !tokenNearExpiry(record.Token.ExpiresAt, c.now()) || record.Token.RefreshToken == "" {
		return record.Token.AccessToken
	}
	if err := c.refreshToken(ctx); err != nil {
		c.mu.Lock()
		current := c.token
		c.mu.Unlock()
		if current != nil {
			return current.Token.AccessToken
		}
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != nil {
		return c.token.Token.AccessToken
	}
	return ""
}

// currentAccessToken 返回内存中的令牌（不触发刷新）。
func (c *OAuthClient) currentAccessToken() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == nil {
		return ""
	}
	return c.token.Token.AccessToken
}

// hasRefreshToken 报告是否存在可刷新的 refresh token。
func (c *OAuthClient) hasRefreshToken() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token != nil && strings.TrimSpace(c.token.Token.RefreshToken) != ""
}

// markReauthorizationRequired 把“令牌被拒且无法刷新”的状态置为需要重新授权。
func (c *OAuthClient) markReauthorizationRequired(message string) {
	c.mu.Lock()
	c.state = OAuthStateExpired
	c.lastErr = oauthErrorf(OAuthErrRefresh, "%s", message)
	c.mu.Unlock()
}

// captureChallenge 记录 401 的 challenge，供 Prepare 复用并驱动 UI 状态。
func (c *OAuthClient) captureChallenge(headers []string) {
	challenge := parseWWWAuthenticate(headers)
	if challenge == nil {
		return
	}
	c.mu.Lock()
	c.challenge = challenge
	if c.token == nil && c.state != OAuthStateAuthorizing {
		c.state = OAuthStateRequired
	}
	c.mu.Unlock()
}

// captureInsufficientScope 记录 403 的 scope 挑战（step-up 留待后续版本）。
func (c *OAuthClient) captureInsufficientScope(headers []string) {
	challenge := parseWWWAuthenticate(headers)
	if challenge == nil {
		return
	}
	c.mu.Lock()
	c.challenge = challenge
	if challenge.Error != "" {
		c.lastErr = oauthErrorf(OAuthErrInsufficientScope,
			"server requires additional scope: %s", strings.TrimSpace(challenge.Scope))
	}
	c.mu.Unlock()
}

// metadataTokenEndpoint 返回已发现 metadata 的 token endpoint（记录里缺失时兜底）。
func (c *OAuthClient) metadataTokenEndpoint() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.metadata == nil {
		return ""
	}
	return c.metadata.tokenEndpoint
}

func revocationEndpointOf(metadata *oauthMetadata) string {
	if metadata == nil {
		return ""
	}
	return metadata.revocationEndpoint
}

// tokenNearExpiry 报告令牌是否已到期或进入提前刷新窗口（零值表示未知过期时间）。
func tokenNearExpiry(expiresAt, now time.Time) bool {
	if expiresAt.IsZero() {
		return false
	}
	return !now.Before(expiresAt.Add(-oauthRefreshWindow))
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
