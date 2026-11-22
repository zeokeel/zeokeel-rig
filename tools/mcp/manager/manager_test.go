package manager_test

// Tests for the host-agnostic MCP server manager. The manager supervises one
// live rig mcp.Client per configured server, exposes their tools, isolates a
// failing server from the healthy ones, transparently reconnects after a client
// loss and closes every connection on request.
//
// The fake MCP server is the test binary itself: TestMain intercepts when
// RIG_MCP_MANAGER_HELPER=1 and speaks newline-delimited JSON-RPC on stdin/stdout.
// That keeps the suite standard-library only (no external MCP binary, no
// network) and deterministic.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	rig "github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/mcp"
	"github.com/zeokeel/zeokeel-rig/tools/mcp/manager"
)

const (
	helperEnv   = "RIG_MCP_MANAGER_HELPER"
	scenarioEnv = "RIG_MCP_MANAGER_SCENARIO"
	serverEnv   = "RIG_MCP_MANAGER_SERVER"
)

// TestMain doubles as the fake MCP server executable.
func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		os.Exit(runFakeServer(os.Stdin, os.Stdout, os.Getenv(scenarioEnv), os.Getenv(serverEnv)))
	}
	os.Exit(m.Run())
}

// fakeConfig returns the stdio config that re-executes this test binary as one
// fake server.
func fakeConfig(scenario, serverName string) mcp.Config {
	return mcp.Config{
		Command: os.Args[0],
		Env: append(os.Environ(),
			helperEnv+"=1",
			scenarioEnv+"="+scenario,
			serverEnv+"="+serverName,
		),
		Timeout: 5 * time.Second,
	}
}

// startManager builds and starts one manager, failing the test on error.
func startManager(t *testing.T, cfg manager.Config) *manager.Manager {
	t.Helper()
	m := manager.New(cfg)
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// toolByName finds one model-visible tool by exact name.
func toolByName(t *testing.T, tools []rig.Tool, name string) rig.Tool {
	t.Helper()
	for _, tool := range tools {
		if tool.Name() == name {
			return tool
		}
	}
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name())
	}
	t.Fatalf("tool %q not found in %v", name, names)
	return nil
}

// invokeText invokes one tool and returns its rendered text.
func invokeText(t *testing.T, tool rig.Tool, arguments string) string {
	t.Helper()
	result, err := tool.Invoke(context.Background(), rig.ToolCall{Name: tool.Name(), Arguments: arguments})
	if err != nil {
		t.Fatalf("invoke %s: %v", tool.Name(), err)
	}
	if result.IsError {
		t.Fatalf("invoke %s returned a tool error: %q", tool.Name(), result.Content)
	}
	return result.Content
}

// TestStartRegistersToolsFromConfiguredServer is the base supervision test: one
// configured server connects, its tools appear with the rig MCP namespace and
// calls reach the server.
func TestStartRegistersToolsFromConfiguredServer(t *testing.T) {
	var logs lockedLog
	m := startManager(t, manager.Config{
		Servers: []manager.Server{{
			Name:   "alpha",
			Config: fakeConfig("normal", "alpha"),
		}},
		Logger: logs.logf,
	})

	clients := m.Clients()
	if len(clients) != 1 || clients["alpha"] == nil {
		t.Fatalf("Clients = %+v, want the alpha client", clients)
	}
	if errs := m.Errors(); len(errs) != 0 {
		t.Fatalf("Errors = %v, want none", errs)
	}

	echo := toolByName(t, m.Tools(), "mcp__alpha__echo")
	if got := invokeText(t, echo, `{"text":"hello"}`); got != "echo:normal:hello" {
		t.Fatalf("echo result = %q", got)
	}
	if logs.count("connected") == 0 {
		t.Fatalf("connect was not logged: %v", logs.snapshot())
	}
}

