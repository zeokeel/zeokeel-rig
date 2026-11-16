package mcp

// OAuth 客户端注册：优先使用预注册 ClientID；否则在 AS 提供
// registration_endpoint 时走 RFC 7591 动态注册（OIDC 场景下声明
// application_type=native，以允许 127.0.0.1 回环 redirect_uri）。

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// oauthClientRecord 是一次注册得到的客户端身份（持久化在令牌记录里）。
type oauthClientRecord struct {
	ID     string `json:"id"`
	Secret string `json:"secret,omitempty"`
	Source string `json:"source,omitempty"`
}

// dynamicClientRegistrationRequest 是 RFC 7591 注册请求（本客户端用到的字段）。
type dynamicClientRegistrationRequest struct {
	ClientName              string   `json:"client_name"`
	ApplicationType         string   `json:"application_type,omitempty"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	Scope                   string   `json:"scope,omitempty"`
}

// dynamicClientRegistrationResponse 是 RFC 7591 注册响应。
type dynamicClientRegistrationResponse struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

// registerClient 返回本次授权使用的 client 身份。
func (c *OAuthClient) registerClient(ctx context.Context, metadata *oauthMetadata,
	redirectURI string, scopes []string) (oauthClientRecord, error) {
	if clientID := strings.TrimSpace(c.cfg.ClientID); clientID != "" {
		return oauthClientRecord{
			ID:     clientID,
			Secret: strings.TrimSpace(c.cfg.ClientSecret),
			Source: "preregistered",
		}, nil
	}
	if metadata == nil || strings.TrimSpace(metadata.registrationEndpoint) == "" {
		return oauthClientRecord{}, oauthErrorf(OAuthErrRegistration,
			"authorization server does not support dynamic client registration; configure a pre-registered client ID")
	}
	payload := dynamicClientRegistrationRequest{
		ClientName:              "ZeoKeel MCP Client",
		ApplicationType:         "native",
		RedirectURIs:            []string{redirectURI},
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		ResponseTypes:           []string{"code"},
		TokenEndpointAuthMethod: "none",
		Scope:                   strings.Join(scopes, " "),
	}
	response, err := c.postRegistration(ctx, metadata.registrationEndpoint, payload)
	if err != nil {
		return oauthClientRecord{}, asOAuthError(err)
	}
	if response.ClientID == "" {
		return oauthClientRecord{}, oauthErrorf(OAuthErrRegistration,
			"dynamic client registration response has no client_id")
	}
	return oauthClientRecord{
		ID:     response.ClientID,
		Secret: response.ClientSecret,
		Source: "dcr",
	}, nil
}

// postRegistration 发送 DCR 请求；部分 OIDC 服务器不接受 application_type，
// 按规范建议去掉该字段重试一次。
func (c *OAuthClient) postRegistration(ctx context.Context,
	endpoint string, payload dynamicClientRegistrationRequest) (*dynamicClientRegistrationResponse, error) {
	status, body, err := c.postJSON(ctx, endpoint, payload)
	if err != nil {
		return nil, err
	}
	if status >= 200 && status < 300 {
		return decodeRegistrationBody(body)
	}
	if status == http.StatusBadRequest {
		retry := payload
		retry.ApplicationType = ""
		retryStatus, retryBody, retryErr := c.postJSON(ctx, endpoint, retry)
		if retryErr == nil && retryStatus >= 200 && retryStatus < 300 {
			if decoded, decodeErr := decodeRegistrationBody(retryBody); decodeErr == nil {
				return decoded, nil
			}
		}
		body = retryBody
		status = retryStatus
	}
	return nil, oauthErrorf(OAuthErrRegistration,
		"dynamic client registration failed with status %d: %s", status, safeSnippet(body, 200))
}

// decodeRegistrationBody 解析 RFC 7591 注册响应。
func decodeRegistrationBody(body []byte) (*dynamicClientRegistrationResponse, error) {
	var decoded dynamicClientRegistrationResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, oauthWrap(OAuthErrRegistration, err, "decode client registration response")
	}
	if strings.TrimSpace(decoded.ClientID) == "" {
		return nil, oauthErrorf(OAuthErrRegistration, "dynamic client registration response has no client_id")
	}
	return &decoded, nil
}

// postJSON POST 一个 JSON 请求并返回状态码与响应体。
func (c *OAuthClient) postJSON(ctx context.Context, endpoint string, payload any) (int, []byte, error) {
	if err := ensureSecureEndpoint(endpoint); err != nil {
		return 0, nil, oauthWrap(OAuthErrRegistration, err, "registration endpoint")
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, oauthWrap(OAuthErrRegistration, err, "encode registration request")
	}
	requestCtx, cancel := c.withTimeout(ctx)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := c.base.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, oauthMaxBodyBytes))
	if err != nil {
		return response.StatusCode, nil, err
	}
	return response.StatusCode, body, nil
}

// tokenEndpointAuthMethod 报告是否使用 client secret（confidential client）。
func (c oauthClientRecord) usesSecret() bool {
	return strings.TrimSpace(c.Secret) != ""
}
