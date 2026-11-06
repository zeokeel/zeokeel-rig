// Package settings provides settings_get and settings_set, the model-facing
// tools over the contracts/v1.Settings key/value surface.
//
// The tools are deliberately thin: keys are arbitrary strings (dot-separated
// names and UI-scoped keys are fine), values are strings, and storage is owned
// by the host. A product passes its file-backed or database-backed v1.Settings;
// passing nil makes both tools return a readable error instead of panicking.
//
// The tools are stateless and safe for concurrent use when the underlying
// Settings implementation is (the contract requires it).
package settings

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
)

// Tool names. They are stable model-facing identifiers.
const (
	ToolSettingsGet = "settings_get"
	ToolSettingsSet = "settings_set"
)

// Tools returns settings_get and settings_set bound to store. A nil store (or a
// typed nil implementing v1.Settings) is reported by every call as a readable
// tool error.
func Tools(store v1.Settings) []v1.Tool {
	return []v1.Tool{
		getTool(store),
		setTool(store),
	}
}

// settingsTool is the common v1.Tool implementation of the settings tools.
type settingsTool struct {
	name        string
	description string
	parameters  map[string]any
	run         func(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error)
}

func (t *settingsTool) Name() string           { return t.name }
func (t *settingsTool) Description() string    { return t.description }
func (t *settingsTool) Schema() map[string]any { return t.parameters }
func (t *settingsTool) Invoke(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error) {
	return t.run(ctx, call)
}

type getArgs struct {
	Key     string `json:"key"`
	Default string `json:"default"`
}

func getTool(store v1.Settings) v1.Tool {
	return &settingsTool{
		name: ToolSettingsGet,
		description: "Read one setting by key and return its value. When the key is not stored the optional " +
			"default is returned (an empty string when no default is given). Use settings_set to change values.",
		parameters: objectSchema(map[string]any{
			"key":     stringProp("Setting key, for example \"ui.theme\" or \"model\". Any non-empty string is valid."),
			"default": stringProp("Value to return when the key is absent (default: empty string)."),
		}, "key"),
		run: func(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error) {
			var args getArgs
			if err := decodeArgs(call, &args); err != nil {
				return v1.ToolResult{}, err
			}
			if args.Key == "" {
				return errorResult("settings_get: key is required"), nil
			}
			if isNil(store) {
				return errorResult("settings_get: the host did not configure a v1.Settings store"), nil
			}
			if err := ctx.Err(); err != nil {
				return v1.ToolResult{}, err
			}
			return textResult(store.Get(args.Key, args.Default)), nil
		},
	}
}

type setArgs struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

func setTool(store v1.Settings) v1.Tool {
	return &settingsTool{
		name: ToolSettingsSet,
		description: "Store one setting value by key. The value is a string; the host decides where settings " +
			"persist. Pass an empty value to clear the value (the key keeps its empty value).",
		parameters: objectSchema(map[string]any{
			"key":   stringProp("Setting key, for example \"ui.theme\" or \"model\". Any non-empty string is valid."),
			"value": stringProp("New value to store."),
		}, "key", "value"),
		run: func(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error) {
			var args setArgs
			if err := decodeArgs(call, &args); err != nil {
				return v1.ToolResult{}, err
			}
			if args.Key == "" {
				return errorResult("settings_set: key is required"), nil
			}
			if isNil(store) {
				return errorResult("settings_set: the host did not configure a v1.Settings store"), nil
			}
			if err := ctx.Err(); err != nil {
				return v1.ToolResult{}, err
			}
			if err := store.Set(args.Key, args.Value); err != nil {
				return errorResult("settings_set: %v", err), nil
			}
			return textResult(fmt.Sprintf("set %q", args.Key)), nil
		},
	}
}

// --- shared helpers --------------------------------------------------------

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

func textResult(content string) v1.ToolResult { return v1.ToolResult{Content: content} }

func errorResult(format string, args ...any) v1.ToolResult {
	return v1.ToolResult{Content: fmt.Sprintf(format, args...), IsError: true}
}

// isNil reports whether an interface value is nil, including a typed nil
// pointer that would panic when its method is called.
func isNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
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