// TestDisabledServerIsNotStarted: a disabled server keeps no connection and
// contributes no tools, while the enabled sibling still works.
func TestDisabledServerIsNotStarted(t *testing.T) {
	m := startManager(t, manager.Config{
		Servers: []manager.Server{
			{Name: "alpha", Config: fakeConfig("normal", "alpha")},
			{Name: "beta", Config: fakeConfig("normal", "beta"), Disabled: true},
		},
	})

	clients := m.Clients()
	if clients["beta"] != nil {
		t.Fatal("disabled server beta has a live client")
	}
	if clients["alpha"] == nil {
		t.Fatal("enabled server alpha has no client")
	}
	for _, tool := range m.Tools() {
		if strings.Contains(tool.Name(), "__beta__") {
			t.Fatalf("disabled server contributed tool %q", tool.Name())
		}
	}
	if _, ok := m.Errors()["beta"]; ok {
		t.Fatal("disabled server was reported as a startup error")
	}
}

// TestDisabledToolsAreSkipped: per-server disabled tool names never reach the
// exposed tool list, but the other tools of the same server do.
func TestDisabledToolsAreSkipped(t *testing.T) {
	m := startManager(t, manager.Config{
		Servers: []manager.Server{{
			Name:          "alpha",
			Config:        fakeConfig("normal", "alpha"),
			DisabledTools: []string{"  mcp__alpha__echo  "},
		}},
	})
	names := make([]string, 0, len(m.Tools()))
	for _, tool := range m.Tools() {
		names = append(names, tool.Name())
		if tool.Name() == "mcp__alpha__echo" {
			t.Fatal("disabled tool mcp__alpha__echo was registered")
		}
	}
	if len(names) == 0 {
		t.Fatal("disabling one tool removed every tool")
	}
}

