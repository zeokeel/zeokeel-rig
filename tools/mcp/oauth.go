package mcp

// OAuth 2.1 授权客户端：为需要 OAuth 的远端 MCP server（Streamable HTTP）提供
// MCP Authorization 规范要求的发现、客户端注册、PKCE、回环回调、令牌存取、
// 刷新与撤销能力。
//
// 设计边界：
//   - 本文件与 oauth_*.go 只依赖标准库，不依赖 UI/宿主；浏览器通过
//     OAuthConfig.OpenBrowser 注入，令牌持久化通过 OAuthConfig.TokenStore 注入。
//   - 授权协议（端点、参数、轮换）由本包负责；调用方只做三件事：
//     构造 OAuthClient、把 HTTPClient() 交给 mcp.Config/HTTPConfig、在 UI 线程
//     调用 Prepare/Wait 并展示状态。
//   - 静态 Bearer 不需要本类型：现有 Headers 已可发送 Authorization。
//
// 安全要点：
//   - PKCE 必须是 S256；Authorization Server metadata 缺少
//     code_challenge_methods_supported（或其中没有 S256）时拒绝授权，不做降级。
//   - Protected Resource Metadata 的 resource、AS metadata 的 issuer 必须与请求
//     时使用的标识精确匹配；回环回调校验 state 与 RFC 9207 的 iss。
//   - 远程 PRM/AS/token 端点必须是 https；只有回环地址允许 http。
//   - 令牌只发给该 MCP server 对应的 authorization server；错误与日志不回显令牌。

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// oauthDefaultTimeout 是发现/注册/令牌请求的缺省单请求超时。
	oauthDefaultTimeout = 30 * time.Second
	// oauthFlowTTL 是一次交互式授权等待回调的最长时间。
	oauthFlowTTL = 5 * time.Minute
	// oauthRefreshWindow 是提前刷新 access token 的窗口。
	oauthRefreshWindow = 60 * time.Second
	// oauthMaxBodyBytes 限制元数据/令牌响应体大小。
	oauthMaxBodyBytes = 1 << 20
)

// OAuthState 是 OAuth 授权的可见状态。宿主据此渲染卡片/弹窗，不自行推断。
type OAuthState string

const (
	// OAuthStateNone 表示未配置 OAuth 或已撤销。
	OAuthStateNone OAuthState = "none"
	// OAuthStateRequired 表示 server 要求授权，但本地还没有可用令牌。
	OAuthStateRequired OAuthState = "required"
	// OAuthStateAuthorizing 表示交互式授权流程正在进行。
	OAuthStateAuthorizing OAuthState = "authorizing"
	// OAuthStateAuthorized 表示本地有令牌（可能在到期后由下一次请求刷新）。
	OAuthStateAuthorized OAuthState = "authorized"
	// OAuthStateExpired 表示刷新失败（invalid_grant/invalid_client），需要重新授权。
	OAuthStateExpired OAuthState = "expired"
	// OAuthStateError 表示其他可诊断错误（配置/网络/注册/持久化等）。
	OAuthStateError OAuthState = "error"
)

// OAuthErrorCode 是稳定的错误分类，宿主用于本地化文案；不是给模型看的文本。
type OAuthErrorCode string

const (
	OAuthErrInvalidConfig          OAuthErrorCode = "invalid_config"
	OAuthErrNeedsAuthorization     OAuthErrorCode = "needs_authorization"
	OAuthErrInteractiveUnavailable OAuthErrorCode = "interactive_unavailable"
	OAuthErrDiscovery              OAuthErrorCode = "discovery_failed"
	OAuthErrResourceMismatch       OAuthErrorCode = "resource_mismatch"
	OAuthErrASMetadata             OAuthErrorCode = "as_metadata_failed"
	OAuthErrIssuerMismatch         OAuthErrorCode = "issuer_mismatch"
	OAuthErrPKCEUnsupported        OAuthErrorCode = "pkce_unsupported"
	OAuthErrRegistration           OAuthErrorCode = "registration_failed"
	OAuthErrBrowser                OAuthErrorCode = "browser_open_failed"
	OAuthErrCallbackTimeout        OAuthErrorCode = "callback_timeout"
	OAuthErrAccessDenied           OAuthErrorCode = "access_denied"
	OAuthErrStateMismatch          OAuthErrorCode = "state_mismatch"
	OAuthErrISSMismatch            OAuthErrorCode = "iss_mismatch"
	OAuthErrTokenExchange          OAuthErrorCode = "token_exchange_failed"
	OAuthErrStorage                OAuthErrorCode = "storage_failed"
	OAuthErrRefresh                OAuthErrorCode = "refresh_failed"
	OAuthErrInsufficientScope      OAuthErrorCode = "insufficient_scope"
	OAuthErrRevoked                OAuthErrorCode = "revoked"
	OAuthErrHTTP                   OAuthErrorCode = "http_failed"
)

