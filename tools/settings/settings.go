// Package settings provides settings_get and settings_set, the model-facing
// tools over the contracts/rig.Settings key/value surface.
//
// The tools are deliberately thin: keys are arbitrary strings (dot-separated
// names and UI-scoped keys are fine), values are strings, and storage is owned
// by the host. A product passes its file-backed or database-backed rig.Settings;
// passing nil makes both tools return a readable error instead of panicking.
//
// The tools are stateless and safe for concurrent use when the underlying
// Settings implementation is (the contract requires it).
package settings

import (
	"context"
	"fmt"
	"reflect"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// Tool names. They are stable model-facing identifiers.
const (
	ToolSettingsGet = "settings_get"
	ToolSettingsSet = "settings_set"
)

// Tools returns settings_get and settings_set bound to store. A nil store (or a
// typed nil implementing rig.Settings) is reported by every call as a readable
// tool error.
func Tools(store rig.Settings) []rig.Tool {
	return []rig.Tool{
		getTool(store),
		setTool(store),
	}
}

// settingsTool is the common rig.Tool implementation of the settings tools.
type settingsTool struct {
	name        string
	description string
	parameters  map[string]any
	run         func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error)
}

func (t *settingsTool) Name() string           { return t.name }
func (t *settingsTool) Description() string    { return t.description }
func (t *settingsTool) Schema() map[string]any { return t.parameters }
func (t *settingsTool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	return t.run(ctx, call)
}

type getArgs struct {
	Key     string `json:"key"`
	Default string `json:"default"`
}

func getTool(store rig.Settings) rig.Tool {
	return &settingsTool{
		name: ToolSettingsGet,
		description: "Read one setting by key and return its value. When the key is not stored the optional " +
			"default is returned (an empty string when no default is given). Use settings_set to change values.",
		parameters: toolutil.ObjectSchema(map[string]any{
			"key":     toolutil.StringProp("Setting key, for example \"ui.theme\" or \"model\". Any non-empty string is valid."),
			"default": toolutil.StringProp("Value to return when the key is absent (default: empty string)."),
		}, "key"),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args getArgs
			if err := toolutil.DecodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			if args.Key == "" {
				return toolutil.ErrorResultf("settings_get: key is required"), nil
			}
			if isNil(store) {
				return toolutil.ErrorResultf("settings_get: the host did not configure a rig.Settings store"), nil
			}
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			return toolutil.TextResult(store.Get(args.Key, args.Default)), nil
		},
	}
}

type setArgs struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

func setTool(store rig.Settings) rig.Tool {
	return &settingsTool{
		name: ToolSettingsSet,
		description: "Store one setting value by key. The value is a string; the host decides where settings " +
			"persist. Pass an empty value to clear the value (the key keeps its empty value).",
		parameters: toolutil.ObjectSchema(map[string]any{
			"key":   toolutil.StringProp("Setting key, for example \"ui.theme\" or \"model\". Any non-empty string is valid."),
			"value": toolutil.StringProp("New value to store."),
		}, "key", "value"),
		run: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			var args setArgs
			if err := toolutil.DecodeArgs(call, &args); err != nil {
				return rig.ToolResult{}, err
			}
			if args.Key == "" {
				return toolutil.ErrorResultf("settings_set: key is required"), nil
			}
			if isNil(store) {
				return toolutil.ErrorResultf("settings_set: the host did not configure a rig.Settings store"), nil
			}
			if err := ctx.Err(); err != nil {
				return rig.ToolResult{}, err
			}
			if err := store.Set(args.Key, args.Value); err != nil {
				return toolutil.ErrorResultf("settings_set: %v", err), nil
			}
			return toolutil.TextResult(fmt.Sprintf("set %q", args.Key)), nil
		},
	}
}

// --- shared helpers --------------------------------------------------------

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
