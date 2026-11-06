package session

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
	rigtools "github.com/zeokeel/zeokeel-rig/tools"
)

func testRegistry(t *testing.T, log v1.SessionLog) *rigtools.Registry {
	t.Helper()
	return rigtools.NewRegistry(Tools(log)...)
}

func invoke(t *testing.T, registry *rigtools.Registry, name, arguments string) (v1.ToolResult, error) {
	t.Helper()
	return registry.Invoke(context.Background(), v1.ToolCall{ID: "call-1", Name: name, Arguments: arguments})
}

func invokeOK(t *testing.T, registry *rigtools.Registry, name, arguments string) string {
	t.Helper()
	result, err := invoke(t, registry, name, arguments)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if result.IsError {
		t.Fatalf("%s returned an error result: %s", name, result.Content)
	}
	return result.Content
}

func invokeError(t *testing.T, registry *rigtools.Registry, name, arguments string) string {
	t.Helper()
	result, err := invoke(t, registry, name, arguments)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if !result.IsError {
		t.Fatalf("%s did not return an error result: %q", name, result.Content)
	}
	return result.Content
}

func TestToolCatalog(t *testing.T) {
	tools := Tools(NewMemoryLog())
	want := []string{ToolSearch, ToolTrace, ToolEventRead, ToolEventSearch, ToolEventTrace}
	if len(tools) != len(want) {
		t.Fatalf("Tools() = %d tools, want %d", len(tools), len(want))
	}
	for index, tool := range tools {
		if tool.Name() != want[index] {
			t.Fatalf("tool[%d] = %q, want %q", index, tool.Name(), want[index])
		}
		if strings.TrimSpace(tool.Description()) == "" {
			t.Fatalf("%s has an empty description", tool.Name())
		}
		schema := tool.Schema()
		if schema["type"] != "object" {
			t.Fatalf("%s schema type = %v, want object", tool.Name(), schema["type"])
		}
		if _, ok := schema["properties"].(map[string]any); !ok {
			t.Fatalf("%s schema has no properties object", tool.Name())
		}
	}
	required := func(name string) []string {
		for _, tool := range tools {
			if tool.Name() == name {
				values, _ := tool.Schema()["required"].([]string)
				return values
			}
		}
		return nil
	}
	if got := strings.Join(required(ToolSearch), ","); got != "query" {
		t.Fatalf("session_search required = %q", got)
	}
	if got := strings.Join(required(ToolTrace), ","); got != "session_id" {
		t.Fatalf("session_trace required = %q", got)
	}
	if got := strings.Join(required(ToolEventRead), ","); got != "session_id,seq" {
		t.Fatalf("session_event_read required = %q", got)
	}
	if got := strings.Join(required(ToolEventSearch), ","); got != "query" {
		t.Fatalf("session_event_search required = %q", got)
	}
	if got := strings.Join(required(ToolEventTrace), ","); got != "seq" {
		t.Fatalf("session_event_trace required = %q", got)
	}
}

func TestSessionSearchTool(t *testing.T) {
	registry := testRegistry(t, populatedLog(t))
	content := invokeOK(t, registry, ToolSearch, `{"query":"boom"}`)
	if !strings.Contains(content, "#4") || !strings.Contains(content, "[s1]") || !strings.Contains(content, "error") {
		t.Fatalf("session_search content = %q", content)
	}
	if content := invokeOK(t, registry, ToolSearch, `{"query":"nothing-matches"}`); !strings.Contains(content, "no session events match") {
		t.Fatalf("empty search = %q", content)
	}
	if content := invokeError(t, registry, ToolSearch, `{"query":"  "}`); !strings.Contains(content, "query is required") {
		t.Fatalf("blank query = %q", content)
	}

	// Limit is clamped to the documented maximum.
	spy := &spyLog{MemoryLog: NewMemoryLog()}
	spyRegistry := testRegistry(t, spy)
	invokeOK(t, spyRegistry, ToolSearch, `{"query":"x","limit":1000}`)
	if got := spy.searchLimit(); got != MaxLimit {
		t.Fatalf("search limit = %d, want clamp to %d", got, MaxLimit)
	}
	invokeOK(t, spyRegistry, ToolSearch, `{"query":"x"}`)
	if got := spy.searchLimit(); got != DefaultLimit {
		t.Fatalf("default search limit = %d, want %d", got, DefaultLimit)
	}
}

