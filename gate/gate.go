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

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
)

// funcGate adapts a function to v1.Gate.
type funcGate struct {
	fn func(ctx context.Context, tool v1.Tool, call v1.ToolCall) (v1.Decision, error)
}

// Approve implements v1.Gate. A nil function fails closed with an error, and an
// empty decision from the function is treated as a denial (fail closed).
func (g funcGate) Approve(ctx context.Context, tool v1.Tool, call v1.ToolCall) (v1.Decision, error) {
	if g.fn == nil {
		return "", fmt.Errorf("gate: nil approval function")
	}
	decision, err := g.fn(ctx, tool, call)
	if err != nil {
		return "", err
	}
	if decision == "" {
		return v1.DecisionDeny, nil
	}
	return decision, nil
}

// AllowAll returns a gate that allows every call. It is the trusted default for
// tools that only read state; it must not guard tools with side effects.
func AllowAll() v1.Gate {
	return funcGate{fn: func(context.Context, v1.Tool, v1.ToolCall) (v1.Decision, error) {
		return v1.DecisionAllow, nil
	}}
}

// DenyAll returns a gate that denies every call. The agent reports the denial
// to the model (or fails, depending on its configuration).
func DenyAll() v1.Gate {
	return funcGate{fn: func(context.Context, v1.Tool, v1.ToolCall) (v1.Decision, error) {
		return v1.DecisionDeny, nil
	}}
}

// Func returns a gate backed by fn. fn is called on the agent run's goroutine
// and may block. A nil fn produces a gate that errors on every call; the return
// values are passed through, except an empty decision which fails closed.
func Func(fn func(ctx context.Context, tool v1.Tool, call v1.ToolCall) (v1.Decision, error)) v1.Gate {
	return funcGate{fn: fn}
}

// AllowTools returns a gate that allows calls to the named tools and denies
// every other call. Names are compared exactly with Tool.Name(); blank names are
// ignored. Calling it with no names is equivalent to DenyAll.
func AllowTools(names ...string) v1.Gate {
	allowed := make(map[string]struct{}, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		allowed[name] = struct{}{}
	}
	return funcGate{fn: func(_ context.Context, tool v1.Tool, call v1.ToolCall) (v1.Decision, error) {
		name := call.Name
		if tool != nil {
			name = tool.Name()
		}
		if _, ok := allowed[name]; ok {
			return v1.DecisionAllow, nil
		}
		return v1.DecisionDeny, nil
	}}
}

// Compile-time check: the built-in gates implement the contract.
var _ v1.Gate = funcGate{}
