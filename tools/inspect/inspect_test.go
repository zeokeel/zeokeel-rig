package inspect

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	rigtools "github.com/zeokeel/zeokeel-rig/tools"
	"github.com/zeokeel/zeokeel-rig/tools/session"
)

func testRegistry(t *testing.T, opts Options) *rigtools.Registry {
	t.Helper()
	return rigtools.NewRegistry(NewInspectTool(opts))
}

func invoke(t *testing.T, registry *rigtools.Registry, arguments string) (rig.ToolResult, error) {
	t.Helper()
	return registry.Invoke(context.Background(), rig.ToolCall{ID: "call-1", Name: ToolName, Arguments: arguments})
}

func invokeOK(t *testing.T, registry *rigtools.Registry, arguments string) string {
	t.Helper()
	result, err := invoke(t, registry, arguments)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if result.IsError {
		t.Fatalf("inspect returned an error result: %s", result.Content)
	}
	return result.Content
}

func invokeError(t *testing.T, registry *rigtools.Registry, arguments string) string {
	t.Helper()
	result, err := invoke(t, registry, arguments)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !result.IsError {
		t.Fatalf("inspect did not return an error result: %q", result.Content)
	}
	return result.Content
}

// populatedLog mirrors the session package fixture: two sessions, four s1
// events (including one tool call and one error) and two s2 events.
func populatedLog() *session.MemoryLog {
	log := session.NewMemoryLog()
	stamp := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	log.Append(rig.SessionEvent{SessionID: "s1", Type: "user_message", Time: stamp, Payload: json.RawMessage(`{"text":"hello"}`)})
	log.Append(rig.SessionEvent{SessionID: "s1", Type: "tool_call", Time: stamp, Payload: json.RawMessage(`{"tool":"read_file"}`)})
	log.Append(rig.SessionEvent{SessionID: "s1", Type: "tool_result", Payload: json.RawMessage(`{"ok":true}`)})
	log.Append(rig.SessionEvent{SessionID: "s1", Type: "error", Payload: json.RawMessage(`{"message":"boom"}`)})
	log.Append(rig.SessionEvent{SessionID: "s2", Type: "user_message", Payload: json.RawMessage(`{"text":"other"}`)})
	log.Append(rig.SessionEvent{SessionID: "s2", Type: "tool_call", Payload: json.RawMessage(`{"tool":"bash"}`)})
	return log
}

// plainLog exposes only the rig.SessionLog methods (no Sessions extension).
type plainLog struct {
	inner rig.SessionLog
}

func (p plainLog) Events(ctx context.Context, sessionID string, from, to int64) ([]rig.SessionEvent, error) {
	return p.inner.Events(ctx, sessionID, from, to)
}

func (p plainLog) Search(ctx context.Context, query string, limit int) ([]rig.SessionEvent, error) {
	return p.inner.Search(ctx, query, limit)
}

func (p plainLog) Last(ctx context.Context, sessionID string, limit int) ([]rig.SessionEvent, error) {
	return p.inner.Last(ctx, sessionID, limit)
}

func TestToolCatalog(t *testing.T) {
	tool := NewInspectTool(Options{})
	if tool.Name() != ToolName {
		t.Fatalf("Name() = %q, want %q", tool.Name(), ToolName)
	}
	if strings.TrimSpace(tool.Description()) == "" {
		t.Fatal("inspect has an empty description")
	}
	schema := tool.Schema()
	if schema["type"] != "object" {
		t.Fatalf("schema type = %v", schema["type"])
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("schema has no properties")
	}
	action, ok := properties["action"].(map[string]any)
	if !ok {
		t.Fatal("schema has no action property")
	}
	enum, _ := action["enum"].([]any)
	if len(enum) != 4 {
		t.Fatalf("action enum = %v, want four actions", enum)
	}
	required, _ := schema["required"].([]string)
	if len(required) != 1 || required[0] != "action" {
		t.Fatalf("required = %v, want [action]", required)
	}
}

func TestWorkspaceAction(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "one.txt"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "two.go"), []byte("2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "sub", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "three.md"), []byte("3"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := testRegistry(t, Options{Root: root})
	content := invokeOK(t, registry, `{"action":"workspace"}`)
	for _, want := range []string{"inspect: workspace", "root: " + root, "files: 3", "directories: 2", "top-level:", "one.txt", "sub/"} {
		if !strings.Contains(content, want) {
			t.Fatalf("workspace content = %q, want %q", content, want)
		}
	}
	// The limit caps the file walk.
	content = invokeOK(t, registry, `{"action":"workspace","limit":1}`)
	if !strings.Contains(content, "files: 1") || !strings.Contains(content, "[truncated: counted the first 1 files]") {
		t.Fatalf("workspace limited content = %q", content)
	}

	// No root configured: readable placeholder, no error.
	content = invokeOK(t, testRegistry(t, Options{}), `{"action":"workspace"}`)
	if !strings.Contains(content, "root: (not configured)") {
		t.Fatalf("unconfigured workspace = %q", content)
	}
}

