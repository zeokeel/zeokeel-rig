package manager_test

// Tests for the additive OAuth integration: Server.OAuth builds its client via
// the auth runtime, AuthStatus/RefreshAuth expose it, an authorization
// completion reconnects the server, and an externally supplied runtime is not
// closed by the manager.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/tools/mcp"
	"github.com/zeokeel/zeokeel-rig/tools/mcp/auth"
	"github.com/zeokeel/zeokeel-rig/tools/mcp/manager"
)

// plainMCPHTTPServer is a minimal Streamable-HTTP MCP server that ignores
// Authorization: enough to prove the manager built and used an OAuth client.
func plainMCPHTTPServer(t *testing.T, name string) *httptest.Server {
	t.Helper()
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		calls.Add(1)
		var request struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if request.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch request.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": "2025-06-18",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": name, "version": "1.0.0"},
			}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{
				"name": "echo", "description": "Echo", "inputSchema": map[string]any{"type": "object"},
			}}}
		case "tools/call":
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "pong"}}}
		case "ping":
			result = map[string]any{}
		default:
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}))
	t.Cleanup(server.Close)
	return server
}

// oauthServer returns a manager.Server using the plain HTTP endpoint with an
// OAuth client built by the runtime.
func oauthServer(t *testing.T, name string, server *httptest.Server, tokenStore mcp.TokenStore) manager.Server {
	t.Helper()
	return manager.Server{
		Name: name,
		Config: mcp.Config{
			Transport: mcp.TransportHTTP,
			URL:       server.URL,
		},
		OAuth: &auth.Config{
			TokenStore: tokenStore,
			Timeout:    5 * time.Second,
		},
	}
}

// TestServerOAuthBuildsClientViaRuntime: a Server.OAuth row starts through the
// shared auth runtime and exposes an OAuth status snapshot.
func TestServerOAuthBuildsClientViaRuntime(t *testing.T) {
	server := plainMCPHTTPServer(t, "alpha")
	store := auth.FileTokenStore(filepath.Join(t.TempDir(), "tokens.json"))
	m := startManager(t, manager.Config{
		Servers: []manager.Server{oauthServer(t, "alpha", server, store)},
	})
	if m.Auth() == nil {
		t.Fatal("manager has no auth runtime")
	}
	status, ok := m.AuthStatus("alpha")
	if !ok {
		t.Fatal("AuthStatus(alpha) = not configured, want an OAuth status")
	}
	if status.State == mcp.OAuthStateAuthorized {
		t.Fatalf("unexpected authorized status without a token: %+v", status)
	}
	if got := invokeText(t, toolByName(t, m.Tools(), "mcp__alpha__echo"), `{}`); got != "pong" {
		t.Fatalf("echo = %q", got)
	}
}

// TestAuthStatusWithoutOAuth: non-OAuth servers report no auth status and a
// zero refresh count.
func TestAuthStatusWithoutOAuth(t *testing.T) {
	m := startManager(t, manager.Config{
		Servers: []manager.Server{{Name: "alpha", Config: fakeConfig("normal", "alpha")}},
	})
	if status, ok := m.AuthStatus("alpha"); ok {
		t.Fatalf("AuthStatus = %+v/%v, want not configured", status, ok)
	}
	if refreshed := m.RefreshAuth(context.Background()); refreshed != 0 {
		t.Fatalf("RefreshAuth = %d, want 0", refreshed)
	}
}

// TestOAuthConfigFailureIsIsolated: an invalid OAuth config fails only its own
// server; the healthy sibling starts and stays usable.
func TestOAuthConfigFailureIsIsolated(t *testing.T) {
	server := plainMCPHTTPServer(t, "beta")
	m := startManager(t, manager.Config{
		Servers: []manager.Server{
			{Name: "alpha", Config: fakeConfig("normal", "alpha")},
			{
				Name: "beta",
				Config: mcp.Config{
					Transport: mcp.TransportHTTP,
					URL:       server.URL,
				},
				OAuth: &auth.Config{}, // no TokenStore
			},
		},
	})
	if m.Clients()["beta"] != nil {
		t.Fatal("broken OAuth server has a live client")
	}
	if _, ok := m.Errors()["beta"]; !ok {
		t.Fatalf("Errors = %v, want a beta entry", m.Errors())
	}
	if got := invokeText(t, toolByName(t, m.Tools(), "mcp__alpha__echo"), `{"text":"ok"}`); got != "echo:normal:ok" {
		t.Fatalf("healthy server = %q", got)
	}
	if _, ok := m.AuthStatus("beta"); !ok {
		t.Fatal("OAuth-configured beta should still report an auth status")
	}
}

// TestAuthorizedReconnectsServerAndNotifies: the authorization completion hook
// rebuilds the server connection and calls the host callback.
func TestAuthorizedReconnectsServerAndNotifies(t *testing.T) {
	server := plainMCPHTTPServer(t, "alpha")
	notified := make(chan string, 1)
	m := startManager(t, manager.Config{
		Servers:      []manager.Server{oauthServer(t, "alpha", server, auth.FileTokenStore(filepath.Join(t.TempDir(), "tokens.json")))},
		OnAuthorized: func(server string) { notified <- server },
	})
	first := m.Clients()["alpha"]
	if err := m.Authorized("alpha"); err != nil {
		t.Fatalf("Authorized: %v", err)
	}
	second := m.Clients()["alpha"]
	if second == nil || second == first {
		t.Fatalf("Authorized did not reconnect (first=%p second=%p)", first, second)
	}
	select {
	case name := <-notified:
		if name != "alpha" {
			t.Fatalf("OnAuthorized(%q), want alpha", name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnAuthorized was not called")
	}
	if err := m.Authorized("missing"); !errors.Is(err, manager.ErrUnknownServer) {
		t.Fatalf("Authorized(missing) = %v, want ErrUnknownServer", err)
	}
}

// TestExternalAuthRuntimeNotClosedByManager: a host-supplied runtime outlives the
// manager (the host owns its lifecycle).
func TestExternalAuthRuntimeNotClosedByManager(t *testing.T) {
	runtime := auth.New()
	store := auth.FileTokenStore(filepath.Join(t.TempDir(), "tokens.json"))
	m := manager.New(manager.Config{
		Auth:    runtime,
		Servers: []manager.Server{{Name: "alpha", Config: fakeConfig("normal", "alpha")}},
	})
	if m.Auth() != runtime {
		t.Fatal("manager did not use the supplied auth runtime")
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	cfg := auth.Config{
		ServerName: "external",
		Resource:   "http://127.0.0.1:9/mcp",
		TokenStore: store,
	}
	client, err := runtime.Client(cfg)
	if err != nil || client == nil {
		t.Fatalf("external runtime was closed by the manager: %v", err)
	}
}
