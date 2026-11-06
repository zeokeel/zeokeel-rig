// Package toolutil holds the small helpers that every Rig tool package used to
// copy-paste: argument decoding, model-facing results, JSON Schema property
// builders and UTF-8-safe truncation.
//
// It is internal on purpose. The exported tool surface lives in the tool
// packages and in the root rig package; these helpers are implementation
// details shared across tools/builtin, tools/editor, tools/web, and so on, and
// they must never become part of the SDK contract.
package toolutil

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/zeokeel/zeokeel-rig"
)

// DecodeArgs decodes one tool call's argument JSON into target.
//
// An empty (or whitespace-only) Arguments string decodes as the empty object,
// so tools whose arguments are all optional still work when the model sends no
// arguments at all. A malformed payload yields
// "invalid arguments JSON: <cause>".
func DecodeArgs(call rig.ToolCall, target any) error {
	return DecodeArgsAs("", call, target)
}

// DecodeArgsAs is DecodeArgs with a source prefix on decode failures, e.g.
// "ralph: invalid arguments JSON: ...".
//
// A handful of packages historically namespaced their decode errors this way.
// The helper exists so their tool-call errors stay byte-identical after the
// deduplication; new call sites should use DecodeArgs.
func DecodeArgsAs(prefix string, call rig.ToolCall, target any) error {
	raw := strings.TrimSpace(call.Arguments)
	if raw == "" {
		raw = "{}"
	}
	if err := json.Unmarshal([]byte(raw), target); err != nil {
		if prefix != "" {
			return fmt.Errorf("%s: invalid arguments JSON: %w", prefix, err)
		}
		return fmt.Errorf("invalid arguments JSON: %w", err)
	}
	return nil
}

// TextResult builds a successful tool result.
func TextResult(content string) rig.ToolResult {
	return rig.ToolResult{Content: content}
}

// ErrorResult builds a model-facing tool failure whose text is err.Error().
// The agent reports the result to the model without aborting the run.
func ErrorResult(err error) rig.ToolResult {
	return rig.ToolResult{Content: err.Error(), IsError: true}
}

// ErrorResultf builds a model-facing tool failure with a formatted message.
// Format verbs and arguments follow fmt.Sprintf; a stray "%" in a message that
// is not a format string must be passed as ErrorResultf("%s", message).
func ErrorResultf(format string, args ...any) rig.ToolResult {
	return rig.ToolResult{Content: fmt.Sprintf(format, args...), IsError: true}
}

// ObjectSchema builds the JSON Schema object advertised for one tool:
// type "object", the given properties, no additional properties, and, when at
// least one name is given, the required list.
func ObjectSchema(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// StringProp builds one string property schema.
func StringProp(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

// IntegerProp builds one integer property schema.
func IntegerProp(description string) map[string]any {
	return map[string]any{"type": "integer", "description": description}
}

// BooleanProp builds one boolean property schema.
func BooleanProp(description string) map[string]any {
	return map[string]any{"type": "boolean", "description": description}
}

// StringArrayProp builds one array-of-strings property schema.
func StringArrayProp(description string) map[string]any {
	return map[string]any{
		"type":        "array",
		"description": description,
		"items":       map[string]any{"type": "string"},
	}
}

// TruncateBytes shortens s to at most max bytes without splitting a UTF-8 rune
// and appends suffix when it actually cuts. max <= 0 disables truncation, so a
// zero policy limit means "no cap". Callers that treat non-positive limits
// specially must handle that before calling.
func TruncateBytes(s string, max int, suffix string) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + suffix
}
