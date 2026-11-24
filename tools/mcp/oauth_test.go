package mcp

// OAuth 2.1 客户端协议测试：fake MCP（resource server）+ fake AS（authorization
// server）+ 内存 TokenStore。覆盖发现、注册、PKCE、回调校验、令牌交换、401
// 刷新重试、刷新失效、撤销与持久化重建。

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// memoryTokenStore 是确定性测试用的 TokenStore。
type memoryTokenStore struct {
	mu     sync.Mutex
	values map[string][]byte
}

func newMemoryTokenStore() *memoryTokenStore {
	return &memoryTokenStore{values: make(map[string][]byte)}
}

func (s *memoryTokenStore) Load(_ context.Context, server string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.values[server]
	if !ok {
		return nil, false, nil
	}
	return append([]byte(nil), data...), true, nil
}

func (s *memoryTokenStore) Save(_ context.Context, server string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[server] = append([]byte(nil), data...)
	return nil
}

func (s *memoryTokenStore) Delete(_ context.Context, server string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.values, server)
	return nil
}

func (s *memoryTokenStore) has(server string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.values[server]
	return ok
}

func (s *memoryTokenStore) record(t *testing.T, server string) oauthTokenRecord {
	t.Helper()
	s.mu.Lock()
	data := append([]byte(nil), s.values[server]...)
	s.mu.Unlock()
	var record oauthTokenRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("decode stored token: %v", err)
	}
	return record
}

// fakeAS 是一个最小 OAuth 2.1 authorization server。
type fakeAS struct {
	t        *testing.T
	server   *httptest.Server
	resource string

	mu                       sync.Mutex
	omitPKCE                 bool
	issuerOverride           string
	omitISS                  bool
	rejectRefresh            bool
	expiresIn                int
	authorizeCalls           int
	tokenCalls               int
	refreshCalls             int
	revokeCalls              int
	registerCalls            int
	authorizeQuery           url.Values
	expectedChallenge        string
	accessToken              string
	refreshToken             string
	registrationID           string
	requireNoApplicationType bool
	refreshDelay             time.Duration
	refreshServerError       bool
}

func newFakeAS(t *testing.T) *fakeAS {
	t.Helper()
	as := &fakeAS{
		t:              t,
		expiresIn:      3600,
		accessToken:    "access-1",
		refreshToken:   "refresh-1",
		registrationID: "dcr-client",
	}
	as.server = httptest.NewServer(http.HandlerFunc(as.handle))
	t.Cleanup(as.server.Close)
	return as
}

func (a *fakeAS) issuer() string {
	if a.issuerOverride != "" {
		return a.issuerOverride
	}
	return a.server.URL
}

func (a *fakeAS) handle(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/.well-known/oauth-authorization-server", "/.well-known/openid-configuration":
		a.serveMetadata(w)
	case "/register":
		a.serveRegister(w, r)
	case "/authorize":
		a.serveAuthorize(w, r)
	case "/token":
		a.serveToken(w, r)
	case "/revoke":
		a.serveRevoke(w)
	default:
		http.NotFound(w, r)
	}
}

func (a *fakeAS) serveMetadata(w http.ResponseWriter) {
	methods := []string{"S256"}
	a.mu.Lock()
	if a.omitPKCE {
		methods = []string{}
	}
	issuer := a.issuer()
	a.mu.Unlock()
	writeJSON(a.t, w, map[string]any{
		"issuer":                           issuer,
		"authorization_endpoint":           a.server.URL + "/authorize",
		"token_endpoint":                   a.server.URL + "/token",
		"registration_endpoint":            a.server.URL + "/register",
		"revocation_endpoint":              a.server.URL + "/revoke",
		"scopes_supported":                 []string{"files:read"},
		"code_challenge_methods_supported": methods,
		"authorization_response_iss_parameter_supported": true,
	})
}

func (a *fakeAS) serveRegister(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.registerCalls++
	registrationID := a.registrationID
	requireNoApplicationType := a.requireNoApplicationType
	a.mu.Unlock()
	var request dynamicClientRegistrationRequest
	_ = json.NewDecoder(r.Body).Decode(&request)
	if requireNoApplicationType && request.ApplicationType != "" {
		writeJSONLocked(a.t, w, http.StatusBadRequest, map[string]string{
			"error": "invalid_client_metadata", "error_description": "application_type is not supported",
		})
		return
	}
	if len(request.RedirectURIs) != 1 || !strings.HasPrefix(request.RedirectURIs[0], "http://127.0.0.1:") {
		writeJSONLocked(a.t, w, http.StatusBadRequest, map[string]string{"error": "invalid_redirect_uri"})
		return
	}
	writeJSON(a.t, w, map[string]any{"client_id": registrationID, "token_endpoint_auth_method": "none"})
}

