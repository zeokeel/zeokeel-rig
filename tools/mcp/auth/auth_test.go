package auth_test

// Tests for the MCP OAuth runtime: file token store, client cache/fingerprint
// rebuilds, status snapshots, the interactive Prepare/Wait flow against a fake
// authorization server, background refresh, revoke and close.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/tools/mcp"
	"github.com/zeokeel/zeokeel-rig/tools/mcp/auth"
)

// writeJSON writes one JSON response.
func writeJSON(t *testing.T, w http.ResponseWriter, status int, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

// fakeAS is a minimal OAuth 2.1 authorization server: metadata, dynamic
// registration, authorization redirect, token exchange and revocation.
type fakeAS struct {
	t      *testing.T
	server *httptest.Server

	mu           sync.Mutex
	expiresIn    int
	accessToken  string
	refreshToken string
	registerID   string
	authorizes   int
	tokens       int
	refreshes    int
	revokes      int
}

func newFakeAS(t *testing.T) *fakeAS {
	t.Helper()
	as := &fakeAS{
		t:            t,
		expiresIn:    30,
		accessToken:  "access-1",
		refreshToken: "refresh-1",
		registerID:   "dcr-client",
	}
	as.server = httptest.NewServer(http.HandlerFunc(as.handle))
	t.Cleanup(as.server.Close)
	return as
}

func (a *fakeAS) handle(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/.well-known/oauth-authorization-server", "/.well-known/openid-configuration":
		writeJSON(a.t, w, http.StatusOK, map[string]any{
			"issuer":                                         a.server.URL,
			"authorization_endpoint":                         a.server.URL + "/authorize",
			"token_endpoint":                                 a.server.URL + "/token",
			"registration_endpoint":                          a.server.URL + "/register",
			"revocation_endpoint":                            a.server.URL + "/revoke",
			"scopes_supported":                               []string{"files:read"},
			"code_challenge_methods_supported":               []string{"S256"},
			"authorization_response_iss_parameter_supported": true,
		})
	case "/register":
		writeJSON(a.t, w, http.StatusOK, map[string]any{
			"client_id": a.registerID, "token_endpoint_auth_method": "none",
		})
	case "/authorize":
		query := r.URL.Query()
		a.mu.Lock()
		a.authorizes++
		a.mu.Unlock()
		target, err := r.URL.Parse(query.Get("redirect_uri"))
		if err != nil {
			http.Error(w, "bad redirect_uri", http.StatusBadRequest)
			return
		}
		values := target.Query()
		values.Set("code", "auth-code-1")
		values.Set("state", query.Get("state"))
		values.Set("iss", a.server.URL)
		target.RawQuery = values.Encode()
		http.Redirect(w, r, target.String(), http.StatusFound)
	case "/token":
		if err := r.ParseForm(); err != nil {
			writeJSON(a.t, w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		switch r.PostForm.Get("grant_type") {
		case "authorization_code":
			a.tokens++
			verifier := r.PostForm.Get("code_verifier")
			sum := sha256.Sum256([]byte(verifier))
			if r.PostForm.Get("code") != "auth-code-1" || base64.RawURLEncoding.EncodeToString(sum[:]) == "" {
				writeJSON(a.t, w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
				return
			}
		case "refresh_token":
			a.refreshes++
			if r.PostForm.Get("refresh_token") != a.refreshToken {
				writeJSON(a.t, w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
				return
			}
			a.accessToken = "access-2"
			a.refreshToken = "refresh-2"
		default:
			writeJSON(a.t, w, http.StatusBadRequest, map[string]string{"error": "unsupported_grant_type"})
			return
		}
		writeJSON(a.t, w, http.StatusOK, map[string]any{
			"access_token":  a.accessToken,
			"token_type":    "Bearer",
			"expires_in":    a.expiresIn,
			"refresh_token": a.refreshToken,
			"scope":         "files:read",
		})
	case "/revoke":
		a.mu.Lock()
		a.revokes++
		a.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	default:
		http.NotFound(w, r)
	}
}

// counters returns the observed AS call counts.
func (a *fakeAS) counters() (authorizes, tokens, refreshes, revokes int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.authorizes, a.tokens, a.refreshes, a.revokes
}

func (a *fakeAS) setExpiresIn(seconds int) {
	a.mu.Lock()
	a.expiresIn = seconds
	a.mu.Unlock()
}

// fakeResource serves RFC 9728 protected-resource metadata for one MCP URL.
type fakeResource struct {
	server *httptest.Server
	asURL  string
}

func newFakeResource(t *testing.T, asURL string) *fakeResource {
	t.Helper()
	resource := &fakeResource{asURL: asURL}
	resource.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/.well-known/oauth-protected-resource") {
			http.NotFound(w, r)
			return
		}
		writeJSON(t, w, http.StatusOK, map[string]any{
			"resource":              resource.URL() + "/mcp",
			"authorization_servers": []string{asURL},
			"scopes_supported":      []string{"files:read"},
		})
	}))
	t.Cleanup(resource.server.Close)
	return resource
}

