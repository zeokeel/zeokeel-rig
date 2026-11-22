// Package auth owns the host-agnostic OAuth state of MCP servers: a refreshable
// client cache (rebuilt when its configuration fingerprint changes), status
// snapshots for UI surfaces, the interactive Prepare/Wait/Cancel flow, token
// revocation, and an optional background refresh loop that rotates tokens only
// shortly before they expire.
//
// The protocol implementation stays in the parent tools/mcp package
// (mcp.OAuthClient); this package adds the lifecycle a host needs around it and
// a file-backed mcp.TokenStore so a desktop client can persist tokens without
// writing its own settings adapter. It is safe for concurrent use.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/zeokeel/zeokeel-rig/tools/mcp"
)

const (
	// DefaultRefreshInterval is the background loop cadence when no option is
	// set (the product refreshes every 30s).
	DefaultRefreshInterval = 30 * time.Second
	// DefaultRefreshWindow is how close to expiry a token must be for the
	// background loop to refresh it (the product uses 5 minutes).
	DefaultRefreshWindow = 5 * time.Minute
)

// ErrClosed reports Runtime operations after Close.
var ErrClosed = errors.New("mcp/auth: runtime is closed")

// Config is one MCP server's OAuth configuration. It mirrors mcp.OAuthConfig so
// hosts do not have to import the protocol package for the common case.
type Config struct {
	// ServerName is the stable identity used by the client cache and TokenStore.
	ServerName string
	// Resource is the MCP server's canonical URI (RFC 8707).
	Resource string
	// Scopes are the requested scopes (empty = server metadata).
	Scopes []string
	// AuthorizationServer optionally overrides the discovered AS.
	AuthorizationServer string
	// ClientID/ClientSecret optionally pin a pre-registered client.
	ClientID     string
	ClientSecret string
	// RedirectURI optionally pins the loopback redirect.
	RedirectURI string
	// OpenBrowser opens the authorization URL on the host UI thread (nil makes
	// interactive authorization unavailable; stored tokens can still refresh).
	OpenBrowser func(authURL string) error
	// TokenStore persists the opaque token record (required).
	TokenStore mcp.TokenStore
	// HTTPClient optionally overrides discovery/registration/token requests.
	HTTPClient *http.Client
	// Now optionally replaces the clock (tests).
	Now func() time.Time
	// Logf receives diagnostics (never token material).
	Logf func(format string, args ...any)
	// Timeout bounds one discovery/registration/token request (0 = 30s,
	// negative disables the client-side timeout).
	Timeout time.Duration
}

// Option configures a Runtime.
type Option func(*Runtime)

// WithOnAuthorized installs the callback invoked after a completed interactive
// authorization (asynchronously, with the server name). The manager uses it to
// reconnect the server.
func WithOnAuthorized(callback func(server string)) Option {
	return func(runtime *Runtime) {
		if callback != nil {
			runtime.onAuthorized = callback
		}
	}
}

// WithLogger installs the runtime diagnostic logger (nil discards).
func WithLogger(logf func(format string, args ...any)) Option {
	return func(runtime *Runtime) {
		if logf != nil {
			runtime.logf = logf
		}
	}
}

// RefreshInterval sets the background loop cadence (non-positive keeps the
// default).
func RefreshInterval(interval time.Duration) Option {
	return func(runtime *Runtime) {
		if interval > 0 {
			runtime.interval = interval
		}
	}
}

// RefreshWindow sets how close to expiry a token must be for a background
// refresh (non-positive keeps the default).
func RefreshWindow(window time.Duration) Option {
	return func(runtime *Runtime) {
		if window > 0 {
			runtime.window = window
		}
	}
}

// clientEntry is one cached OAuth client plus the configuration fingerprint it
// was built from.
type clientEntry struct {
	client      *mcp.OAuthClient
	fingerprint string
	now         func() time.Time
}

// Runtime manages the OAuth clients of one host process. The zero value is not
// usable; call New.
type Runtime struct {
	mu           sync.Mutex
	clients      map[string]*clientEntry
	onAuthorized func(server string)
	logf         func(format string, args ...any)
	interval     time.Duration
	window       time.Duration
	stop         chan struct{}
	done         chan struct{}
	started      bool
	closed       bool
}

// New returns an empty runtime. Clients are built lazily by Client (and by the
// manager during startup).
func New(opts ...Option) *Runtime {
	runtime := &Runtime{
		clients:  make(map[string]*clientEntry),
		logf:     func(string, ...any) {},
		interval: DefaultRefreshInterval,
		window:   DefaultRefreshWindow,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(runtime)
		}
	}
	return runtime
}

