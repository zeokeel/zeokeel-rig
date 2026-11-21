package discovery

import (
	"strings"

	"github.com/zeokeel/zeokeel-rig"
)

// Activation describes which registered tools a request starts with when lazy
// discovery is on: everything except the hidden tail, plus forced names the host
// decided must stay active (an explicit preset grant, a tool the UI pinned).
//
// A tool the host list does not know is active: discovery must never silently
// hide a newly registered or custom tool just because a deny list was stale.
type Activation struct {
	// Tail is hidden by exact name.
	Tail []string
	// TailPrefixes hides every tool whose name starts with the prefix
	// (for example "mcp__" for MCP servers).
	TailPrefixes []string
	// Forced overrides Tail/TailPrefixes (names still must be registered).
	Forced []string
}

// Hidden reports whether the tool name is part of the hidden tail.
func (a Activation) Hidden(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	for _, forced := range a.Forced {
		if name == strings.TrimSpace(forced) {
			return false
		}
	}
	for _, prefix := range a.TailPrefixes {
		if prefix = strings.TrimSpace(prefix); prefix != "" && strings.HasPrefix(name, prefix) {
			return true
		}
	}
	for _, tail := range a.Tail {
		if name == strings.TrimSpace(tail) {
			return true
		}
	}
	return false
}

// ActiveNames returns the names a request starts with: registered order, hidden
// tail removed, forced names kept, no duplicates.
func ActiveNames(tools []rig.Tool, a Activation) []string {
	names := make([]string, 0, len(tools))
	seen := make(map[string]bool, len(tools))
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		name := strings.TrimSpace(tool.Name())
		if name == "" || seen[name] || a.Hidden(name) {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}