func (a *fakeAS) serveAuthorize(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	a.mu.Lock()
	a.authorizeCalls++
	a.authorizeQuery = query
	a.expectedChallenge = query.Get("code_challenge")
	omitISS := a.omitISS
	issuer := a.issuer()
	a.mu.Unlock()

	target, err := url.Parse(query.Get("redirect_uri"))
	if err != nil {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	values := target.Query()
	values.Set("code", "auth-code-1")
	values.Set("state", query.Get("state"))
	if !omitISS {
		values.Set("iss", issuer)
	}
	target.RawQuery = values.Encode()
	http.Redirect(w, r, target.String(), http.StatusFound)
}

func (a *fakeAS) serveToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
		return
	}
	grant := r.PostForm.Get("grant_type")
	a.mu.Lock()
	defer a.mu.Unlock()
	switch grant {
	case "authorization_code":
		a.tokenCalls++
		if r.PostForm.Get("code") != "auth-code-1" || r.PostForm.Get("resource") != a.resource {
			writeJSONLocked(a.t, w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
			return
		}
		verifier := r.PostForm.Get("code_verifier")
		sum := sha256.Sum256([]byte(verifier))
		challenge := base64.RawURLEncoding.EncodeToString(sum[:])
		if a.expectedChallenge == "" || challenge != a.expectedChallenge {
			writeJSONLocked(a.t, w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
			return
		}
		a.accessToken = "access-1"
		a.refreshToken = "refresh-1"
	case "refresh_token":
		a.refreshCalls++
		if a.refreshServerError {
			writeJSONLocked(a.t, w, http.StatusServiceUnavailable, map[string]string{
				"error": "server_error", "error_description": "certificate expired",
			})
			return
		}
		if a.refreshDelay > 0 {
			delay := a.refreshDelay
			a.mu.Unlock()
			time.Sleep(delay)
			a.mu.Lock()
		}
		if a.rejectRefresh || r.PostForm.Get("refresh_token") != a.refreshToken {
			writeJSONLocked(a.t, w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
			return
		}
		a.accessToken = "access-2"
		a.refreshToken = "refresh-2"
	default:
		writeJSONLocked(a.t, w, http.StatusBadRequest, map[string]string{"error": "unsupported_grant_type"})
		return
	}
	expiresIn := a.expiresIn
	access, refresh := a.accessToken, a.refreshToken
	writeJSONLocked(a.t, w, http.StatusOK, map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    expiresIn,
		"refresh_token": refresh,
		"scope":         "files:read",
	})
}

func (a *fakeAS) serveRevoke(w http.ResponseWriter) {
	a.mu.Lock()
	a.revokeCalls++
	a.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (a *fakeAS) calls() (register, authorize, token, refresh, revoke int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.registerCalls, a.authorizeCalls, a.tokenCalls, a.refreshCalls, a.revokeCalls
}

func (a *fakeAS) lastAuthorizeQuery() url.Values {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.authorizeQuery
}

func (a *fakeAS) setRejectRefresh(reject bool) {
	a.mu.Lock()
	a.rejectRefresh = reject
	a.mu.Unlock()
}

func (a *fakeAS) setOmitISS(omit bool) {
	a.mu.Lock()
	a.omitISS = omit
	a.mu.Unlock()
}

func (a *fakeAS) setOmitPKCE(omit bool) {
	a.mu.Lock()
	a.omitPKCE = omit
	a.mu.Unlock()
}

func (a *fakeAS) setIssuerOverride(issuer string) {
	a.mu.Lock()
	a.issuerOverride = issuer
	a.mu.Unlock()
}

func (a *fakeAS) setRequireNoApplicationType(required bool) {
	a.mu.Lock()
	a.requireNoApplicationType = required
	a.mu.Unlock()
}

func (a *fakeAS) setRefreshServerError(enabled bool) {
	a.mu.Lock()
	a.refreshServerError = enabled
	a.mu.Unlock()
}

func (a *fakeAS) setRefreshDelay(delay time.Duration) {
	a.mu.Lock()
	a.refreshDelay = delay
	a.mu.Unlock()
}

func (a *fakeAS) setResource(resource string) {
	a.mu.Lock()
	a.resource = resource
	a.mu.Unlock()
}

// fakeMCP 是一个要求 Bearer 的远端 MCP server（Streamable HTTP）。
type fakeMCP struct {
	t      *testing.T
	server *httptest.Server
	asURL  string

	mu              sync.Mutex
	expectedBearer  string
	challenge       bool
	prmPath         string
	initializeCalls int
	forbiddenTools  bool
	redirectTo      string
	sawAuth         []string
}

func newFakeMCP(t *testing.T, asURL string) *fakeMCP {
	t.Helper()
	server := &fakeMCP{t: t, asURL: asURL, challenge: true}
	server.server = httptest.NewServer(http.HandlerFunc(server.handle))
	t.Cleanup(server.server.Close)
	return server
}

func (s *fakeMCP) setExpectedBearer(token string) {
	s.mu.Lock()
	s.expectedBearer = token
	s.mu.Unlock()
}

func (s *fakeMCP) setChallenge(enabled bool) {
	s.mu.Lock()
	s.challenge = enabled
	s.mu.Unlock()
}

func (s *fakeMCP) setRedirectTo(target string) {
	s.mu.Lock()
	s.redirectTo = target
	s.mu.Unlock()
}

func (s *fakeMCP) setForbiddenTools(forbidden bool) {
	s.mu.Lock()
	s.forbiddenTools = forbidden
	s.mu.Unlock()
}

func (s *fakeMCP) setPRMPath(path string) {
	s.mu.Lock()
	s.prmPath = path
	s.mu.Unlock()
}

func (s *fakeMCP) authCalls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sawAuth...)
}