// TestReconnectAfterClientFailure: when the underlying client died, the next
// tool call transparently rebuilds the connection and retries once.
func TestReconnectAfterClientFailure(t *testing.T) {
	m := startManager(t, manager.Config{
		Servers: []manager.Server{{Name: "alpha", Config: fakeConfig("normal", "alpha")}},
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
	second := m.Clients()["alpha"]
	if second == nil || second == first {
		t.Fatalf("client was not replaced after the failure (first=%p second=%p)", first, second)
	}
	if errs := m.Errors(); len(errs) != 0 {
		t.Fatalf("Errors after successful reconnect = %v", errs)
	}
}

// TestExplicitReconnect: the host can rebuild a live connection on demand (for
// example after completing an OAuth authorization).
func TestExplicitReconnect(t *testing.T) {
	m := startManager(t, manager.Config{
		Servers: []manager.Server{{Name: "alpha", Config: fakeConfig("normal", "alpha")}},
	})
	first := m.Clients()["alpha"]
	if err := m.Reconnect(context.Background(), "alpha"); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}
	second := m.Clients()["alpha"]
	if second == nil || second == first {
		t.Fatalf("Reconnect did not replace the client (first=%p second=%p)", first, second)
	}
	if err := first.Ping(context.Background()); !errors.Is(err, mcp.ErrClosed) {
		t.Fatalf("old client Ping = %v, want ErrClosed", err)
	}
	if err := m.Reconnect(context.Background(), "missing"); !errors.Is(err, manager.ErrUnknownServer) {
		t.Fatalf("Reconnect(missing) = %v, want ErrUnknownServer", err)
	}
}

// TestCloseAll: Close closes every client, idempotently, and the manager refuses
// further work afterwards.
func TestCloseAll(t *testing.T) {
	m := manager.New(manager.Config{
		Servers: []manager.Server{
			{Name: "alpha", Config: fakeConfig("normal", "alpha")},
			{Name: "beta", Config: fakeConfig("normal", "beta")},
		},
	})
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	clients := m.Clients()
	if len(clients) != 2 {
		t.Fatalf("Clients = %v, want two", clients)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if got := m.Clients(); len(got) != 0 {
		t.Fatalf("Clients after Close = %v, want empty", got)
	}
	if got := m.Tools(); len(got) != 0 {
		t.Fatalf("Tools after Close = %v, want empty", got)
	}
	for name, client := range clients {
		if err := client.Ping(context.Background()); !errors.Is(err, mcp.ErrClosed) {
			t.Fatalf("client %q Ping after Close = %v, want ErrClosed", name, err)
		}
	}
	if err := m.Start(context.Background()); !errors.Is(err, manager.ErrClosed) {
		t.Fatalf("Start after Close = %v, want ErrClosed", err)
	}
	if err := m.Reconnect(context.Background(), "alpha"); !errors.Is(err, manager.ErrClosed) {
		t.Fatalf("Reconnect after Close = %v, want ErrClosed", err)
	}
}

// TestDuplicateToolNamesFirstServerWins: two distinct server names whose visible
// tool namespace sanitizes to the same string must not produce duplicate tool
// names. The first configured server keeps the name; the later duplicate is
// skipped and logged.
func TestDuplicateToolNamesFirstServerWins(t *testing.T) {
	var logs lockedLog
	m := startManager(t, manager.Config{
		Servers: []manager.Server{
			{Name: "srv.one", Config: fakeConfig("first", "srv.one")},
			{Name: "srv_one", Config: fakeConfig("second", "srv_one")},
		},
		Logger: logs.logf,
	})

	var matches []rig.Tool
	for _, tool := range m.Tools() {
		if tool.Name() == "mcp__srv_one__echo" {
			matches = append(matches, tool)
		}
	}
	if len(matches) != 1 {
		names := make([]string, 0, len(m.Tools()))
		for _, tool := range m.Tools() {
			names = append(names, tool.Name())
		}
		t.Fatalf("duplicate tool name count = %d, tools = %v", len(matches), names)
	}
	if got := invokeText(t, matches[0], `{"text":"hi"}`); got != "echo:first:hi" {
		t.Fatalf("colliding tool dispatched to %q, want the first server", got)
	}
	if logs.count("duplicate") == 0 {
		t.Fatalf("duplicate skip was not logged: %v", logs.snapshot())
	}
}

// TestStartupFailureIsIsolated: one server that cannot start must not prevent
// the healthy one from starting, and the failure stays observable.
func TestStartupFailureIsIsolated(t *testing.T) {
	var logs lockedLog
	m := startManager(t, manager.Config{
		Servers: []manager.Server{
			{Name: "alpha", Config: fakeConfig("normal", "alpha")},
			{Name: "broken", Config: mcp.Config{Command: filepath.Join(t.TempDir(), "no-such-mcp-server")}},
		},
		Logger: logs.logf,
	})

	if m.Clients()["broken"] != nil {
		t.Fatal("broken server has a live client")
	}
	if _, ok := m.Errors()["broken"]; !ok {
		t.Fatalf("Errors = %v, want a broken entry", m.Errors())
	}
	if got := invokeText(t, toolByName(t, m.Tools(), "mcp__alpha__echo"), `{"text":"ok"}`); got != "echo:normal:ok" {
		t.Fatalf("healthy server result = %q", got)
	}
	if logs.count("broken") == 0 {
		t.Fatalf("startup failure was not logged: %v", logs.snapshot())
	}
}

// TestOutputLimitTruncates bounds one tool result and marks the cut, so a large
// MCP payload cannot flood the model context.
func TestOutputLimitTruncates(t *testing.T) {
	m := startManager(t, manager.Config{
		MaxOutputBytes: 80,
		Servers:        []manager.Server{{Name: "alpha", Config: fakeConfig("normal", "alpha")}},
	})
	big := toolByName(t, m.Tools(), "mcp__alpha__big")
	content := invokeText(t, big, `{}`)
	if len(content) > 80 {
		t.Fatalf("bounded result length = %d, want <= 80", len(content))
	}
	if !strings.Contains(content, "[truncated:") {
		t.Fatalf("bounded result missing the truncation marker: %q", content)
	}
}

// TestToolsBeforeStartAndEmptyConfig: a fresh manager exposes nothing, and an
// empty configuration starts cleanly.
func TestToolsBeforeStartAndEmptyConfig(t *testing.T) {
	m := manager.New(manager.Config{})
	if got := m.Tools(); len(got) != 0 {
		t.Fatalf("Tools before Start = %v, want empty", got)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start(empty): %v", err)
	}
	if got := m.Tools(); len(got) != 0 {
		t.Fatalf("Tools for an empty config = %v, want empty", got)
	}
	if got := m.Clients(); len(got) != 0 {
		t.Fatalf("Clients for an empty config = %v, want empty", got)
	}
}

// lockedLog is a goroutine-safe logger callback.
type lockedLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *lockedLog) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *lockedLog) count(fragment string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	total := 0
	for _, line := range l.lines {
		if strings.Contains(strings.ToLower(line), strings.ToLower(fragment)) {
			total++
		}
	}
	return total
}