func (r *fakeResource) URL() string { return r.server.URL }

// oauthConfig returns the runtime config used by the flow tests.
func oauthConfig(t *testing.T, resource *fakeResource, store mcp.TokenStore, openBrowser func(string) error) auth.Config {
	t.Helper()
	return auth.Config{
		ServerName:  "files",
		Resource:    resource.URL() + "/mcp",
		Scopes:      []string{"files:read"},
		OpenBrowser: openBrowser,
		TokenStore:  store,
		Timeout:     5 * time.Second,
	}
}

// authorize runs Prepare + Wait against the fake AS. The browser opener performs
// the authorization GET (which follows the AS redirect back to the loopback
// listener), so the flow completes without a UI.
func authorize(t *testing.T, runtime *auth.Runtime, cfg auth.Config) mcp.OAuthStatus {
	t.Helper()
	ctx := context.Background()
	flow, err := runtime.Prepare(ctx, cfg)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if flow.AuthURL == "" || flow.RedirectURI == "" {
		t.Fatalf("flow = %+v, want auth URL and redirect URI", flow)
	}
	type result struct {
		status mcp.OAuthStatus
		err    error
	}
	done := make(chan result, 1)
	go func() {
		status, err := runtime.Wait(ctx, cfg, flow)
		done <- result{status: status, err: err}
	}()
	// The callback page may be torn down (EOF) the instant Wait receives the
	// code and closes its loopback listener; the Wait result is authoritative.
	if response, err := http.Get(flow.AuthURL); err == nil {
		_ = response.Body.Close()
	}
	select {
	case outcome := <-done:
		if outcome.err != nil {
			t.Fatalf("Wait: %v", outcome.err)
		}
		return outcome.status
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not finish")
		return mcp.OAuthStatus{}
	}
}

func TestFileTokenStoreRoundTripPermissionsAndDelete(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested")
	path := filepath.Join(dir, "tokens.json")
	store := auth.FileTokenStore(path)
	ctx := context.Background()

	if data, found, err := store.Load(ctx, "alpha"); err != nil || found || data != nil {
		t.Fatalf("Load(empty) = %q/%v/%v", data, found, err)
	}
	first := []byte(`{"version":1,"token":"one"}`)
	second := []byte(`{"version":1,"token":"two"}`)
	if err := store.Save(ctx, "alpha", first); err != nil {
		t.Fatalf("Save(alpha): %v", err)
	}
	if err := store.Save(ctx, "beta", second); err != nil {
		t.Fatalf("Save(beta): %v", err)
	}
	if data, found, err := store.Load(ctx, "alpha"); err != nil || !found || string(data) != string(first) {
		t.Fatalf("Load(alpha) = %q/%v/%v", data, found, err)
	}
	// The returned bytes are a copy.
	data, _, _ := store.Load(ctx, "alpha")
	data[0] = 'X'
	if again, _, _ := store.Load(ctx, "alpha"); string(again) != string(first) {
		t.Fatal("Load exposed the stored bytes")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat tokens: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("token file mode = %o, want 600", got)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat token dir: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("token dir mode = %o, want 700", got)
	}
	temporaries, err := filepath.Glob(filepath.Join(dir, ".tokens-*"))
	if err != nil || len(temporaries) != 0 {
		t.Fatalf("temporaries = %v/%v, want none", temporaries, err)
	}

	if err := store.Delete(ctx, "alpha"); err != nil {
		t.Fatalf("Delete(alpha): %v", err)
	}
	if _, found, _ := store.Load(ctx, "alpha"); found {
		t.Fatal("alpha survived Delete")
	}
	if data, found, err := store.Load(ctx, "beta"); err != nil || !found || string(data) != string(second) {
		t.Fatalf("Load(beta) after delete = %q/%v/%v", data, found, err)
	}
	if err := store.Delete(ctx, "missing"); err != nil {
		t.Fatalf("Delete(missing): %v", err)
	}

	corrupt := filepath.Join(t.TempDir(), "corrupt.json")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write corrupt store: %v", err)
	}
	if _, _, err := auth.FileTokenStore(corrupt).Load(ctx, "alpha"); err == nil {
		t.Fatal("Load(corrupt) returned nil error")
	}
}