// OAuthError 是 OAuth 客户端的结构化错误。Code 稳定；Message 是技术诊断，
// 可以展示在详情里，但不要直接当正文（宿主应本地化）。
type OAuthError struct {
	Code      OAuthErrorCode
	Message   string
	Retryable bool
	cause     error
	// tokenCode 是令牌端点返回的原始 RFC 6749 error（如 invalid_grant）；
	// 刷新是否应清空令牌只依据它，不依据 Message 文本。
	tokenCode string
}

// Error 实现 error。
func (e *OAuthError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Message == "" {
		return "mcp oauth: " + string(e.Code)
	}
	return "mcp oauth: " + string(e.Code) + ": " + e.Message
}

// Unwrap 返回底层错误（若有）。
func (e *OAuthError) Unwrap() error { return e.cause }

func oauthErrorf(code OAuthErrorCode, format string, args ...any) *OAuthError {
	return &OAuthError{Code: code, Message: fmt.Sprintf(format, args...)}
}

func oauthWrap(code OAuthErrorCode, err error, format string, args ...any) *OAuthError {
	return &OAuthError{Code: code, Message: fmt.Sprintf(format, args...), cause: err}
}

// TokenStore 由宿主持久化一次授权得到的令牌记录。data 是不透明字节（由本包
// 序列化的版本化 JSON，含 access/refresh token 与可能的 client_secret）；宿主
// 必须按机密数据保存（例如 0600 权限、不进入日志/导出/诊断），并只原样
// 保存/读取/删除。
type TokenStore interface {
	Load(ctx context.Context, server string) (data []byte, found bool, err error)
	Save(ctx context.Context, server string, data []byte) error
	Delete(ctx context.Context, server string) error
}

// OAuthConfig 配置一个 MCP server 的 OAuth 客户端。
type OAuthConfig struct {
	// ServerName 是稳定的服务器身份（TokenStore 的 key、日志与状态查询使用）。
	ServerName string
	// Resource 是 MCP server 的 canonical URI（RFC 8707），必填。
	Resource string
	// Scopes 是期望的作用域；空值时依次使用 401 challenge 的 scope、
	// Protected Resource Metadata 的 scopes_supported。
	Scopes []string
	// AuthorizationServer 可选：覆盖 Protected Resource Metadata 中的 AS 选择。
	AuthorizationServer string
	// ClientID/ClientSecret 可选：预注册客户端。ClientID 非空时跳过动态注册。
	ClientID     string
	ClientSecret string
	// RedirectURI 可选：只允许 http://127.0.0.1:<port>/<path> 形式的回环地址；
	// 空值自动监听 127.0.0.1:0 并生成回调地址。
	RedirectURI string
	// OpenBrowser 由宿主提供，在 UI 线程打开授权页；nil 表示无桌面环境
	// （交互式授权返回 OAuthErrInteractiveUnavailable，已有令牌仍可刷新）。
	OpenBrowser func(authURL string) error
	// TokenStore 必填：令牌记录的持久化实现。
	TokenStore TokenStore
	// HTTPClient 可选：发现/注册/令牌请求使用的客户端（代理、自定义根证书）。
	// 它不会带上 Authorization 头；MCP 请求使用本客户端包装后的 transport。
	HTTPClient *http.Client
	// Now 可选：测试用的时钟。
	Now func() time.Time
	// Logf 可选：诊断日志（不得记录令牌/code/verifier）。
	Logf func(format string, args ...any)
	// Timeout 是发现/注册/令牌请求的单请求超时（0 = 30s）。
	Timeout time.Duration
}