func TestSessionTraceTool(t *testing.T) {
	registry := testRegistry(t, populatedLog(t))
	content := invokeOK(t, registry, ToolTrace, `{"session_id":"s1"}`)
	for _, want := range []string{"session \"s1\"", "#1", "#2", "tool_call", "user_message"} {
		if !strings.Contains(content, want) {
			t.Fatalf("session_trace content = %q, want %q", content, want)
		}
	}
	content = invokeOK(t, registry, ToolTrace, `{"session_id":"s1","from":2,"to":3}`)
	if strings.Contains(content, "#1") || !strings.Contains(content, "#2") || !strings.Contains(content, "#3") {
		t.Fatalf("session_trace range content = %q", content)
	}
	if content := invokeError(t, registry, ToolTrace, `{}`); !strings.Contains(content, "session_id is required") {
		t.Fatalf("trace without session = %q", content)
	}
	if content := invokeOK(t, registry, ToolTrace, `{"session_id":"ghost"}`); !strings.Contains(content, "has no events") {
		t.Fatalf("trace unknown session = %q", content)
	}
}

func TestSessionEventReadTool(t *testing.T) {
	log := populatedLog(t)
	registry := testRegistry(t, log)
	content := invokeOK(t, registry, ToolEventRead, `{"session_id":"s1","seq":2}`)
	for _, want := range []string{"event #2", "type: tool_call", "read_file", "time: 2026-11-06T12:00:00Z"} {
		if !strings.Contains(content, want) {
			t.Fatalf("session_event_read content = %q, want %q", content, want)
		}
	}
	if content := invokeOK(t, registry, ToolEventRead, `{"session_id":"s1","seq":99}`); !strings.Contains(content, "not found") {
		t.Fatalf("missing event = %q", content)
	}
	if content := invokeError(t, registry, ToolEventRead, `{"seq":2}`); !strings.Contains(content, "session_id is required") {
		t.Fatalf("event_read without session = %q", content)
	}
	if content := invokeError(t, registry, ToolEventRead, `{"session_id":"s1","seq":0}`); !strings.Contains(content, "seq must be a positive") {
		t.Fatalf("event_read bad seq = %q", content)
	}

	// A large payload is truncated with a notice.
	huge := json.RawMessage(`{"text":"` + strings.Repeat("x", 8<<10) + `"}`)
	log.Append(v1.SessionEvent{SessionID: "s1", Type: "message", Payload: huge})
	content = invokeOK(t, registry, ToolEventRead, `{"session_id":"s1","seq":7}`)
	if !strings.Contains(content, "…") {
		t.Fatalf("large payload was not truncated: %d bytes", len(content))
	}
}

func TestSessionEventSearchTool(t *testing.T) {
	registry := testRegistry(t, populatedLog(t))
	content := invokeOK(t, registry, ToolEventSearch, `{"query":"read_file"}`)
	if !strings.Contains(content, "payload:") || !strings.Contains(content, "read_file") {
		t.Fatalf("session_event_search content = %q", content)
	}
}

