// Package tool provides the tool registry that turns a set of v1.Tool
// implementations into the registry the agent invokes: name-based lookup,
// deterministic specs for the model, and a concurrency-safe snapshot API.
//
// A registry is a compile-time set (tools are linked into the binary); it holds
// no state beyond the tool values themselves, so it is safe for concurrent
// Invoke/Specs/Register calls. Tools are expected to be stateless or
// internally synchronized (see v1.Tool).
package tool

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
)

// Registry is an explicit, concurrency-safe set of tools keyed by name.
//
// The zero value is not usable; construct it with NewRegistry or &Registry{}.
// (The zero value is usable too once fields are initialized by Register, but
// NewRegistry is the documented constructor.)
type Registry struct {
	mu    sync.RWMutex
	tools map[string]v1.Tool
}

// NewRegistry creates a registry containing tools. Invalid or duplicate tools
// are programming errors on the compile-time registration path, so NewRegistry
// panics instead of silently dropping them; use Register when the tool set is
// not static and the error must be handled.
func NewRegistry(tools ...v1.Tool) *Registry {
	registry := &Registry{tools: make(map[string]v1.Tool, len(tools))}
	for _, t := range tools {
		if err := registry.Register(t); err != nil {
			panic("tool: NewRegistry: " + err.Error())
		}
	}
	return registry
}

// Register adds one tool. A nil tool, a tool with an empty name, or a duplicate
// name is an error; the registry is unchanged on error.
func (r *Registry) Register(t v1.Tool) error {
	if toolIsNil(t) {
		return fmt.Errorf("tool: nil tool")
	}
	name := t.Name()
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("tool: tool with an empty name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tools == nil {
		r.tools = make(map[string]v1.Tool)
	}
	if _, exists := r.tools[name]; exists {
		return fmt.Errorf("tool: duplicate tool name %q", name)
	}
	r.tools[name] = t
	return nil
}

// Specs returns the model-facing specs of every tool, sorted by name. The
// returned slice and the schema maps are the tools' own values; callers must
// not mutate them (the registry is shared).
func (r *Registry) Specs() []v1.ToolSpec {
	tools := r.Tools()
	specs := make([]v1.ToolSpec, 0, len(tools))
	for _, t := range tools {
		specs = append(specs, v1.ToolSpec{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  t.Schema(),
		})
	}
	return specs
}

// Invoke runs one tool call. The arguments string is passed to the tool exactly
// as the model produced it (no re-encoding). An unknown tool name returns an
// error wrapping v1.ErrToolNotFound.
func (r *Registry) Invoke(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error) {
	r.mu.RLock()
	t, ok := r.tools[call.Name]
	r.mu.RUnlock()
	if !ok {
		return v1.ToolResult{}, fmt.Errorf("%w: %q", v1.ErrToolNotFound, call.Name)
	}
	return t.Invoke(ctx, call)
}

// Tools returns every registered tool, sorted by name. The returned slice is a
// snapshot: mutating it does not affect the registry. Tools implements
// v1.ToolLister.
func (r *Registry) Tools() []v1.Tool {
	r.mu.RLock()
	tools := make([]v1.Tool, 0, len(r.tools))
	for _, t := range r.tools {
		tools = append(tools, t)
	}
	r.mu.RUnlock()
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name() < tools[j].Name() })
	return tools
}

// Len returns the number of registered tools.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.tools)
}

// toolIsNil reports whether t is nil, including a typed nil pointer that would
// otherwise panic when a method is called through the interface.
func toolIsNil(t v1.Tool) bool {
	if t == nil {
		return true
	}
	value := reflect.ValueOf(t)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// Compile-time check: the registry satisfies the contract's lister interface.
var _ v1.ToolLister = (*Registry)(nil)