// OAuthFlow 是一次已准备好的交互式授权：宿主先打开 AuthURL，再调用 Wait。
// 它只包含可以公开的字段；PKCE verifier/state 保存在客户端内部。
type OAuthFlow struct {
	AuthURL     string
	RedirectURI string
	Issuer      string
	Scopes      []string
	ExpiresAt   time.Time
}

// OAuthStatus 是当前授权状态的只读快照（Status 不做 I/O）。
type OAuthStatus struct {
	State     OAuthState
	Issuer    string
	Resource  string
	Scopes    []string
	ExpiresAt time.Time
	Err       *OAuthError
}

type oauthChallenge struct {
	ResourceMetadata string
	Scope            string
	Error            string
	ErrorDescription string
}

type oauthMetadata struct {
	issuer                string
	authorizationEndpoint string
	tokenEndpoint         string
	registrationEndpoint  string
	revocationEndpoint    string
	scopesSupported       []string
	codeChallengeMethods  []string
	issParameterSupported bool
}

type oauthFlowState struct {
	authURL            string
	redirectURI        string
	verifier           string
	state              string
	issuer             string
	scopes             []string
	client             oauthClientRecord
	tokenEndpoint      string
	revocationEndpoint string
	metadata           *oauthMetadata
	expiresAt          time.Time
	loop               *oauthLoopback
}

// OAuthClient 是一个 MCP server 的 OAuth 授权客户端，可安全并发调用。
// Status 可从 UI 主线程每帧调用；Prepare/Wait/Refresh/Revoke 会阻塞，必须由
// 宿主在后台 goroutine 调用。
type OAuthClient struct {
	cfg       OAuthConfig
	now       func() time.Time
	base      *http.Client
	mcpClient *http.Client

	mu        sync.Mutex
	state     OAuthState
	lastErr   *OAuthError
	token     *oauthTokenRecord
	challenge *oauthChallenge
	flow      *oauthFlowState
	metadata  *oauthMetadata
	closed    bool
	preparing bool
	// epoch 在 Revoke 时递增；进行中的刷新据此丢弃“撤销后才返回”的令牌。
	epoch uint64

	// 刷新单飞：并发 401 共享同一次 refresh 请求的结果。
	refreshing     bool
	refreshWait    chan struct{}
	lastRefreshErr error
}