// Client returns the cached OAuth client for cfg, rebuilding it when the
// configuration fingerprint changed. The replaced client is closed.
func (r *Runtime) Client(cfg Config) (*mcp.OAuthClient, error) {
	if r == nil {
		return nil, errors.New("mcp/auth: nil runtime")
	}
	cfg.ServerName = strings.TrimSpace(cfg.ServerName)
	if cfg.ServerName == "" {
		return nil, errors.New("mcp/auth: ServerName is required")
	}
	if strings.TrimSpace(cfg.Resource) == "" {
		return nil, errors.New("mcp/auth: Resource is required")
	}
	if cfg.TokenStore == nil {
		return nil, errors.New("mcp/auth: TokenStore is required")
	}
	fingerprint := fingerprint(cfg)
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, ErrClosed
	}
	if entry := r.clients[cfg.ServerName]; entry != nil && entry.fingerprint == fingerprint {
		client := entry.client
		r.mu.Unlock()
		return client, nil
	}
	r.mu.Unlock()

	client, err := mcp.NewOAuthClient(mcp.OAuthConfig{
		ServerName:          cfg.ServerName,
		Resource:            cfg.Resource,
		Scopes:              append([]string(nil), cfg.Scopes...),
		AuthorizationServer: cfg.AuthorizationServer,
		ClientID:            cfg.ClientID,
		ClientSecret:        cfg.ClientSecret,
		RedirectURI:         cfg.RedirectURI,
		OpenBrowser:         cfg.OpenBrowser,
		TokenStore:          cfg.TokenStore,
		HTTPClient:          cfg.HTTPClient,
		Now:                 cfg.Now,
		Logf:                cfg.Logf,
		Timeout:             cfg.Timeout,
	})
	if err != nil {
		return nil, fmt.Errorf("mcp/auth: %q: %w", cfg.ServerName, err)
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		_ = client.Close()
		return nil, ErrClosed
	}
	// Another goroutine may have built the same fingerprint while we were
	// connecting: keep one shared client and close the loser.
	if entry := r.clients[cfg.ServerName]; entry != nil && entry.fingerprint == fingerprint {
		existing := entry.client
		r.mu.Unlock()
		_ = client.Close()
		return existing, nil
	}
	previous := r.clients[cfg.ServerName]
	r.clients[cfg.ServerName] = &clientEntry{client: client, fingerprint: fingerprint, now: now}
	r.mu.Unlock()
	if previous != nil && previous.client != nil {
		_ = previous.client.Close()
	}
	return client, nil
}

// Status returns the cached client's authorization status without any I/O. A
// configuration that was never built (or whose fingerprint changed) reports
// OAuthStateNone, matching the product's UI-thread-safe read contract.
func (r *Runtime) Status(cfg Config) mcp.OAuthStatus {
	if r == nil {
		return mcp.OAuthStatus{State: mcp.OAuthStateNone}
	}
	cfg.ServerName = strings.TrimSpace(cfg.ServerName)
	if cfg.ServerName == "" {
		return mcp.OAuthStatus{State: mcp.OAuthStateNone}
	}
	fingerprint := fingerprint(cfg)
	r.mu.Lock()
	entry := r.clients[cfg.ServerName]
	r.mu.Unlock()
	if entry == nil || entry.fingerprint != fingerprint {
		return mcp.OAuthStatus{State: mcp.OAuthStateNone}
	}
	return entry.client.Status()
}

// Prepare starts an interactive authorization flow (discovery + registration +
// PKCE + loopback listener). The caller opens flow.AuthURL and then calls Wait.
func (r *Runtime) Prepare(ctx context.Context, cfg Config) (mcp.OAuthFlow, error) {
	client, err := r.Client(cfg)
	if err != nil {
		return mcp.OAuthFlow{}, err
	}
	return client.Prepare(ctx)
}

// Wait completes an interactive flow and, on success, invokes the authorized
// callback asynchronously with the server name.
func (r *Runtime) Wait(ctx context.Context, cfg Config, flow mcp.OAuthFlow) (mcp.OAuthStatus, error) {
	client, err := r.Client(cfg)
	if err != nil {
		return mcp.OAuthStatus{}, err
	}
	status, err := client.Wait(ctx, flow)
	if err != nil {
		return status, err
	}
	if r.onAuthorized != nil {
		name := strings.TrimSpace(cfg.ServerName)
		go func() { r.onAuthorized(name) }()
	}
	return status, nil
}

