// Package misc provides small self-contained tools that keep session-local
// state in memory: working_directory (navigate a filesystem root) and
// todo_write (the model's todo list).
//
// Both tools are stateful on purpose, but the state lives in the tool value (or
// its store), never in package variables: a host creates one tool per agent and
// the tool is safe for concurrent calls. State is process-local and is lost on
// restart, which matches how these tools are used (scratch space for one run).
package misc

import (
	"encoding/json"
	"fmt"
	"strings"

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
)

func decodeArgs(call v1.ToolCall, target any) error {
	raw := strings.TrimSpace(call.Arguments)
	if raw == "" {
		raw = "{}"
	}
	if err := json.Unmarshal([]byte(raw), target); err != nil {
		return fmt.Errorf("invalid arguments JSON: %w", err)
	}
	return nil
}

func textResult(content string) v1.ToolResult {
	return v1.ToolResult{Content: content}
}

func errorResult(format string, args ...any) v1.ToolResult {
	return v1.ToolResult{Content: fmt.Sprintf(format, args...), IsError: true}
}

func objectSchema(properties map[string]any, required ...string) map[string]any {
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

func stringProp(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}