// NewOAuthClient 校验配置、加载已持久化的令牌并返回客户端。
func NewOAuthClient(cfg OAuthConfig) (*OAuthClient, error) {
	cfg.ServerName = strings.TrimSpace(cfg.ServerName)
	if cfg.ServerName == "" {
		return nil, errors.New("mcp: OAuthConfig.ServerName is required")
	}
	resource, err := canonicalResource(cfg.Resource)
	if err != nil {
		return nil, fmt.Errorf("mcp: OAuthConfig.Resource: %w", err)
	}
	cfg.Resource = resource
	if cfg.TokenStore == nil {
		return nil, errors.New("mcp: OAuthConfig.TokenStore is required")
	}
	if cfg.RedirectURI != "" {
		if _, err := parseLoopbackRedirect(cfg.RedirectURI); err != nil {
			return nil, fmt.Errorf("mcp: OAuthConfig.RedirectURI: %w", err)
		}
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = oauthDefaultTimeout
	} else if cfg.Timeout < 0 {
		cfg.Timeout = 0
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	base := cfg.HTTPClient
	if base == nil {
		base = &http.Client{}
	}
	client := &OAuthClient{
		cfg:   cfg,
		now:   now,
		base:  base,
		state: OAuthStateNone,
	}
	client.mcpClient = &http.Client{
		Transport:     &oauthTransport{client: client, base: baseTransport(base)},
		CheckRedirect: oauthRedirectPolicy(base.CheckRedirect),
		Jar:           base.Jar,
	}
	client.loadStoredToken(context.Background())
	return client, nil
}

// baseTransport 返回客户端使用的底层 RoundTripper（nil 时用标准库默认值）。
func baseTransport(client *http.Client) http.RoundTripper {
	if client != nil && client.Transport != nil {
		return client.Transport
	}
	return http.DefaultTransport
}

// HTTPClient 返回用于 MCP 请求的 HTTP 客户端：每个请求注入 Bearer，401 时
// 单飞刷新并重试一次。把它交给 mcp.Config.HTTPClient / mcp.HTTPConfig.HTTPClient。
func (c *OAuthClient) HTTPClient() *http.Client {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mcpClient
}

// Status 返回当前授权状态快照，不做任何 I/O。
func (c *OAuthClient) Status() OAuthStatus {
	if c == nil {
		return OAuthStatus{State: OAuthStateNone}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	status := OAuthStatus{
		State:    c.state,
		Resource: c.cfg.Resource,
		Err:      c.lastErr,
	}
	switch {
	case c.token != nil:
		status.Issuer = c.token.Issuer
		status.ExpiresAt = c.token.Token.ExpiresAt
		status.Scopes = splitScope(c.token.Token.Scope)
		if len(status.Scopes) == 0 && c.metadata != nil {
			status.Scopes = append([]string(nil), c.metadata.scopesSupported...)
		}
	case c.metadata != nil:
		status.Issuer = c.metadata.issuer
		status.Scopes = append([]string(nil), c.metadata.scopesSupported...)
	}
	return status
}

// Prepare 执行发现、客户端注册、PKCE 与回环监听，返回待打开的授权 URL。
// 它不打开浏览器；宿主应在返回后自行打开 AuthURL（或调用 Authorize）。
func (c *OAuthClient) Prepare(ctx context.Context) (OAuthFlow, error) {
	if c == nil {
		return OAuthFlow{}, oauthErrorf(OAuthErrInvalidConfig, "nil OAuthClient")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return OAuthFlow{}, oauthErrorf(OAuthErrInvalidConfig, "client is closed")
	}
	if c.flow != nil || c.preparing {
		c.mu.Unlock()
		return OAuthFlow{}, oauthErrorf(OAuthErrInvalidConfig, "an authorization flow is already in progress")
	}
	// 在离开锁、开始网络 I/O 之前占位：并发 Prepare 只有一个能通过，
	// 否则会各自监听一个回环端口（泄漏监听器）。
	c.preparing = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.preparing = false
		c.mu.Unlock()
	}()

	prevState, prevErr := c.stateSnapshot()
	c.setAuthorizing()
	fail := func(err *OAuthError) (OAuthFlow, error) {
		c.restoreState(prevState, prevErr)
		return OAuthFlow{}, err
	}

	metadata, prm, err := c.discover(ctx)
	if err != nil {
		return fail(asOAuthError(err))
	}
	// 预注册凭据/持久化令牌必须绑定到发现出的 issuer（RFC 6749 §2.2 /
	// registration.md 的 Authorization Server Binding）。AS 变了就丢弃旧令牌，
	// 绝不复用另一个 AS 的凭据。
	c.reconcileIssuer(ctx, metadata.issuer)
	scopes := c.scopesFor(prm)
	loop, err := startLoopback(c.cfg.RedirectURI)
	if err != nil {
		return fail(oauthWrap(OAuthErrInvalidConfig, err, "start loopback listener"))
	}
	client, err := c.registerClient(ctx, metadata, loop.redirectURI, scopes)
	if err != nil {
		_ = loop.close()
		return fail(asOAuthError(err))
	}
	verifier, challengeValue, err := newPKCE()
	if err != nil {
		_ = loop.close()
		return fail(oauthWrap(OAuthErrInvalidConfig, err, "generate PKCE"))
	}
	stateValue, err := randomToken(32)
	if err != nil {
		_ = loop.close()
		return fail(oauthWrap(OAuthErrInvalidConfig, err, "generate state"))
	}
	authURL, err := buildAuthorizationURL(metadata.authorizationEndpoint, url.Values{
		"response_type":         {"code"},
		"client_id":             {client.ID},
		"redirect_uri":          {loop.redirectURI},
		"code_challenge":        {challengeValue},
		"code_challenge_method": {"S256"},
		"resource":              {c.cfg.Resource},
		"scope":                 {strings.Join(scopes, " ")},
		"state":                 {stateValue},
	})
	if err != nil {
		_ = loop.close()
		return fail(oauthWrap(OAuthErrInvalidConfig, err, "build authorization URL"))
	}
	flow := &oauthFlowState{
		authURL:            authURL,
		redirectURI:        loop.redirectURI,
		verifier:           verifier,
		state:              stateValue,
		issuer:             metadata.issuer,
		scopes:             scopes,
		client:             client,
		tokenEndpoint:      metadata.tokenEndpoint,
		revocationEndpoint: metadata.revocationEndpoint,
		metadata:           metadata,
		expiresAt:          c.now().Add(oauthFlowTTL),
		loop:               loop,
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		_ = loop.close()
		return OAuthFlow{}, oauthErrorf(OAuthErrInvalidConfig, "client is closed")
	}
	c.flow = flow
	c.metadata = metadata
	c.lastErr = nil
	c.state = OAuthStateAuthorizing
	c.mu.Unlock()
	c.logf("oauth %q: prepared authorization flow for issuer %s", c.cfg.ServerName, metadata.issuer)
	return OAuthFlow{
		AuthURL:     authURL,
		RedirectURI: loop.redirectURI,
		Issuer:      metadata.issuer,
		Scopes:      append([]string(nil), scopes...),
		ExpiresAt:   flow.expiresAt,
	}, nil
}

// Wait 等待 Prepare 的回环回调，校验 state/iss，换取并持久化令牌。
func (c *OAuthClient) Wait(ctx context.Context, flow OAuthFlow) (OAuthStatus, error) {
	if c == nil {
		return OAuthStatus{}, oauthErrorf(OAuthErrInvalidConfig, "nil OAuthClient")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	active := c.flow
	if active == nil || active.authURL != flow.AuthURL || active.redirectURI != flow.RedirectURI {
		c.mu.Unlock()
		return c.Status(), oauthErrorf(OAuthErrInvalidConfig, "no matching authorization flow")
	}
	c.mu.Unlock()

	result := active.loop.wait(ctx, time.Until(active.expiresAt))
	_ = active.loop.close()
	c.mu.Lock()
	stillActive := c.flow == active
	if stillActive {
		c.flow = nil
	}
	c.mu.Unlock()
	failWait := func(err *OAuthError) error {
		if stillActive {
			c.fail(err)
		}
		return err
	}
	if result.noCallback {
		return c.Status(), failWait(result.err)
	}
	// 先校验 state / iss，再处理回调里的 error：授权服务器的错误响应同样不能
	// 被跨源的伪造回调利用（RFC 9207 §2.4）。
	if subtle.ConstantTimeCompare([]byte(result.state), []byte(active.state)) != 1 {
		return c.Status(), failWait(oauthErrorf(OAuthErrStateMismatch,
			"authorization response state does not match"))
	}
	if err := validateAuthorizationISS(active.metadata, result.iss); err != nil {
		return c.Status(), failWait(err)
	}
	if result.err != nil {
		return c.Status(), failWait(result.err)
	}
	if !stillActive {
		// 该 flow 已被 Cancel/Close/新的 Prepare 取代；不要把迟到的授权结果写入。
		return c.Status(), oauthErrorf(OAuthErrCallbackTimeout,
			"authorization flow was cancelled before its callback was processed")
	}
	record, err := c.exchangeCode(ctx, active, result.code)
	if err != nil {
		c.fail(asOAuthError(err))
		return c.Status(), err
	}
	if saveErr := c.storeRecord(ctx, record); saveErr != nil {
		// 令牌已拿到但写不进持久层：当前进程仍可在内存里使用，但必须暴露错误，
		// 不能在下次进程启动时静默丢授权。
		c.mu.Lock()
		c.token = record
		c.state = OAuthStateAuthorized
		c.lastErr = saveErr
		c.metadata = active.metadata
		c.challenge = nil
		c.mu.Unlock()
		return c.Status(), saveErr
	}
	c.mu.Lock()
	c.token = record
	c.state = OAuthStateAuthorized
	c.lastErr = nil
	c.metadata = active.metadata
	c.challenge = nil
	c.mu.Unlock()
	c.logf("oauth %q: authorized (issuer %s)", c.cfg.ServerName, record.Issuer)
	return c.Status(), nil
}

// Authorize 是 Prepare + OpenBrowser + Wait 的便捷方法（CLI/无 UI 分步需求时
// 用 Prepare/Wait）。OpenBrowser 为 nil 时返回 OAuthErrInteractiveUnavailable。
func (c *OAuthClient) Authorize(ctx context.Context) (OAuthStatus, error) {
	if c.cfg.OpenBrowser == nil {
		unavailable := oauthErrorf(OAuthErrInteractiveUnavailable,
			"interactive authorization requires a desktop browser")
		c.fail(unavailable)
		return c.Status(), unavailable
	}
	flow, err := c.Prepare(ctx)
	if err != nil {
		return c.Status(), err
	}
	if err := c.cfg.OpenBrowser(flow.AuthURL); err != nil {
		c.Cancel(flow)
		openErr := oauthWrap(OAuthErrBrowser, err, "open authorization URL")
		c.fail(openErr)
		return c.Status(), openErr
	}
	return c.Wait(ctx, flow)
}

// Cancel 关闭一次未完成的授权流程（丢弃 PKCE/state 与回环监听）。重复调用安全。
func (c *OAuthClient) Cancel(flow OAuthFlow) {
	if c == nil {
		return
	}
	c.mu.Lock()
	active := c.flow
	if active == nil || (flow.AuthURL != "" && active.authURL != flow.AuthURL) {
		c.mu.Unlock()
		return
	}
	c.flow = nil
	c.mu.Unlock()
	active.loop.cancel()
	c.mu.Lock()
	switch {
	case c.token != nil:
		c.state = OAuthStateAuthorized
	case c.challenge != nil:
		c.state = OAuthStateRequired
	default:
		c.state = OAuthStateNone
	}
	c.lastErr = nil
	c.mu.Unlock()
}

// Close 关闭客户端：取消进行中的流程并关闭空闲连接。不会删除令牌。
func (c *OAuthClient) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	active := c.flow
	c.flow = nil
	c.closed = true
	c.mu.Unlock()
	if active != nil && active.loop != nil {
		active.loop.cancel()
	}
	if c.mcpClient != nil {
		c.mcpClient.CloseIdleConnections()
	}
	if c.base != nil {
		c.base.CloseIdleConnections()
	}
	return nil
}

// --- 内部状态辅助 -----------------------------------------------------------

func (c *OAuthClient) logf(format string, args ...any) {
	if c.cfg.Logf != nil {
		c.cfg.Logf(format, args...)
	}
}

func (c *OAuthClient) stateSnapshot() (OAuthState, *OAuthError) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state, c.lastErr
}