func TestSessionEventTraceTool(t *testing.T) {
	registry := testRegistry(t, populatedLog(t))
	content := invokeOK(t, registry, ToolEventTrace, `{"session_id":"s1","seq":2}`)
	if !strings.Contains(content, ">> #2") {
		t.Fatalf("session_event_trace content = %q, want a target marker", content)
	}
	for _, want := range []string{"#1", "#3", "#4"} {
		if !strings.Contains(content, want) {
			t.Fatalf("session_event_trace content = %q, want context %q", content, want)
		}
	}
	// Session auto-resolution when the sequence number is unique.
	content = invokeOK(t, registry, ToolEventTrace, `{"seq":5,"before":1,"after":1}`)
	if !strings.Contains(content, "s2") || !strings.Contains(content, ">> #5") {
		t.Fatalf("auto-resolved trace content = %q", content)
	}

	// Sequence numbers are unique in MemoryLog; a log with repeated sequence
	// numbers across sessions must report the ambiguity.
	ambiguous := NewMemoryLog()
	ambiguous.Append(v1.SessionEvent{Seq: 2, SessionID: "a", Type: "message"})
	ambiguous.Append(v1.SessionEvent{Seq: 2, SessionID: "b", Type: "message"})
	ambiguousRegistry := testRegistry(t, ambiguous)
	if content := invokeError(t, ambiguousRegistry, ToolEventTrace, `{"seq":2}`); !strings.Contains(content, "several sessions") {
		t.Fatalf("ambiguous trace = %q", content)
	}
	if content := invokeError(t, registry, ToolEventTrace, `{"seq":99}`); !strings.Contains(content, "pass session_id") {
		t.Fatalf("unknown trace seq = %q", content)
	}
	if content := invokeError(t, registry, ToolEventTrace, `{}`); !strings.Contains(content, "seq must be a positive") {
		t.Fatalf("trace without seq = %q", content)
	}
	if content := invokeError(t, registry, ToolEventTrace, `{"session_id":"s1","seq":1,"before":-1}`); !strings.Contains(content, "must not be negative") {
		t.Fatalf("negative before = %q", content)
	}
}

func TestNilLogDegrades(t *testing.T) {
	registry := testRegistry(t, nil)
	cases := map[string]string{
		ToolSearch:      `{"query":"x"}`,
		ToolTrace:       `{"session_id":"s1"}`,
		ToolEventRead:   `{"session_id":"s1","seq":1}`,
		ToolEventSearch: `{"query":"x"}`,
		ToolEventTrace:  `{"seq":1,"session_id":"s1"}`,
	}
	for name, arguments := range cases {
		content := invokeError(t, registry, name, arguments)
		if !strings.Contains(content, "no session log is configured") {
			t.Fatalf("%s with nil log = %q", name, content)
		}
	}
}

func TestToolContextCancellation(t *testing.T) {
	registry := testRegistry(t, populatedLog(t))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cases := map[string]string{
		ToolSearch:      `{"query":"x"}`,
		ToolTrace:       `{"session_id":"s1"}`,
		ToolEventRead:   `{"session_id":"s1","seq":1}`,
		ToolEventSearch: `{"query":"x"}`,
		ToolEventTrace:  `{"seq":1,"session_id":"s1"}`,
	}
	for name, arguments := range cases {
		if _, err := registry.Invoke(ctx, v1.ToolCall{Name: name, Arguments: arguments}); !errors.Is(err, context.Canceled) {
			t.Fatalf("%s(cancelled ctx) = %v, want context.Canceled", name, err)
		}
	}
}

func TestToolMalformedArguments(t *testing.T) {
	registry := testRegistry(t, populatedLog(t))
	if _, err := invoke(t, registry, ToolSearch, "not-json"); err == nil {
		t.Fatal("session_search accepted invalid arguments JSON")
	}
}

// spyLog records the limit a tool passed to Search.
type spyLog struct {
	*MemoryLog
	mu    sync.Mutex
	limit int
}

func (s *spyLog) Search(ctx context.Context, query string, limit int) ([]v1.SessionEvent, error) {
	s.mu.Lock()
	s.limit = limit
	s.mu.Unlock()
	return s.MemoryLog.Search(ctx, query, limit)
}

func (s *spyLog) searchLimit() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.limit
}