func TestRuntimeClientCacheAndFingerprintRebuild(t *testing.T) {
	runtime := auth.New()
	cfg := auth.Config{
		ServerName: "files",
		Resource:   "http://127.0.0.1:9/mcp",
		TokenStore: auth.FileTokenStore(filepath.Join(t.TempDir(), "tokens.json")),
	}
	first, err := runtime.Client(cfg)
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	again, err := runtime.Client(cfg)
	if err != nil || again != first {
		t.Fatalf("cached Client = %p/%v, want the same client %p", again, err, first)
	}

	changed := cfg
	changed.ClientID = "pre-registered-2"
	rebuilt, err := runtime.Client(changed)
	if err != nil {
		t.Fatalf("Client(changed): %v", err)
	}
	if rebuilt == first {
		t.Fatal("changed fingerprint reused the old client")
	}
	if _, err := first.Prepare(context.Background()); err == nil {
		t.Fatal("replaced client still accepts Prepare")
	}
	// Status is a pure read: a config that was never built reports none.
	other := cfg
	other.ClientID = "never-built"
	if status := runtime.Status(other); status.State != mcp.OAuthStateNone {
		t.Fatalf("Status(never built) = %+v, want none", status)
	}
}

func TestRuntimeValidation(t *testing.T) {
	runtime := auth.New()
	store := auth.FileTokenStore(filepath.Join(t.TempDir(), "tokens.json"))
	for name, cfg := range map[string]auth.Config{
		"no server": {Resource: "http://127.0.0.1:9/mcp", TokenStore: store},
		"no resource": {
			ServerName: "files", TokenStore: store,
		},
		"no store": {ServerName: "files", Resource: "http://127.0.0.1:9/mcp"},
	} {
		if _, err := runtime.Client(cfg); err == nil {
			t.Fatalf("Client(%s) err = nil, want an error", name)
		}
	}
}