func (c *OAuthClient) setAuthorizing() {
	c.mu.Lock()
	c.state = OAuthStateAuthorizing
	c.lastErr = nil
	c.mu.Unlock()
}

func (c *OAuthClient) restoreState(state OAuthState, err *OAuthError) {
	c.mu.Lock()
	c.state = state
	c.lastErr = err
	c.mu.Unlock()
}

// fail 记录一次 OAuth 失败：授权类错误回到 required（可重试），其余进入 error。
func (c *OAuthClient) fail(err *OAuthError) {
	if err == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastErr = err
	switch err.Code {
	case OAuthErrNeedsAuthorization, OAuthErrCallbackTimeout, OAuthErrStateMismatch,
		OAuthErrISSMismatch, OAuthErrAccessDenied, OAuthErrBrowser,
		OAuthErrInteractiveUnavailable, OAuthErrRevoked:
		if c.token != nil {
			c.state = OAuthStateAuthorized
		} else {
			c.state = OAuthStateRequired
		}
	case OAuthErrRefresh:
		c.state = OAuthStateExpired
	default:
		c.state = OAuthStateError
	}
}

func (c *OAuthClient) scopesFor(prm *protectedResourceMetadata) []string {
	if len(c.cfg.Scopes) > 0 {
		return normalizedScopes(c.cfg.Scopes)
	}
	c.mu.Lock()
	challenge := c.challenge
	c.mu.Unlock()
	if challenge != nil {
		if scopes := normalizedScopes(splitScope(challenge.Scope)); len(scopes) > 0 {
			return scopes
		}
	}
	if prm != nil {
		return normalizedScopes(prm.ScopesSupported)
	}
	return nil
}

