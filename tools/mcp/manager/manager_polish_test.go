package manager_test

// Tests for the two MCP wrap-up additions: the OnReconnect host hook (fired by
// every successful reconnect, never by the initial Start) and the optional
// Server.Key namespace for two configured servers that share a display name.

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/tools/mcp"
	"github.com/zeokeel/zeokeel-rig/tools/mcp/auth"
	"github.com/zeokeel/zeokeel-rig/tools/mcp/manager"
)

// noCallback fails when a reconnect callback arrives within the grace window.
func noCallback(t *testing.T, calls <-chan string) {
	t.Helper()
	select {
	case name := <-calls:
		t.Fatalf("OnReconnect(%q) fired unexpectedly", name)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestOnReconnectNotFiredOnInitialStart(t *testing.T) {
	calls := make(chan string, 4)
	startManager(t, manager.Config{
		Servers:     []manager.Server{{Name: "alpha", Config: fakeConfig("normal", "alpha")}},
		OnReconnect: func(server string) { calls <- server },
	})
	noCallback(t, calls)
}

func TestOnReconnectFiredOnExplicitReconnect(t *testing.T) {
	calls := make(chan string, 4)
	m := startManager(t, manager.Config{
		Servers:     []manager.Server{{Name: "alpha", Config: fakeConfig("normal", "alpha")}},
		OnReconnect: func(server string) { calls <- server },
	})
	if err := m.Reconnect(context.Background(), "alpha"); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}
	select {
	case name := <-calls:
		if name != "alpha" {
			t.Fatalf("OnReconnect(%q), want alpha", name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnReconnect was not called after an explicit reconnect")
	}
}

func TestOnReconnectFiredOnTransparentReconnect(t *testing.T) {
	calls := make(chan string, 4)
	m := startManager(t, manager.Config{
		Servers:     []manager.Server{{Name: "alpha", Config: fakeConfig("normal", "alpha")}},
		OnReconnect: func(server string) { calls <- server },
	})
	first := m.Clients()["alpha"]
	if first == nil {
		t.Fatal("no initial client")
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close initial client: %v", err)
	}
	echo := toolByName(t, m.Tools(), "mcp__alpha__echo")
	if got := invokeText(t, echo, `{"text":"again"}`); got != "echo:normal:again" {
		t.Fatalf("echo after reconnect = %q", got)
	}
	select {
	case name := <-calls:
		if name != "alpha" {
			t.Fatalf("OnReconnect(%q), want alpha", name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnReconnect was not called after a transparent reconnect")
	}
}

func TestOnReconnectNotFiredOnFailedReconnect(t *testing.T) {
	calls := make(chan string, 4)
	m := startManager(t, manager.Config{
		Servers: []manager.Server{{
			Name:   "broken",
			Config: mcp.Config{Command: filepath.Join(t.TempDir(), "missing-binary")},
		}},
		OnReconnect: func(server string) { calls <- server },
	})
	if err := m.Reconnect(context.Background(), "broken"); err == nil {
		t.Fatal("Reconnect of an unusable server succeeded")
	}
	noCallback(t, calls)
}

func TestOnReconnectFiredOnAuthorizedReconnect(t *testing.T) {
	httpServer := plainMCPHTTPServer(t, "alpha")
	reconnects := make(chan string, 2)
	authorized := make(chan string, 2)
	server := oauthServer(t, "alpha", httpServer, auth.FileTokenStore(filepath.Join(t.TempDir(), "tokens.json")))
	server.Key = "alpha-key"
	m := startManager(t, manager.Config{
		Servers:      []manager.Server{server},
		OnReconnect:  func(server string) { reconnects <- server },
		OnAuthorized: func(server string) { authorized <- server },
	})
	if client := m.Clients()["alpha-key"]; client == nil {
		t.Fatalf("Clients = %v, want the alpha-key connection", m.Clients())
	}
	if err := m.Authorized("alpha"); err != nil {
		t.Fatalf("Authorized: %v", err)
	}
	select {
	case name := <-reconnects:
		if name != "alpha-key" {
			t.Fatalf("OnReconnect(%q), want the manager key alpha-key", name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnReconnect was not called after an authorized reconnect")
	}
	select {
	case name := <-authorized:
		if name != "alpha-key" {
			t.Fatalf("OnAuthorized(%q), want the manager key alpha-key", name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnAuthorized was not called")
	}
}

func TestDuplicateNamesDistinctKeysNamespaceTools(t *testing.T) {
	m := startManager(t, manager.Config{
		Servers: []manager.Server{
			{Name: "files", Key: "files-a", Config: fakeConfig("alpha", "srv-a")},
			{Name: "files", Key: "files-b", Config: fakeConfig("beta", "srv-b")},
		},
	})
	clients := m.Clients()
	if len(clients) != 2 || clients["files-a"] == nil || clients["files-b"] == nil {
		t.Fatalf("Clients = %v, want files-a and files-b", clients)
	}
	names := make([]string, 0, len(m.Tools()))
	for _, tool := range m.Tools() {
		names = append(names, tool.Name())
	}
	first := toolByName(t, m.Tools(), "mcp__files-a__echo")
	if got := invokeText(t, first, `{"text":"hi"}`); got != "echo:alpha:hi" {
		t.Fatalf("files-a echo = %q, want the first server", got)
	}
	second := toolByName(t, m.Tools(), "mcp__files-b__echo")
	if got := invokeText(t, second, `{"text":"hi"}`); got != "echo:beta:hi" {
		t.Fatalf("files-b echo = %q, want the second server", got)
	}
	// The manager identity is the key: both connections are independently
	// reachable for reconnect.
	if err := m.Reconnect(context.Background(), "files-b"); err != nil {
		t.Fatalf("Reconnect(files-b): %v", err)
	}
	if errs := m.Errors(); len(errs) != 0 {
		t.Fatalf("Errors = %v, want none", errs)
	}
}

func TestDuplicateNamesWithoutKeysStillSkipped(t *testing.T) {
	var logs lockedLog
	m := startManager(t, manager.Config{
		Servers: []manager.Server{
			{Name: "alpha", Config: fakeConfig("first", "alpha")},
			{Name: "alpha", Config: fakeConfig("second", "alpha")},
		},
		Logger: logs.logf,
	})
	if clients := m.Clients(); len(clients) != 1 || clients["alpha"] == nil {
		t.Fatalf("Clients = %v, want only the first alpha", clients)
	}
	echo := toolByName(t, m.Tools(), "mcp__alpha__echo")
	if got := invokeText(t, echo, `{"text":"hi"}`); got != "echo:first:hi" {
		t.Fatalf("duplicate-name echo = %q, want the first server", got)
	}
	if logs.count("duplicate server name") == 0 {
		t.Fatalf("duplicate identity was not logged: %v", logs.snapshot())
	}
}

func TestUniqueNameWithoutKeyKeepsNamespace(t *testing.T) {
	m := startManager(t, manager.Config{
		Servers: []manager.Server{{Name: "alpha", Config: fakeConfig("normal", "alpha")}},
	})
	if clients := m.Clients(); len(clients) != 1 || clients["alpha"] == nil {
		t.Fatalf("Clients = %v, want alpha", clients)
	}
	toolByName(t, m.Tools(), "mcp__alpha__echo")
}
