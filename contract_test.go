package rig_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
)

// TestVersion pins the module version constant: it must be non-empty and carry
// a numeric major.minor.patch so consumers can parse it for diagnostics.
func TestVersion(t *testing.T) {
	if rig.Version == "" {
		t.Fatal("rig.Version is empty")
	}
	parts := strings.Split(rig.Version, ".")
	if len(parts) != 3 {
		t.Fatalf("rig.Version = %q, want major.minor.patch", rig.Version)
	}
	for _, part := range parts {
		if _, err := strconv.Atoi(part); err != nil {
			t.Fatalf("rig.Version = %q: %q is not numeric", rig.Version, part)
		}
	}
}

// TestNewMessageAndText covers the message constructor and text extraction.
func TestNewMessageAndText(t *testing.T) {
	message := rig.NewMessage(rig.RoleUser, "hello")
	if message.Role != rig.RoleUser {
		t.Fatalf("role = %q, want %q", message.Role, rig.RoleUser)
	}
	if len(message.Content) != 1 || message.Content[0] != rig.TextPart("hello") {
		t.Fatalf("content = %+v, want one text part", message.Content)
	}
	if message.ToolCalls != nil || message.ToolCallID != "" || message.Name != "" || message.Reasoning != "" {
		t.Fatalf("NewMessage set non-content fields: %+v", message)
	}
	if got := message.Text(); got != "hello" {
		t.Fatalf("Text() = %q, want hello", got)
	}

	// Text() concatenates every text part in order and skips non-text parts.
	mixed := rig.Message{
		Role: rig.RoleAssistant,
		Content: []rig.ContentPart{
			rig.TextPart("alpha "),
			rig.ImagePart("https://example.com/image.png", "high"),
			rig.TextPart("omega"),
		},
	}
	if got := mixed.Text(); got != "alpha omega" {
		t.Fatalf("Text() = %q, want %q", got, "alpha omega")
	}

	// The zero message has no text.
	if got := (rig.Message{}).Text(); got != "" {
		t.Fatalf("zero Message Text() = %q, want empty", got)
	}
}

// TestContentParts covers the text/image part constructors and their types.
func TestContentParts(t *testing.T) {
	text := rig.TextPart("body")
	if text.Type != rig.PartText || text.Text != "body" || text.ImageURL != "" || text.Detail != "" {
		t.Fatalf("TextPart = %+v", text)
	}

	image := rig.ImagePart("data:image/png;base64,AAAA", "low")
	if image.Type != rig.PartImageURL || image.ImageURL != "data:image/png;base64,AAAA" || image.Detail != "low" {
		t.Fatalf("ImagePart = %+v", image)
	}
	if image.Text != "" {
		t.Fatalf("ImagePart carries text %q", image.Text)
	}

	if rig.PartText != "text" {
		t.Fatalf("PartText = %q, want text", rig.PartText)
	}
	if rig.PartImageURL != "image_url" {
		t.Fatalf("PartImageURL = %q, want image_url", rig.PartImageURL)
	}
}

// TestToolResult covers the tool result value: the zero value is a successful
// empty result and every documented field round-trips.
func TestToolResult(t *testing.T) {
	var zero rig.ToolResult
	if zero.Content != "" || zero.IsError || zero.Parts != nil {
		t.Fatalf("zero ToolResult = %+v", zero)
	}

	result := rig.ToolResult{
		Content: "attachment follows",
		IsError: true,
		Parts:   []rig.ContentPart{rig.ImagePart("https://example.com/a.png", "")},
	}
	if result.Content != "attachment follows" || !result.IsError {
		t.Fatalf("ToolResult = %+v", result)
	}
	if len(result.Parts) != 1 || result.Parts[0].Type != rig.PartImageURL {
		t.Fatalf("ToolResult.Parts = %+v", result.Parts)
	}
}

// TestUsageAdd covers the token accounting accumulator.
func TestUsageAdd(t *testing.T) {
	var usage rig.Usage
	usage.Add(rig.Usage{PromptTokens: 10, CompletionTokens: 4, TotalTokens: 14, CachedTokens: 3})
	usage.Add(rig.Usage{PromptTokens: 2, CompletionTokens: 1, TotalTokens: 3})
	want := rig.Usage{PromptTokens: 12, CompletionTokens: 5, TotalTokens: 17, CachedTokens: 3}
	if usage != want {
		t.Fatalf("usage = %+v, want %+v", usage, want)
	}

	usage.Add(rig.Usage{})
	if usage != want {
		t.Fatalf("adding a zero usage changed the accumulator: %+v", usage)
	}
}

