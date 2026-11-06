package gate_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
	"github.com/zeokeel/zeokeel-rig/gate"
)

// stubTool is the minimal v1.Tool a gate inspects.
type stubTool struct{ name string }

func (s stubTool) Name() string           { return s.name }
func (s stubTool) Description() string    { return "" }
func (s stubTool) Schema() map[string]any { return nil }
func (s stubTool) Invoke(context.Context, v1.ToolCall) (v1.ToolResult, error) {
	return v1.ToolResult{}, nil
}

func approve(t *testing.T, g v1.Gate, tool v1.Tool, call v1.ToolCall) (v1.Decision, error) {
	t.Helper()
	return g.Approve(context.Background(), tool, call)
}

func TestAllowAllAndDenyAll(t *testing.T) {
	call := v1.ToolCall{Name: "bash", Arguments: `{"command":"rm -rf /"}`}
	tool := stubTool{name: "bash"}

	decision, err := approve(t, gate.AllowAll(), tool, call)
	if err != nil || decision != v1.DecisionAllow {
		t.Fatalf("AllowAll = (%q, %v), want (allow, nil)", decision, err)
	}
	decision, err = approve(t, gate.DenyAll(), tool, call)
	if err != nil || decision != v1.DecisionDeny {
		t.Fatalf("DenyAll = (%q, %v), want (deny, nil)", decision, err)
	}
	// The decision is independent of the tool and call.
	decision, _ = approve(t, gate.AllowAll(), nil, v1.ToolCall{})
	if decision != v1.DecisionAllow {
		t.Fatalf("AllowAll(nil tool) = %q, want allow", decision)
	}
}

func TestAllowTools(t *testing.T) {
	g := gate.AllowTools("read_file", " glob ", "", "read_file")
	cases := []struct {
		name string
		tool v1.Tool
		call v1.ToolCall
		want v1.Decision
	}{
		{"allowed by tool name", stubTool{name: "read_file"}, v1.ToolCall{Name: "read_file"}, v1.DecisionAllow},
		{"allowed with spaces trimmed", stubTool{name: "glob"}, v1.ToolCall{Name: "glob"}, v1.DecisionAllow},
		{"denied", stubTool{name: "bash"}, v1.ToolCall{Name: "bash"}, v1.DecisionDeny},
		{"denied unknown", stubTool{name: "write_file"}, v1.ToolCall{Name: "write_file"}, v1.DecisionDeny},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			decision, err := approve(t, g, testCase.tool, testCase.call)
			if err != nil {
				t.Fatalf("Approve: %v", err)
			}
			if decision != testCase.want {
				t.Fatalf("decision = %q, want %q", decision, testCase.want)
			}
		})
	}
	if decision, err := approve(t, gate.AllowTools(), stubTool{name: "any"}, v1.ToolCall{Name: "any"}); err != nil || decision != v1.DecisionDeny {
		t.Fatalf("AllowTools() = (%q, %v), want (deny, nil)", decision, err)
	}
}

func TestFuncPassesThroughAndFailsClosed(t *testing.T) {
	var sawArgs string
	g := gate.Func(func(_ context.Context, tool v1.Tool, call v1.ToolCall) (v1.Decision, error) {
		sawArgs = call.Arguments
		if tool.Name() == "bash" {
			return v1.DecisionAllowAlways, nil
		}
		return v1.DecisionDeny, nil
	})
	decision, err := approve(t, g, stubTool{name: "bash"}, v1.ToolCall{Name: "bash", Arguments: `{"command":"ls"}`})
	if err != nil || decision != v1.DecisionAllowAlways {
		t.Fatalf("Func(bash) = (%q, %v), want (allow-always, nil)", decision, err)
	}
	if sawArgs != `{"command":"ls"}` {
		t.Fatalf("gate saw arguments %q, want the raw JSON", sawArgs)
	}
	decision, _ = approve(t, g, stubTool{name: "write_file"}, v1.ToolCall{Name: "write_file"})
	if decision != v1.DecisionDeny {
		t.Fatalf("Func(write_file) = %q, want deny", decision)
	}

	// An empty decision fails closed.
	empty := gate.Func(func(context.Context, v1.Tool, v1.ToolCall) (v1.Decision, error) { return "", nil })
	if decision, err = approve(t, empty, stubTool{name: "x"}, v1.ToolCall{Name: "x"}); err != nil || decision != v1.DecisionDeny {
		t.Fatalf("Func(empty) = (%q, %v), want (deny, nil)", decision, err)
	}
	// Errors pass through.
	boom := errors.New("approval UI unavailable")
	failing := gate.Func(func(context.Context, v1.Tool, v1.ToolCall) (v1.Decision, error) { return "", boom })
	if _, err := approve(t, failing, stubTool{name: "x"}, v1.ToolCall{Name: "x"}); !errors.Is(err, boom) {
		t.Fatalf("Func(error) = %v, want the gate's error", err)
	}
	// A nil function errors on every call instead of panicking.
	if _, err := approve(t, gate.Func(nil), stubTool{name: "x"}, v1.ToolCall{Name: "x"}); err == nil {
		t.Fatal("Func(nil) approved a call")
	}
}

func TestGatesAreConcurrencySafe(t *testing.T) {
	tool := stubTool{name: "read_file"}
	call := v1.ToolCall{Name: "read_file", Arguments: `{}`}
	gates := []v1.Gate{
		gate.AllowAll(),
		gate.DenyAll(),
		gate.AllowTools("read_file"),
		gate.Func(func(_ context.Context, _ v1.Tool, _ v1.ToolCall) (v1.Decision, error) {
			return v1.DecisionAllow, nil
		}),
	}
	var wg sync.WaitGroup
	for _, g := range gates {
		g := g
		for worker := 0; worker < 8; worker++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for index := 0; index < 50; index++ {
					if _, err := approve(t, g, tool, call); err != nil {
						t.Errorf("Approve: %v", err)
						return
					}
				}
			}()
		}
	}
	wg.Wait()
}