func (s *fakeMCP) initializeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.initializeCalls
}

func (s *fakeMCP) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	challenge := s.challenge
	prmPath := s.prmPath
	expected := s.expectedBearer
	s.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && (r.URL.Path == "/.well-known/oauth-protected-resource" ||
		(prmPath != "" && r.URL.Path == prmPath)):
		writeJSON(s.t, w, map[string]any{
			"resource":              s.server.URL,
			"authorization_servers": []string{s.asURL},
			"scopes_supported":      []string{"files:read"},
		})
		return
	case r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
		return
	case r.Method == http.MethodGet:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	case r.Method != http.MethodPost:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	authorization := r.Header.Get("Authorization")
	s.mu.Lock()
	s.sawAuth = append(s.sawAuth, authorization)
	s.mu.Unlock()
	if expected != "" && authorization != "Bearer "+expected {
		if challenge {
			metadataPath := "/.well-known/oauth-protected-resource"
			if prmPath != "" {
				metadataPath = prmPath
			}
			w.Header().Set("WWW-Authenticate",
				fmt.Sprintf(`Bearer resource_metadata="%s%s", scope="files:read"`, s.server.URL, metadataPath))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
		return
	}
	message := readHTTPRPCRequest(s.t, r)
	switch message.Method {
	case "initialize":
		s.mu.Lock()
		s.initializeCalls++
		redirectTo := s.redirectTo
		s.mu.Unlock()
		if redirectTo != "" {
			w.Header().Set("Location", redirectTo)
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		writeHTTPRPCResult(s.t, w, message.ID, httpInitializeResult())
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "tools/list":
		s.mu.Lock()
		forbidden := s.forbiddenTools
		s.mu.Unlock()
		if forbidden {
			w.Header().Set("WWW-Authenticate",
				`Bearer error="insufficient_scope", scope="files:write"`)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		writeHTTPRPCResult(s.t, w, message.ID, map[string]any{"tools": []any{}})
	default:
		writeHTTPRPCError(s.t, w, message.ID, -32601, "method not found", nil)
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	writeJSONLocked(t, w, http.StatusOK, value)
}

func writeJSONLocked(t *testing.T, w http.ResponseWriter, status int, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode JSON: %v", err)
	}
}

// browserOpening 返回一个模拟系统浏览器的回调：GET 授权 URL 并跟随 302 到
// 回环回调。
func browserOpening(t *testing.T) func(string) error {
	t.Helper()
	return func(authURL string) error {
		response, err := http.Get(authURL)
		if err != nil {
			return err
		}
		return response.Body.Close()
	}
}

func newOAuthTestClient(t *testing.T, resource string, store TokenStore, browser func(string) error) *OAuthClient {
	t.Helper()
	client, err := NewOAuthClient(OAuthConfig{
		ServerName:  "fake",
		Resource:    resource,
		TokenStore:  store,
		OpenBrowser: browser,
		Timeout:     5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewOAuthClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// authorizeWithBrowser 执行 Prepare → 模拟浏览器 → Wait。
func authorizeWithBrowser(t *testing.T, client *OAuthClient) OAuthStatus {
	t.Helper()
	flow, err := client.Prepare(context.Background())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if client.cfg.OpenBrowser == nil {
		t.Fatal("test requires a browser callback")
	}
	if err := client.cfg.OpenBrowser(flow.AuthURL); err != nil {
		t.Fatalf("open browser: %v", err)
	}
	status, err := client.Wait(context.Background(), flow)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if status.State != OAuthStateAuthorized {
		t.Fatalf("status state = %q, want authorized", status.State)
	}
	return status
}

func oauthErrorCode(t *testing.T, err error) OAuthErrorCode {
	t.Helper()
	var oauthErr *OAuthError
	if !errors.As(err, &oauthErr) {
		t.Fatalf("error %v is not *OAuthError", err)
	}
	return oauthErr.Code
}

func TestOAuthAuthorizationPersistenceAndConnect(t *testing.T) {
	as := newFakeAS(t)
	resource := newFakeMCP(t, as.server.URL)
	as.setResource(resource.server.URL)
	resource.setExpectedBearer("access-1")
	store := newMemoryTokenStore()
	client := newOAuthTestClient(t, resource.server.URL, store, browserOpening(t))

	status := authorizeWithBrowser(t, client)
	if status.Issuer != as.server.URL {
		t.Fatalf("issuer = %q, want %q", status.Issuer, as.server.URL)
	}
	if !store.has("fake") {
		t.Fatal("token was not persisted")
	}
	record := store.record(t, "fake")
	if record.Token.AccessToken != "access-1" || record.Token.RefreshToken != "refresh-1" {
		t.Fatalf("stored token = %+v", record.Token)
	}
	if record.Issuer != as.server.URL {
		t.Fatalf("stored issuer = %q", record.Issuer)
	}

	rigClient, err := StartHTTP(context.Background(), HTTPConfig{URL: resource.server.URL, OAuth: client})
	if err != nil {
		t.Fatalf("StartHTTP with OAuth: %v", err)
	}
	defer rigClient.Close()
	if got := resource.initializeCount(); got != 1 {
		t.Fatalf("initialize calls = %d, want 1", got)
	}
	if calls := resource.authCalls(); len(calls) == 0 || calls[0] != "Bearer access-1" {
		t.Fatalf("MCP saw Authorization %v, want Bearer access-1", calls)
	}

	// 模拟进程重启：新客户端从 TokenStore 直接恢复授权，不需要浏览器。
	restarted := newOAuthTestClient(t, resource.server.URL, store, nil)
	if got := restarted.Status(); got.State != OAuthStateAuthorized {
		t.Fatalf("restored state = %q, want authorized", got.State)
	}
	rigRestarted, err := StartHTTP(context.Background(), HTTPConfig{URL: resource.server.URL, OAuth: restarted})
	if err != nil {
		t.Fatalf("StartHTTP after restart: %v", err)
	}
	_ = rigRestarted.Close()
}

func TestOAuthPrepareRejectsWeakPKCEAndIssuerMismatch(t *testing.T) {
	as := newFakeAS(t)
	resource := newFakeMCP(t, as.server.URL)
	as.setResource(resource.server.URL)

	as.setOmitPKCE(true)
	client := newOAuthTestClient(t, resource.server.URL, newMemoryTokenStore(), browserOpening(t))
	if _, err := client.Prepare(context.Background()); err == nil || oauthErrorCode(t, err) != OAuthErrPKCEUnsupported {
		t.Fatalf("Prepare without S256: %v", err)
	}
	_ = client.Close()

	as.setOmitPKCE(false)
	as.setIssuerOverride("https://wrong.example")
	client = newOAuthTestClient(t, resource.server.URL, newMemoryTokenStore(), browserOpening(t))
	if _, err := client.Prepare(context.Background()); err == nil || oauthErrorCode(t, err) != OAuthErrIssuerMismatch {
		t.Fatalf("Prepare with issuer mismatch: %v", err)
	}
}

func TestOAuthChallengeDrivesDiscoveryWhenWellKnownMissing(t *testing.T) {
	as := newFakeAS(t)
	resource := newFakeMCP(t, as.server.URL)
	as.setResource(resource.server.URL)
	resource.setExpectedBearer("access-1")
	resource.setPRMPath("/prm")

	store := newMemoryTokenStore()
	client := newOAuthTestClient(t, resource.server.URL, store, browserOpening(t))
	// 先按真实产品路径连接一次：401 捕获 WWW-Authenticate 的 resource_metadata。
	if _, err := StartHTTP(context.Background(), HTTPConfig{URL: resource.server.URL, OAuth: client}); err == nil {
		t.Fatal("expected the unauthenticated connection to fail")
	}
	status := authorizeWithBrowser(t, client)
	if status.State != OAuthStateAuthorized {
		t.Fatalf("state = %q", status.State)
	}
	rigClient, err := StartHTTP(context.Background(), HTTPConfig{URL: resource.server.URL, OAuth: client})
	if err != nil {
		t.Fatalf("StartHTTP after authorization: %v", err)
	}
	_ = rigClient.Close()
}

func TestOAuthWellKnownFallbackWithoutChallenge(t *testing.T) {
	as := newFakeAS(t)
	resource := newFakeMCP(t, as.server.URL)
	as.setResource(resource.server.URL)
	resource.setExpectedBearer("access-1")
	resource.setChallenge(false)

	client := newOAuthTestClient(t, resource.server.URL, newMemoryTokenStore(), browserOpening(t))
	flow, err := client.Prepare(context.Background())
	if err != nil {
		t.Fatalf("Prepare via well-known fallback: %v", err)
	}
	if err := client.cfg.OpenBrowser(flow.AuthURL); err != nil {
		t.Fatalf("open browser: %v", err)
	}
	if _, err := client.Wait(context.Background(), flow); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

func TestOAuthRefreshOnUnauthorizedAndRetry(t *testing.T) {
	as := newFakeAS(t)
	resource := newFakeMCP(t, as.server.URL)
	as.setResource(resource.server.URL)
	resource.setExpectedBearer("access-2") // 首次 access-1 必须 401 后刷新
	store := newMemoryTokenStore()
	client := newOAuthTestClient(t, resource.server.URL, store, browserOpening(t))
	if status := authorizeWithBrowser(t, client); status.State != OAuthStateAuthorized {
		t.Fatalf("state = %q", status.State)
	}

	rigClient, err := StartHTTP(context.Background(), HTTPConfig{URL: resource.server.URL, OAuth: client})
	if err != nil {
		t.Fatalf("StartHTTP after refresh: %v", err)
	}
	_ = rigClient.Close()
	if _, _, _, refresh, _ := as.calls(); refresh != 1 {
		t.Fatalf("refresh calls = %d, want 1", refresh)
	}
	if record := store.record(t, "fake"); record.Token.AccessToken != "access-2" {
		t.Fatalf("stored access token = %q, want access-2", record.Token.AccessToken)
	}
}

func TestOAuthRefreshInvalidGrantRequiresReauthorization(t *testing.T) {
	as := newFakeAS(t)
	resource := newFakeMCP(t, as.server.URL)
	as.setResource(resource.server.URL)
	resource.setExpectedBearer("access-1")
	store := newMemoryTokenStore()
	client := newOAuthTestClient(t, resource.server.URL, store, browserOpening(t))
	authorizeWithBrowser(t, client)

	as.setRejectRefresh(true)
	status, err := client.Refresh(context.Background())
	if err == nil || oauthErrorCode(t, err) != OAuthErrRefresh {
		t.Fatalf("Refresh after invalid_grant = %v", err)
	}
	if status.State != OAuthStateExpired {
		t.Fatalf("state = %q, want expired", status.State)
	}
	if store.has("fake") {
		t.Fatal("expired authorization must delete the stored token")
	}
}

func TestOAuthRevokeClearsTokenAndCallsEndpoint(t *testing.T) {
	as := newFakeAS(t)
	resource := newFakeMCP(t, as.server.URL)
	as.setResource(resource.server.URL)
	resource.setExpectedBearer("access-1")
	store := newMemoryTokenStore()
	client := newOAuthTestClient(t, resource.server.URL, store, browserOpening(t))
	authorizeWithBrowser(t, client)

	if err := client.Revoke(context.Background()); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if store.has("fake") {
		t.Fatal("Revoke must delete the stored token")
	}
	if status := client.Status(); status.State != OAuthStateNone {
		t.Fatalf("state = %q, want none", status.State)
	}
	if _, _, _, _, revoke := as.calls(); revoke != 1 {
		t.Fatalf("revoke calls = %d, want 1", revoke)
	}
}

func TestOAuthCallbackValidation(t *testing.T) {
	as := newFakeAS(t)
	resource := newFakeMCP(t, as.server.URL)
	as.setResource(resource.server.URL)
	resource.setExpectedBearer("access-1")

	// state 被篡改。
	client := newOAuthTestClient(t, resource.server.URL, newMemoryTokenStore(), func(authURL string) error {
		parsed, err := url.Parse(authURL)
		if err != nil {
			return err
		}
		query := parsed.Query()
		query.Set("state", "wrong-state")
		parsed.RawQuery = query.Encode()
		response, err := http.Get(parsed.String())
		if err != nil {
			return err
		}
		return response.Body.Close()
	})
	flow, err := client.Prepare(context.Background())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := client.cfg.OpenBrowser(flow.AuthURL); err != nil {
		t.Fatalf("open browser: %v", err)
	}
	if _, err := client.Wait(context.Background(), flow); err == nil || oauthErrorCode(t, err) != OAuthErrStateMismatch {
		t.Fatalf("Wait with wrong state = %v", err)
	}
	_ = client.Close()

	// 授权响应缺少 iss（metadata 声明支持 iss）。
	as.setOmitISS(true)
	client = newOAuthTestClient(t, resource.server.URL, newMemoryTokenStore(), browserOpening(t))
	flow, err = client.Prepare(context.Background())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := client.cfg.OpenBrowser(flow.AuthURL); err != nil {
		t.Fatalf("open browser: %v", err)
	}
	if _, err := client.Wait(context.Background(), flow); err == nil || oauthErrorCode(t, err) != OAuthErrISSMismatch {
		t.Fatalf("Wait without iss = %v", err)
	}
}

func TestOAuthHeadlessAndRegistrationSelection(t *testing.T) {
	as := newFakeAS(t)
	resource := newFakeMCP(t, as.server.URL)
	as.setResource(resource.server.URL)
	resource.setExpectedBearer("access-1")

	headless := newOAuthTestClient(t, resource.server.URL, newMemoryTokenStore(), nil)
	if _, err := headless.Authorize(context.Background()); err == nil || oauthErrorCode(t, err) != OAuthErrInteractiveUnavailable {
		t.Fatalf("headless Authorize = %v", err)
	}

	client := newOAuthTestClient(t, resource.server.URL, newMemoryTokenStore(), browserOpening(t))
	authorizeWithBrowser(t, client)
	if register, _, _, _, _ := as.calls(); register != 1 {
		t.Fatalf("DCR calls = %d, want 1", register)
	}
	var ctx context.Context = context.Background()
	_ = ctx

	// 预注册客户端必须跳过 DCR，并以该 client_id 发起授权。
	preRegistered, err := NewOAuthClient(OAuthConfig{
		ServerName:  "pre",
		Resource:    resource.server.URL,
		ClientID:    "pre-client",
		TokenStore:  newMemoryTokenStore(),
		OpenBrowser: browserOpening(t),
		Timeout:     5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewOAuthClient pre-registered: %v", err)
	}
	defer preRegistered.Close()
	authorizeWithBrowser(t, preRegistered)
	if register, _, _, _, _ := as.calls(); register != 1 {
		t.Fatalf("pre-registered client triggered DCR (%d calls)", register)
	}
	if got := as.lastAuthorizeQuery().Get("client_id"); got != "pre-client" {
		t.Fatalf("authorize client_id = %q, want pre-client", got)
	}
}

// failingSaveStore 模拟持久化失败。
type failingSaveStore struct {
	*memoryTokenStore
}

func (s failingSaveStore) Save(context.Context, string, []byte) error {
	return errors.New("disk is full")
}

func TestOAuthCallbackTimeout(t *testing.T) {
	as := newFakeAS(t)
	resource := newFakeMCP(t, as.server.URL)
	as.setResource(resource.server.URL)
	client := newOAuthTestClient(t, resource.server.URL, newMemoryTokenStore(), browserOpening(t))
	flow, err := client.Prepare(context.Background())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := client.Wait(ctx, flow); err == nil || oauthErrorCode(t, err) != OAuthErrCallbackTimeout {
		t.Fatalf("Wait timeout = %v", err)
	}
}

func TestOAuthStorageFailureSurfaces(t *testing.T) {
	as := newFakeAS(t)
	resource := newFakeMCP(t, as.server.URL)
	as.setResource(resource.server.URL)
	resource.setExpectedBearer("access-1")
	store := failingSaveStore{newMemoryTokenStore()}
	client := newOAuthTestClient(t, resource.server.URL, store, browserOpening(t))
	flow, err := client.Prepare(context.Background())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := client.cfg.OpenBrowser(flow.AuthURL); err != nil {
		t.Fatalf("open browser: %v", err)
	}
	status, err := client.Wait(context.Background(), flow)
	if err == nil || oauthErrorCode(t, err) != OAuthErrStorage {
		t.Fatalf("Wait with failing store = %v", err)
	}
	if status.State != OAuthStateAuthorized {
		t.Fatalf("state = %q, want authorized (in-memory token remains usable)", status.State)
	}
}

func TestOAuthConfigValidationAndCorruptStore(t *testing.T) {
	if _, err := NewOAuthClient(OAuthConfig{Resource: "https://example.com/mcp", TokenStore: newMemoryTokenStore()}); err == nil {
		t.Fatal("missing ServerName must fail")
	}
	if _, err := NewOAuthClient(OAuthConfig{ServerName: "x", Resource: "https://example.com/mcp"}); err == nil {
		t.Fatal("missing TokenStore must fail")
	}
	if _, err := NewOAuthClient(OAuthConfig{ServerName: "x", Resource: "http://example.com/mcp", TokenStore: newMemoryTokenStore()}); err == nil {
		t.Fatal("remote http resource must fail")
	}
	if _, err := NewOAuthClient(OAuthConfig{
		ServerName: "x", Resource: "https://example.com/mcp", TokenStore: newMemoryTokenStore(),
		RedirectURI: "http://localhost:1234/cb",
	}); err == nil {
		t.Fatal("non-127.0.0.1 redirect must fail")
	}
	store := newMemoryTokenStore()
	store.values["fake"] = []byte("{not json")
	client := newOAuthTestClient(t, "https://example.com/mcp", store, nil)
	if status := client.Status(); status.State != OAuthStateNone {
		t.Fatalf("corrupt stored token state = %q, want none", status.State)
	}
	if _, err := Start(context.Background(), Config{Transport: TransportStdio, Command: "true", OAuth: client}); err == nil {
		t.Fatal("OAuth with the stdio transport must fail")
	}
}

func TestOAuthDCRRetryWithoutApplicationType(t *testing.T) {
	as := newFakeAS(t)
	resource := newFakeMCP(t, as.server.URL)
	as.setResource(resource.server.URL)
	resource.setExpectedBearer("access-1")
	as.setRequireNoApplicationType(true)
	client := newOAuthTestClient(t, resource.server.URL, newMemoryTokenStore(), browserOpening(t))
	authorizeWithBrowser(t, client)
	if register, _, _, _, _ := as.calls(); register != 2 {
		t.Fatalf("DCR calls = %d, want 2 (first rejected, retry without application_type)", register)
	}
}

func TestOAuthInsufficientScopeCaptured(t *testing.T) {
	as := newFakeAS(t)
	resource := newFakeMCP(t, as.server.URL)
	as.setResource(resource.server.URL)
	resource.setExpectedBearer("access-1")
	client := newOAuthTestClient(t, resource.server.URL, newMemoryTokenStore(), browserOpening(t))
	authorizeWithBrowser(t, client)
	resource.setForbiddenTools(true)
	rigClient, err := StartHTTP(context.Background(), HTTPConfig{URL: resource.server.URL, OAuth: client})
	if err != nil {
		t.Fatalf("StartHTTP: %v", err)
	}
	defer rigClient.Close()
	if _, err := rigClient.ListTools(context.Background()); err == nil {
		t.Fatal("ListTools should fail with 403")
	}
	status := client.Status()
	if status.Err == nil || status.Err.Code != OAuthErrInsufficientScope {
		t.Fatalf("status error = %+v, want insufficient_scope", status.Err)
	}
}

func TestOAuthConcurrentRefreshSingleFlight(t *testing.T) {
	as := newFakeAS(t)
	resource := newFakeMCP(t, as.server.URL)
	as.setResource(resource.server.URL)
	resource.setExpectedBearer("access-1")
	store := newMemoryTokenStore()
	client := newOAuthTestClient(t, resource.server.URL, store, browserOpening(t))
	authorizeWithBrowser(t, client)
	as.setRefreshDelay(100 * time.Millisecond)
	var wait sync.WaitGroup
	start := make(chan struct{})
	wait.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wait.Done()
			<-start
			_, _ = client.Refresh(context.Background())
		}()
	}
	close(start)
	wait.Wait()
	if _, _, _, refresh, _ := as.calls(); refresh != 1 {
		t.Fatalf("concurrent refresh calls = %d, want 1 (single-flight)", refresh)
	}
}

func TestOAuthCancelWakesWaitAndAllowsRetry(t *testing.T) {
	as := newFakeAS(t)
	resource := newFakeMCP(t, as.server.URL)
	as.setResource(resource.server.URL)
	client := newOAuthTestClient(t, resource.server.URL, newMemoryTokenStore(), browserOpening(t))
	flow, err := client.Prepare(context.Background())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	waitDone := make(chan error, 1)
	go func() {
		_, waitErr := client.Wait(context.Background(), flow)
		waitDone <- waitErr
	}()
	time.Sleep(30 * time.Millisecond)
	client.Cancel(flow)
	select {
	case waitErr := <-waitDone:
		if waitErr == nil || oauthErrorCode(t, waitErr) != OAuthErrCallbackTimeout {
			t.Fatalf("Wait after Cancel = %v", waitErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not wake up after Cancel")
	}
	// 取消后可以立即重新准备。
	if _, err := client.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare after Cancel: %v", err)
	}
}

func TestOAuthConcurrentPrepareOnlyOneWins(t *testing.T) {
	as := newFakeAS(t)
	resource := newFakeMCP(t, as.server.URL)
	as.setResource(resource.server.URL)
	client := newOAuthTestClient(t, resource.server.URL, newMemoryTokenStore(), browserOpening(t))
	type result struct {
		flow OAuthFlow
		err  error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			flow, err := client.Prepare(context.Background())
			results <- result{flow: flow, err: err}
		}()
	}
	close(start)
	successes, failures := 0, 0
	var successful OAuthFlow
	for i := 0; i < 2; i++ {
		got := <-results
		if got.err != nil {
			failures++
			continue
		}
		successes++
		successful = got.flow
	}
	if successes != 1 || failures != 1 {
		t.Fatalf("concurrent Prepare: successes=%d failures=%d, want 1/1", successes, failures)
	}
	client.Cancel(successful)
}

func TestOAuthStaleWaitDoesNotClearNewFlow(t *testing.T) {
	as := newFakeAS(t)
	resource := newFakeMCP(t, as.server.URL)
	as.setResource(resource.server.URL)
	client := newOAuthTestClient(t, resource.server.URL, newMemoryTokenStore(), browserOpening(t))
	oldFlow, err := client.Prepare(context.Background())
	if err != nil {
		t.Fatalf("first Prepare: %v", err)
	}
	oldDone := make(chan error, 1)
	go func() {
		_, waitErr := client.Wait(context.Background(), oldFlow)
		oldDone <- waitErr
	}()
	time.Sleep(20 * time.Millisecond)
	client.Cancel(oldFlow)
	newFlow, err := client.Prepare(context.Background())
	if err != nil {
		t.Fatalf("second Prepare: %v", err)
	}
	if err := <-oldDone; err == nil {
		t.Fatal("stale Wait should return an error")
	}
	client.mu.Lock()
	active := client.flow
	client.mu.Unlock()
	if active == nil || active.authURL != newFlow.AuthURL {
		t.Fatalf("stale Wait cleared the new flow: %+v", active)
	}
	client.Cancel(newFlow)
}

func TestOAuthCrossOriginRedirectDoesNotLeakToken(t *testing.T) {
	as := newFakeAS(t)
	resource := newFakeMCP(t, as.server.URL)
	as.setResource(resource.server.URL)
	resource.setExpectedBearer("access-1")
	var attackerAuth []string
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attackerAuth = append(attackerAuth, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	defer attacker.Close()
	resource.setRedirectTo(attacker.URL)
	client := newOAuthTestClient(t, resource.server.URL, newMemoryTokenStore(), browserOpening(t))
	authorizeWithBrowser(t, client)
	if _, err := StartHTTP(context.Background(), HTTPConfig{URL: resource.server.URL, OAuth: client}); err == nil {
		t.Fatal("cross-origin redirect must fail the MCP handshake")
	}
	if len(attackerAuth) != 0 {
		t.Fatalf("attacker received %d request(s) with Authorization %v", len(attackerAuth), attackerAuth)
	}
}

func TestOAuthStartRejectsResourceMismatch(t *testing.T) {
	as := newFakeAS(t)
	resource := newFakeMCP(t, as.server.URL)
	as.setResource(resource.server.URL)
	resource.setExpectedBearer("access-1")
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer other.Close()
	client := newOAuthTestClient(t, resource.server.URL, newMemoryTokenStore(), browserOpening(t))
	authorizeWithBrowser(t, client)
	if _, err := StartHTTP(context.Background(), HTTPConfig{URL: other.URL, OAuth: client}); err == nil {
		t.Fatal("StartHTTP must reject a URL that differs from OAuthConfig.Resource")
	}
}

func TestOAuthReconcileDropsTokenFromDifferentIssuer(t *testing.T) {
	as := newFakeAS(t)
	resource := newFakeMCP(t, as.server.URL)
	as.setResource(resource.server.URL)
	store := newMemoryTokenStore()
	record := oauthTokenRecord{
		Version: 1, Server: "fake", Issuer: "https://old.example", Resource: resource.server.URL,
		Endpoints: oauthEndpoints{Token: as.server.URL + "/token"},
		Client:    oauthClientRecord{ID: "old-client"},
		Token:     oauthToken{AccessToken: "old-token", TokenType: "Bearer"},
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	store.values["fake"] = data
	client := newOAuthTestClient(t, resource.server.URL, store, browserOpening(t))
	if status := client.Status(); status.State != OAuthStateAuthorized {
		t.Fatalf("precondition state = %q", status.State)
	}
	if _, err := client.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if store.has("fake") {
		t.Fatal("a token issued by a different authorization server must be discarded")
	}
}

func TestOAuthRefreshServerErrorKeepsToken(t *testing.T) {
	as := newFakeAS(t)
	resource := newFakeMCP(t, as.server.URL)
	as.setResource(resource.server.URL)
	resource.setExpectedBearer("access-1")
	store := newMemoryTokenStore()
	client := newOAuthTestClient(t, resource.server.URL, store, browserOpening(t))
	authorizeWithBrowser(t, client)
	as.setRefreshServerError(true)
	status, err := client.Refresh(context.Background())
	if err == nil {
		t.Fatal("Refresh with 503 server_error should fail")
	}
	if status.State == OAuthStateExpired {
		t.Fatal("a transient 5xx must not expire the authorization")
	}
	if !store.has("fake") {
		t.Fatal("a transient 5xx must keep the stored refresh token")
	}
}

func TestOAuthRevokeDuringRefreshDoesNotResurrect(t *testing.T) {
	as := newFakeAS(t)
	resource := newFakeMCP(t, as.server.URL)
	as.setResource(resource.server.URL)
	resource.setExpectedBearer("access-1")
	store := newMemoryTokenStore()
	client := newOAuthTestClient(t, resource.server.URL, store, browserOpening(t))
	authorizeWithBrowser(t, client)
	as.setRefreshDelay(150 * time.Millisecond)
	refreshDone := make(chan struct{})
	go func() {
		defer close(refreshDone)
		_, _ = client.Refresh(context.Background())
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, _, _, refresh, _ := as.calls()
		if refresh > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("refresh request did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := client.Revoke(context.Background()); err != nil {
		t.Fatalf("Revoke during refresh: %v", err)
	}
	<-refreshDone
	if store.has("fake") {
		t.Fatal("revocation must win over an in-flight refresh")
	}
	if status := client.Status(); status.State != OAuthStateNone {
		t.Fatalf("state after revoke = %q, want none", status.State)
	}
}

func TestOAuthParsingHelpers(t *testing.T) {
	challenge := parseWWWAuthenticate([]string{
		`Bearer realm="mcp", resource_metadata="https://example.com/.well-known/oauth-protected-resource", scope="files:read files:write", error="insufficient_scope"`,
	})
	if challenge == nil {
		t.Fatal("challenge was not parsed")
	}
	if challenge.ResourceMetadata != "https://example.com/.well-known/oauth-protected-resource" {
		t.Fatalf("resource_metadata = %q", challenge.ResourceMetadata)
	}
	if challenge.Scope != "files:read files:write" || challenge.Error != "insufficient_scope" {
		t.Fatalf("challenge = %+v", challenge)
	}

	canonical, err := canonicalResource("HTTPS://Example.COM:443/")
	if err != nil || canonical != "https://example.com:443" {
		t.Fatalf("canonicalResource = %q, %v", canonical, err)
	}
	if _, err := canonicalResource("http://example.com/mcp"); err == nil {
		t.Fatal("remote http resource must be rejected")
	}
	if _, err := parseLoopbackRedirect("http://localhost:1234/cb"); err == nil {
		t.Fatal("non-127.0.0.1 loopback redirect must be rejected")
	}
	if _, err := parseLoopbackRedirect("http://127.0.0.1:1234/cb"); err != nil {
		t.Fatalf("valid loopback redirect rejected: %v", err)
	}
	if !tokenNearExpiry(time.Now().Add(10*time.Second), time.Now()) {
		t.Fatal("token inside the refresh window must be near expiry")
	}
	if tokenNearExpiry(time.Now().Add(10*time.Minute), time.Now()) {
		t.Fatal("token outside the refresh window must not be near expiry")
	}
}