func TestSessionsActionWithLister(t *testing.T) {
	log := populatedLog()
	content := invokeOK(t, testRegistry(t, Options{Log: log}), `{"action":"sessions"}`)
	for _, want := range []string{"inspect: sessions", "session log: configured", "sessions: 2", "s1", "s2", "last=#4"} {
		if !strings.Contains(content, want) {
			t.Fatalf("sessions content = %q, want %q", content, want)
		}
	}
	// Limit truncates the session list.
	content = invokeOK(t, testRegistry(t, Options{Log: log}), `{"action":"sessions","limit":1}`)
	if !strings.Contains(content, "[truncated: showing 1 of 2 sessions]") {
		t.Fatalf("limited sessions content = %q", content)
	}
}

func TestSessionsActionFallback(t *testing.T) {
	log := plainLog{inner: populatedLog()}
	content := invokeOK(t, testRegistry(t, Options{Log: log}), `{"action":"sessions"}`)
	for _, want := range []string{"no session enumeration", "sessions: 2", "s1", "s2"} {
		if !strings.Contains(content, want) {
			t.Fatalf("fallback sessions content = %q, want %q", content, want)
		}
	}
}

func TestEventsAction(t *testing.T) {
	registry := testRegistry(t, Options{Log: populatedLog()})
	content := invokeOK(t, registry, `{"action":"events","session_id":"s1","limit":2}`)
	for _, want := range []string{"inspect: events", "session: s1", "events: 2 (limit 2)", "tool_result", "error"} {
		if !strings.Contains(content, want) {
			t.Fatalf("events content = %q, want %q", content, want)
		}
	}
	if content := invokeError(t, registry, `{"action":"events"}`); !strings.Contains(content, "session_id is required") {
		t.Fatalf("events without session = %q", content)
	}
	if content := invokeOK(t, registry, `{"action":"events","session_id":"ghost"}`); !strings.Contains(content, "events: 0") {
		t.Fatalf("events for unknown session = %q", content)
	}
}

func TestStatsAction(t *testing.T) {
	registry := testRegistry(t, Options{Log: populatedLog()})
	content := invokeOK(t, registry, `{"action":"stats","session_id":"s1"}`)
	for _, want := range []string{
		"inspect: stats",
		"scope: session s1",
		"events: 4",
		"sequence range: #1..#4",
		"tool calls: 1",
		"errors: 1",
		"event types:",
		"user_message: 1",
		"recent events:",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("stats content = %q, want %q", content, want)
		}
	}
	// Without session_id the stats scope is the searchable event set.
	content = invokeOK(t, registry, `{"action":"stats"}`)
	for _, want := range []string{"scope: searchable events", "events: 6", "tool calls: 2"} {
		if !strings.Contains(content, want) {
			t.Fatalf("global stats content = %q, want %q", content, want)
		}
	}
	// The event scan is capped by limit and says so.
	content = invokeOK(t, registry, `{"action":"stats","session_id":"s1","limit":2}`)
	if !strings.Contains(content, "events: 2 (limit 2)") || !strings.Contains(content, "truncated") {
		t.Fatalf("capped stats content = %q", content)
	}
}

func TestNilLogDegradesToWorkspace(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := testRegistry(t, Options{Root: root})
	for _, action := range []string{"sessions", "events", "stats"} {
		arguments := `{"action":"` + action + `"`
		if action == "events" {
			arguments += `,"session_id":"s1"`
		}
		arguments += `}`
		content := invokeOK(t, registry, arguments)
		for _, want := range []string{"session log: not configured", "inspect: workspace", "root: " + root, "files: 1"} {
			if !strings.Contains(content, want) {
				t.Fatalf("%s without log = %q, want %q", action, content, want)
			}
		}
	}
}

func TestInvalidActionsAndArguments(t *testing.T) {
	registry := testRegistry(t, Options{})
	if content := invokeError(t, registry, `{}`); !strings.Contains(content, "action is required") {
		t.Fatalf("missing action = %q", content)
	}
	if content := invokeError(t, registry, `{"action":"bogus"}`); !strings.Contains(content, "unknown action") {
		t.Fatalf("unknown action = %q", content)
	}
	if _, err := invoke(t, registry, "not-json"); err == nil {
		t.Fatal("inspect accepted invalid arguments JSON")
	}
}

func TestContextCancellation(t *testing.T) {
	registry := testRegistry(t, Options{Log: populatedLog()})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, arguments := range []string{`{"action":"sessions"}`, `{"action":"events","session_id":"s1"}`, `{"action":"workspace"}`, `{"action":"stats"}`} {
		if _, err := registry.Invoke(ctx, rig.ToolCall{Name: ToolName, Arguments: arguments}); !errors.Is(err, context.Canceled) {
			t.Fatalf("inspect(%s) with cancelled ctx = %v", arguments, err)
		}
	}
}