// TestRoleDecisionAndEventValues pins the wire/contract string values: they are
// observable to providers, models and stored history, so they are API.
func TestRoleDecisionAndEventValues(t *testing.T) {
	roles := map[rig.Role]string{
		rig.RoleSystem:    "system",
		rig.RoleUser:      "user",
		rig.RoleAssistant: "assistant",
		rig.RoleTool:      "tool",
	}
	for role, want := range roles {
		if string(role) != want {
			t.Fatalf("role %q, want %q", role, want)
		}
	}

	decisions := map[rig.Decision]string{
		rig.DecisionAllow:       "allow",
		rig.DecisionAllowAlways: "allow-always",
		rig.DecisionDeny:        "deny",
	}
	for decision, want := range decisions {
		if string(decision) != want {
			t.Fatalf("decision %q, want %q", decision, want)
		}
	}

	events := map[rig.EventType]string{
		rig.EventTurnStart:      "turn_start",
		rig.EventTextDelta:      "text_delta",
		rig.EventReasoningDelta: "reasoning_delta",
		rig.EventToolCall:       "tool_call",
		rig.EventToolResult:     "tool_result",
		rig.EventUsage:          "usage",
		rig.EventMessage:        "message",
		rig.EventTurnEnd:        "turn_end",
		rig.EventError:          "error",
	}
	for event, want := range events {
		if string(event) != want {
			t.Fatalf("event %q, want %q", event, want)
		}
	}
}

// TestMessageJSONRoundTrip pins the JSON shape of Message: history stores and
// providers persist/replay it, so the tags are contract.
func TestMessageJSONRoundTrip(t *testing.T) {
	message := rig.Message{
		Role:    rig.RoleAssistant,
		Content: []rig.ContentPart{rig.TextPart("calling tool")},
		ToolCalls: []rig.ToolCall{
			{ID: "call_1", Name: "read_file", Arguments: `{"path":"README.md"}`},
		},
		Name:      "assistant",
		Reasoning: "why I call the tool",
	}
	encoded, err := json.Marshal(message)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, field := range []string{`"role":"assistant"`, `"type":"text"`, `"tool_calls"`, `"name":"assistant"`} {
		if !strings.Contains(string(encoded), field) {
			t.Fatalf("encoded message %s is missing %s", encoded, field)
		}
	}

	var decoded rig.Message
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(decoded, message) {
		t.Fatalf("round trip = %+v, want %+v", decoded, message)
	}

	// A tool result message round-trips its correlation fields.
	toolMessage := rig.Message{
		Role:       rig.RoleTool,
		Content:    []rig.ContentPart{rig.TextPart("file body")},
		ToolCallID: "call_1",
		Name:       "read_file",
	}
	encoded, err = json.Marshal(toolMessage)
	if err != nil {
		t.Fatalf("marshal tool message: %v", err)
	}
	decoded = rig.Message{}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal tool message: %v", err)
	}
	if !reflect.DeepEqual(decoded, toolMessage) {
		t.Fatalf("tool message round trip = %+v, want %+v", decoded, toolMessage)
	}
}

// TestSentinelErrors covers the documented, still-used sentinels: they are
// non-nil, clearly namespaced, distinct, and survive wrapping with errors.Is.
func TestSentinelErrors(t *testing.T) {
	sentinels := []error{
		rig.ErrProviderRequired,
		rig.ErrModelRequired,
		rig.ErrToolNotFound,
		rig.ErrMaxIterations,
	}
	seen := make(map[error]bool, len(sentinels))
	for _, sentinel := range sentinels {
		if sentinel == nil {
			t.Fatal("nil sentinel error")
		}
		if !strings.HasPrefix(sentinel.Error(), "rig: ") {
			t.Fatalf("sentinel %q is not namespaced with rig:", sentinel)
		}
		if seen[sentinel] {
			t.Fatalf("duplicate sentinel %q", sentinel)
		}
		seen[sentinel] = true

		wrapped := fmt.Errorf("operation failed: %w", sentinel)
		if !errors.Is(wrapped, sentinel) {
			t.Fatalf("errors.Is(wrapped, %v) = false", sentinel)
		}
		for _, other := range sentinels {
			if other != sentinel && errors.Is(wrapped, other) {
				t.Fatalf("%v unexpectedly matches %v", sentinel, other)
			}
		}
	}
}
