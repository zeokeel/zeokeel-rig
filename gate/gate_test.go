package gate_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/gate"
)

// stubTool is the minimal rig.Tool a gate inspects.
type stubTool struct{ name string }

func (s stubTool) Name() string           { return s.name }
func (s stubTool) Description() string    { return "" }
func (s stubTool) Schema() map[string]any { return nil }
func (s stubTool) Invoke(context.Context, rig.ToolCall) (rig.ToolResult, error) {
	return rig.ToolResult{}, nil
}

func approve(t *testing.T, g rig.Gate, tool rig.Tool, call rig.ToolCall) (rig.Decision, error) {
	t.Helper()
	return g.Approve(context.Background(), tool, call)
}

func TestAllowAllAndDenyAll(t *testing.T) {
	call := rig.ToolCall{Name: "bash", Arguments: `{"command":"rm -rf /"}`}
	tool := stubTool{name: "bash"}

	decision, err := approve(t, gate.AllowAll(), tool, call)
	if err != nil || decision != rig.DecisionAllow {
		t.Fatalf("AllowAll = (%q, %v), want (allow, nil)", decision, err)
	}
	decision, err = approve(t, gate.DenyAll(), tool, call)
	if err != nil || decision != rig.DecisionDeny {
		t.Fatalf("DenyAll = (%q, %v), want (deny, nil)", decision, err)
	}
	// The decision is independent of the tool and call.
	decision, _ = approve(t, gate.AllowAll(), nil, rig.ToolCall{})
	if decision != rig.DecisionAllow {
		t.Fatalf("AllowAll(nil tool) = %q, want allow", decision)
	}
}

func TestAllowTools(t *testing.T) {
	g := gate.AllowTools("read_file", " glob ", "", "read_file")
	cases := []struct {
		name string
		tool rig.Tool
		call rig.ToolCall
		want rig.Decision
	}{
		{"allowed by tool name", stubTool{name: "read_file"}, rig.ToolCall{Name: "read_file"}, rig.DecisionAllow},
		{"allowed with spaces trimmed", stubTool{name: "glob"}, rig.ToolCall{Name: "glob"}, rig.DecisionAllow},
		{"denied", stubTool{name: "bash"}, rig.ToolCall{Name: "bash"}, rig.DecisionDeny},
		{"denied unknown", stubTool{name: "write_file"}, rig.ToolCall{Name: "write_file"}, rig.DecisionDeny},
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
	if decision, err := approve(t, gate.AllowTools(), stubTool{name: "any"}, rig.ToolCall{Name: "any"}); err != nil || decision != rig.DecisionDeny {
		t.Fatalf("AllowTools() = (%q, %v), want (deny, nil)", decision, err)
	}
}

func TestFuncPassesThroughAndFailsClosed(t *testing.T) {
	var sawArgs string
	g := gate.Func(func(_ context.Context, tool rig.Tool, call rig.ToolCall) (rig.Decision, error) {
		sawArgs = call.Arguments
		if tool.Name() == "bash" {
			return rig.DecisionAllowAlways, nil
		}
		return rig.DecisionDeny, nil
	})
	decision, err := approve(t, g, stubTool{name: "bash"}, rig.ToolCall{Name: "bash", Arguments: `{"command":"ls"}`})
	if err != nil || decision != rig.DecisionAllowAlways {
		t.Fatalf("Func(bash) = (%q, %v), want (allow-always, nil)", decision, err)
	}
	if sawArgs != `{"command":"ls"}` {
		t.Fatalf("gate saw arguments %q, want the raw JSON", sawArgs)
	}
	decision, _ = approve(t, g, stubTool{name: "write_file"}, rig.ToolCall{Name: "write_file"})
	if decision != rig.DecisionDeny {
		t.Fatalf("Func(write_file) = %q, want deny", decision)
	}

	// An empty decision fails closed.
	empty := gate.Func(func(context.Context, rig.Tool, rig.ToolCall) (rig.Decision, error) { return "", nil })
	if decision, err = approve(t, empty, stubTool{name: "x"}, rig.ToolCall{Name: "x"}); err != nil || decision != rig.DecisionDeny {
		t.Fatalf("Func(empty) = (%q, %v), want (deny, nil)", decision, err)
	}
	// Errors pass through.
	boom := errors.New("approval UI unavailable")
	failing := gate.Func(func(context.Context, rig.Tool, rig.ToolCall) (rig.Decision, error) { return "", boom })
	if _, err := approve(t, failing, stubTool{name: "x"}, rig.ToolCall{Name: "x"}); !errors.Is(err, boom) {
		t.Fatalf("Func(error) = %v, want the gate's error", err)
	}
	// A nil function errors on every call instead of panicking.
	if _, err := approve(t, gate.Func(nil), stubTool{name: "x"}, rig.ToolCall{Name: "x"}); err == nil {
		t.Fatal("Func(nil) approved a call")
	}
}

func TestGatesAreConcurrencySafe(t *testing.T) {
	tool := stubTool{name: "read_file"}
	call := rig.ToolCall{Name: "read_file", Arguments: `{}`}
	gates := []rig.Gate{
		gate.AllowAll(),
		gate.DenyAll(),
		gate.AllowTools("read_file"),
		gate.Func(func(_ context.Context, _ rig.Tool, _ rig.ToolCall) (rig.Decision, error) {
			return rig.DecisionAllow, nil
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
