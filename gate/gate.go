// Package gate provides the built-in approval gates for tool calls: allow
// everything, deny everything, delegate to a function, or allow an explicit
// allow-list of tool names.
//
// A Gate is called by the agent before a tool runs; it may block on user input
// (that is the product's approval UI) and is safe for concurrent use because
// the built-in gates are stateless.
package gate

import (
	"context"
	"fmt"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
)

// funcGate adapts a function to rig.Gate.
type funcGate struct {
	fn func(ctx context.Context, tool rig.Tool, call rig.ToolCall) (rig.Decision, error)
}

// Approve implements rig.Gate. A nil function fails closed with an error, and an
// empty decision from the function is treated as a denial (fail closed).
func (g funcGate) Approve(ctx context.Context, tool rig.Tool, call rig.ToolCall) (rig.Decision, error) {
	if g.fn == nil {
		return "", fmt.Errorf("gate: nil approval function")
	}
	decision, err := g.fn(ctx, tool, call)
	if err != nil {
		return "", err
	}
	if decision == "" {
		return rig.DecisionDeny, nil
	}
	return decision, nil
}

// AllowAll returns a gate that allows every call. It is the trusted default for
// tools that only read state; it must not guard tools with side effects.
func AllowAll() rig.Gate {
	return funcGate{fn: func(context.Context, rig.Tool, rig.ToolCall) (rig.Decision, error) {
		return rig.DecisionAllow, nil
	}}
}

// DenyAll returns a gate that denies every call. The agent reports the denial
// to the model (or fails, depending on its configuration).
func DenyAll() rig.Gate {
	return funcGate{fn: func(context.Context, rig.Tool, rig.ToolCall) (rig.Decision, error) {
		return rig.DecisionDeny, nil
	}}
}

// Func returns a gate backed by fn. fn is called on the agent run's goroutine
// and may block. A nil fn produces a gate that errors on every call; the return
// values are passed through, except an empty decision which fails closed.
func Func(fn func(ctx context.Context, tool rig.Tool, call rig.ToolCall) (rig.Decision, error)) rig.Gate {
	return funcGate{fn: fn}
}

// AllowTools returns a gate that allows calls to the named tools and denies
// every other call. Names are compared exactly with Tool.Name(); blank names are
// ignored. Calling it with no names is equivalent to DenyAll.
func AllowTools(names ...string) rig.Gate {
	allowed := make(map[string]struct{}, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		allowed[name] = struct{}{}
	}
	return funcGate{fn: func(_ context.Context, tool rig.Tool, call rig.ToolCall) (rig.Decision, error) {
		name := call.Name
		if tool != nil {
			name = tool.Name()
		}
		if _, ok := allowed[name]; ok {
			return rig.DecisionAllow, nil
		}
		return rig.DecisionDeny, nil
	}}
}

// Compile-time check: the built-in gates implement the contract.
var _ rig.Gate = funcGate{}