// Cancel aborts one prepared flow. It never builds a client (a cancel for a
// client that was never created is a no-op).
func (r *Runtime) Cancel(cfg Config, flow mcp.OAuthFlow) {
	client := r.cachedClient(cfg)
	if client == nil {
		return
	}
	client.Cancel(flow)
}

// Revoke deletes the stored token (and asks the server to revoke it).
func (r *Runtime) Revoke(ctx context.Context, cfg Config) error {
	client, err := r.Client(cfg)
	if err != nil {
		return err
	}
	return client.Revoke(ctx)
}

// RefreshDue refreshes every authorized client whose access token expires
// within window (non-positive window uses the configured RefreshWindow). It
// returns the number of successfully refreshed clients.
func (r *Runtime) RefreshDue(ctx context.Context, window time.Duration) int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	if window <= 0 {
		window = r.window
	}
	entries := make([]*clientEntry, 0, len(r.clients))
	for _, entry := range r.clients {
		entries = append(entries, entry)
	}
	logf := r.logf
	r.mu.Unlock()
	if window <= 0 {
		window = DefaultRefreshWindow
	}
	refreshed := 0
	for _, entry := range entries {
		if entry == nil || entry.client == nil {
			continue
		}
		status := entry.client.Status()
		if status.State != mcp.OAuthStateAuthorized || status.ExpiresAt.IsZero() {
			continue
		}
		now := time.Now
		if entry.now != nil {
			now = entry.now
		}
		if status.ExpiresAt.After(now().Add(window)) {
			continue
		}
		if _, err := entry.client.Refresh(ctx); err != nil {
			logf("mcp/auth: background refresh failed: %v", err)
			continue
		}
		refreshed++
	}
	return refreshed
}

// Start launches the background refresh loop. It is idempotent; Stop ends it.
func (r *Runtime) Start() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.started || r.closed {
		r.mu.Unlock()
		return
	}
	r.started = true
	stop := make(chan struct{})
	done := make(chan struct{})
	r.stop = stop
	r.done = done
	interval := r.interval
	r.mu.Unlock()
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				r.RefreshDue(context.Background(), 0)
			}
		}
	}()
}

// Stop ends the background loop (idempotent). Cached clients stay open.
func (r *Runtime) Stop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if !r.started {
		r.mu.Unlock()
		return
	}
	stop, done := r.stop, r.done
	r.started = false
	r.stop = nil
	r.done = nil
	r.mu.Unlock()
	close(stop)
	if done != nil {
		<-done
	}
}

// Close stops the loop and closes every cached client (idempotent).
func (r *Runtime) Close() error {
	if r == nil {
		return nil
	}
	r.Stop()
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	clients := r.clients
	r.clients = make(map[string]*clientEntry)
	r.mu.Unlock()
	for _, entry := range clients {
		if entry != nil && entry.client != nil {
			_ = entry.client.Close()
		}
	}
	return nil
}

// OnAuthorized returns the installed authorized callback (diagnostics/tests).
func (r *Runtime) OnAuthorized() func(server string) {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.onAuthorized
}

// cachedClient returns the cached client whose fingerprint still matches cfg.
func (r *Runtime) cachedClient(cfg Config) *mcp.OAuthClient {
	if r == nil {
		return nil
	}
	cfg.ServerName = strings.TrimSpace(cfg.ServerName)
	if cfg.ServerName == "" {
		return nil
	}
	fingerprint := fingerprint(cfg)
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := r.clients[cfg.ServerName]
	if entry == nil || entry.fingerprint != fingerprint {
		return nil
	}
	return entry.client
}

// fingerprint changes whenever a cached client must be rebuilt.
func fingerprint(cfg Config) string {
	return strings.Join([]string{
		strings.TrimSpace(cfg.ServerName),
		strings.TrimSpace(cfg.Resource),
		strings.Join(cfg.Scopes, "\x00"),
		strings.TrimSpace(cfg.AuthorizationServer),
		strings.TrimSpace(cfg.ClientID),
		strings.TrimSpace(cfg.ClientSecret),
		strings.TrimSpace(cfg.RedirectURI),
		fmt.Sprintf("%d", cfg.Timeout),
		fmt.Sprintf("%T", cfg.TokenStore),
		fmt.Sprintf("%T:%p", cfg.HTTPClient, cfg.HTTPClient),
	}, "\x01")
}
