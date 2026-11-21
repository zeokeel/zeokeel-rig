// Package surface filters tool surfaces by name.
//
// A host rarely hands the model every registered tool: disabled tools, preset
// allowlists, mode whitelists, child exclusions and lazy-discovery tails all
// remove names. The operations are small but easy to get subtly wrong (nil
// tools, whitespace, order), so they live here once and keep registration order.
package surface

import (
	"strings"

	"github.com/zeokeel/zeokeel-rig"
)

// Names returns the non-empty trimmed names of the tools, in order.
func Names(tools []rig.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		if name := strings.TrimSpace(tool.Name()); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// NameSet returns the set of non-empty trimmed tool names.
func NameSet(tools []rig.Tool) map[string]bool {
	set := make(map[string]bool, len(tools))
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		if name := strings.TrimSpace(tool.Name()); name != "" {
			set[name] = true
		}
	}
	return set
}

// WithoutNames drops the named tools, preserving registration order. An empty
// exclusion list returns the input untouched.
func WithoutNames(tools []rig.Tool, names []string) []rig.Tool {
	if len(names) == 0 {
		return tools
	}
	excluded := make(map[string]bool, len(names))
	for _, name := range names {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			excluded[trimmed] = true
		}
	}
	if len(excluded) == 0 {
		return tools
	}
	filtered := make([]rig.Tool, 0, len(tools))
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		if excluded[strings.TrimSpace(tool.Name())] {
			continue
		}
		filtered = append(filtered, tool)
	}
	return filtered
}

// OnlyNames keeps the named tools (plus any keep names), preserving registration
// order. An empty allow list returns the input untouched.
func OnlyNames(tools []rig.Tool, allow []string, keep ...string) []rig.Tool {
	if len(allow) == 0 {
		return tools
	}
	allowed := make(map[string]bool, len(allow)+len(keep))
	for _, name := range allow {
		allowed[strings.TrimSpace(name)] = true
	}
	for _, name := range keep {
		allowed[strings.TrimSpace(name)] = true
	}
	filtered := make([]rig.Tool, 0, len(tools))
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		if allowed[strings.TrimSpace(tool.Name())] {
			filtered = append(filtered, tool)
		}
	}
	return filtered
}