func (l *lockedLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// --- fake MCP server -------------------------------------------------------

// fakeRequest is one JSON-RPC request read by the fake server.
type fakeRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// runFakeServer serves the newline-delimited JSON-RPC protocol until stdin
// closes. The scenario wins the echo result, so tests can tell two connections
// apart.
func runFakeServer(stdin io.Reader, stdout io.Writer, scenario, serverName string) int {
	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), mcp.MaxMessageBytes)
	out := bufio.NewWriter(stdout)
	write := func(message any) {
		data, err := json.Marshal(message)
		if err != nil {
			return
		}
		if _, err := out.Write(append(data, '\n')); err != nil {
			return
		}
		_ = out.Flush()
	}
	reply := func(id json.RawMessage, result any) {
		if len(id) == 0 {
			return
		}
		write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	replyError := func(id json.RawMessage, code int, message string) {
		write(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
	}
	if strings.TrimSpace(serverName) == "" {
		serverName = "fakeserver"
	}
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var request fakeRequest
		if err := json.Unmarshal(line, &request); err != nil {
			continue
		}
		switch request.Method {
		case "initialize":
			reply(request.ID, map[string]any{
				"protocolVersion": "2025-06-18",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": serverName, "version": "9.9.9"},
			})
		case "notifications/initialized":
			// no response
		case "ping":
			reply(request.ID, map[string]any{})
		case "tools/list":
			reply(request.ID, map[string]any{"tools": []any{
				fakeTool("echo", "Echo text back", map[string]any{
					"type":       "object",
					"properties": map[string]any{"text": map[string]any{"type": "string"}},
				}),
				fakeTool("big", "Return a large payload", map[string]any{"type": "object"}),
				fakeTool("fail", "Fail at the tool level", map[string]any{"type": "object"}),
			}})
		case "tools/call":
			var params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal(request.Params, &params); err != nil {
				replyError(request.ID, -32602, "invalid params")
				continue
			}
			switch params.Name {
			case "echo":
				var args struct {
					Text string `json:"text"`
				}
				_ = json.Unmarshal(params.Arguments, &args)
				reply(request.ID, map[string]any{
					"content": []any{map[string]any{
						"type": "text",
						"text": "echo:" + scenario + ":" + args.Text,
					}},
				})
			case "big":
				reply(request.ID, map[string]any{
					"content": []any{map[string]any{
						"type": "text",
						"text": strings.Repeat("x", 500),
					}},
				})
			case "fail":
				reply(request.ID, map[string]any{
					"isError": true,
					"content": []any{map[string]any{"type": "text", "text": "boom"}},
				})
			default:
				replyError(request.ID, -32602, "unknown tool "+params.Name)
			}
		default:
			replyError(request.ID, -32601, "method not found: "+request.Method)
		}
	}
	return 0
}

// fakeTool builds one tools/list entry.
func fakeTool(name, description string, schema map[string]any) map[string]any {
	return map[string]any{
		"name":        name,
		"description": description,
		"inputSchema": schema,
	}
}