func TestRuntimeAuthorizationFlowPersistsAndNotifies(t *testing.T) {
	as := newFakeAS(t)
	resource := newFakeResource(t, as.server.URL)
	store := auth.FileTokenStore(filepath.Join(t.TempDir(), "tokens.json"))
	authorized := make(chan string, 1)
	runtime := auth.New(auth.WithOnAuthorized(func(server string) { authorized <- server }))
	cfg := oauthConfig(t, resource, store, func(string) error { return nil })
	defer func() { _ = runtime.Close() }()

	if status := runtime.Status(cfg); status.State != mcp.OAuthStateNone {
		t.Fatalf("initial Status = %+v, want none", status)
	}
	status := authorize(t, runtime, cfg)
	if status.State != mcp.OAuthStateAuthorized {
		t.Fatalf("Wait status = %+v, want authorized", status)
	}
	if status.ExpiresAt.IsZero() {
		t.Fatal("authorized status has no expiry")
	}
	select {
	case server := <-authorized:
		if server != "files" {
			t.Fatalf("OnAuthorized(%q), want files", server)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnAuthorized was not called")
	}
	if _, found, err := store.Load(context.Background(), "files"); err != nil || !found {
		t.Fatalf("token not persisted: found=%v err=%v", found, err)
	}
	if _, tokens, _, _ := as.counters(); tokens == 0 {
		t.Fatal("token endpoint was not called")
	}
	if status := runtime.Status(cfg); status.State != mcp.OAuthStateAuthorized {
		t.Fatalf("Status after flow = %+v, want authorized", status)
	}
}

func TestRuntimeCancelAbortsPreparedFlow(t *testing.T) {
	as := newFakeAS(t)
	resource := newFakeResource(t, as.server.URL)
	runtime := auth.New()
	cfg := oauthConfig(t, resource, auth.FileTokenStore(filepath.Join(t.TempDir(), "tokens.json")), nil)
	defer func() { _ = runtime.Close() }()

	flow, err := runtime.Prepare(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	waitDone := make(chan error, 1)
	go func() {
		_, waitErr := runtime.Wait(context.Background(), cfg, flow)
		waitDone <- waitErr
	}()
	runtime.Cancel(cfg, flow)
	select {
	case err := <-waitDone:
		if err == nil {
			t.Fatal("Wait after Cancel returned nil error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Wait did not observe the cancellation")
	}
	// Cancelling an unknown flow is a no-op.
	runtime.Cancel(cfg, flow)
}

func TestRuntimeRefreshDueOnlyNearExpiry(t *testing.T) {
	as := newFakeAS(t)
	resource := newFakeResource(t, as.server.URL)
	runtime := auth.New()
	cfg := oauthConfig(t, resource, auth.FileTokenStore(filepath.Join(t.TempDir(), "tokens.json")), nil)
	defer func() { _ = runtime.Close() }()
	authorize(t, runtime, cfg)
	before := func() int { _, _, refreshes, _ := as.counters(); return refreshes }

	if refreshed := runtime.RefreshDue(context.Background(), time.Nanosecond); refreshed != 0 {
		t.Fatalf("RefreshDue(1ns) = %d, want 0 (token is 30s away)", refreshed)
	}
	if got := before(); got != 0 {
		t.Fatalf("refresh calls after far-window check = %d, want 0", got)
	}
	if refreshed := runtime.RefreshDue(context.Background(), time.Minute); refreshed != 1 {
		t.Fatalf("RefreshDue(1m) = %d, want 1", refreshed)
	}
	if got := before(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
	if status := runtime.Status(cfg); status.State != mcp.OAuthStateAuthorized {
		t.Fatalf("Status after refresh = %+v, want authorized", status)
	}
}

func TestRuntimeBackgroundRefreshOnlyNearExpiry(t *testing.T) {
	as := newFakeAS(t)
	resource := newFakeResource(t, as.server.URL)
	runtime := auth.New(auth.RefreshInterval(20*time.Millisecond), auth.RefreshWindow(time.Hour))
	cfg := oauthConfig(t, resource, auth.FileTokenStore(filepath.Join(t.TempDir(), "tokens.json")), nil)
	defer func() { _ = runtime.Close() }()
	authorize(t, runtime, cfg)

	runtime.Start()
	runtime.Start() // idempotent
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, _, refreshes, _ := as.counters()
		if refreshes > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background refresh never ran")
		}
		time.Sleep(10 * time.Millisecond)
	}
	runtime.Stop()
	runtime.Stop() // idempotent
}

func TestRuntimeRevokeClearsTokenAndStatus(t *testing.T) {
	as := newFakeAS(t)
	resource := newFakeResource(t, as.server.URL)
	store := auth.FileTokenStore(filepath.Join(t.TempDir(), "tokens.json"))
	runtime := auth.New()
	cfg := oauthConfig(t, resource, store, nil)
	defer func() { _ = runtime.Close() }()
	authorize(t, runtime, cfg)

	if err := runtime.Revoke(context.Background(), cfg); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if status := runtime.Status(cfg); status.State != mcp.OAuthStateNone {
		t.Fatalf("Status after revoke = %+v, want none", status)
	}
	if _, found, _ := store.Load(context.Background(), "files"); found {
		t.Fatal("revoked token still persisted")
	}
	if _, _, _, revokes := as.counters(); revokes == 0 {
		t.Fatal("revocation endpoint was not called")
	}
}

func TestRuntimeClose(t *testing.T) {
	runtime := auth.New()
	cfg := auth.Config{
		ServerName: "files",
		Resource:   "http://127.0.0.1:9/mcp",
		TokenStore: auth.FileTokenStore(filepath.Join(t.TempDir(), "tokens.json")),
	}
	client, err := runtime.Client(cfg)
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := runtime.Client(cfg); !errors.Is(err, auth.ErrClosed) {
		t.Fatalf("Client after Close = %v, want ErrClosed", err)
	}
	if _, err := client.Prepare(context.Background()); err == nil {
		t.Fatal("closed client still accepts Prepare")
	}
}