// asOAuthError 把任意错误折叠成 *OAuthError（保留已有分类）。
func asOAuthError(err error) *OAuthError {
	if err == nil {
		return nil
	}
	var oauthErr *OAuthError
	if errors.As(err, &oauthErr) {
		return oauthErr
	}
	return &OAuthError{Code: OAuthErrHTTP, Message: err.Error(), Retryable: true, cause: err}
}

// withTimeout 给单次网络请求附加 OAuthConfig.Timeout。
func (c *OAuthClient) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.cfg.Timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, c.cfg.Timeout)
}

// limitedBody 读取有上限的响应体（元数据/令牌响应都很小）。
func limitedBody(body io.Reader) ([]byte, error) {
	return io.ReadAll(io.LimitReader(body, oauthMaxBodyBytes))
}

// splitScope 把空格分隔的 scope 字符串拆成去重后的切片。
func splitScope(scope string) []string {
	return normalizedScopes(strings.Fields(scope))
}

func normalizedScopes(scopes []string) []string {
	if len(scopes) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(scopes))
	out := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		scope = strings.TrimSpace(scope)
		if scope == "" || seen[scope] {
			continue
		}
		seen[scope] = true
		out = append(out, scope)
	}
	return out
}

// buildAuthorizationURL 在端点已有 query 时正确合并参数。
func buildAuthorizationURL(endpoint string, params url.Values) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return "", err
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return "", fmt.Errorf("authorization endpoint %q is not http(s)", endpoint)
	}
	query := parsed.Query()
	for key, values := range params {
		for _, value := range values {
			if value != "" {
				query.Set(key, value)
			}
		}
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// reconcileIssuer 丢弃与当前发现出的 AS issuer 不一致的已持久化令牌。
func (c *OAuthClient) reconcileIssuer(ctx context.Context, issuer string) {
	c.mu.Lock()
	record := c.token
	c.mu.Unlock()
	if record == nil || strings.TrimSpace(record.Issuer) == issuer {
		return
	}
	_ = c.deleteStoredToken(ctx)
	c.mu.Lock()
	if c.token == record {
		c.token = nil
		c.state = OAuthStateRequired
		c.lastErr = oauthErrorf(OAuthErrIssuerMismatch,
			"stored authorization issuer %q does not match the discovered issuer %q", record.Issuer, issuer)
	}
	c.mu.Unlock()
	c.logf("oauth %q: discarded stored token for a different issuer", c.cfg.ServerName)
}

// oauthRedirectPolicy 拒绝跨 origin 重定向：Authorization 由 Transport 逐跳注入，
// net/http 自带的跨域敏感头剥离对它不生效，跟随重定向会把 Bearer 发给另一个源。
// 同源重定向最多跟随 10 跳，并保留调用方自定义的 CheckRedirect。
func oauthRedirectPolicy(custom func(req *http.Request, via []*http.Request) error) func(req *http.Request, via []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("mcp oauth: stopped after 10 redirects")
		}
		if len(via) > 0 && !sameOrigin(via[0].URL, req.URL) {
			return http.ErrUseLastResponse
		}
		if custom != nil {
			return custom(req, via)
		}
		return nil
	}
}

// sameOrigin 比较 scheme 与 host（含端口）。
func sameOrigin(left, right *url.URL) bool {
	if left == nil || right == nil {
		return false
	}
	return strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Host, right.Host)
}
