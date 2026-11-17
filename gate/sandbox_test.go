package gate_test

import (
	"context"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/gate"
)

// sandboxStubTool 是审批测试用的最小工具（避免与其它测试文件的 stub 冲突）。
type sandboxStubTool struct{ name string }

func (t sandboxStubTool) Name() string           { return t.name }
func (t sandboxStubTool) Description() string    { return "" }
func (t sandboxStubTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (t sandboxStubTool) Invoke(context.Context, rig.ToolCall) (rig.ToolResult, error) {
	return rig.ToolResult{}, nil
}

// countingGate 记录被委托的调用次数。
type countingGate struct {
	calls    int
	decision rig.Decision
}

func (g *countingGate) Approve(context.Context, rig.Tool, rig.ToolCall) (rig.Decision, error) {
	g.calls++
	return g.decision, nil
}

func TestAllowThenAskAllowsListedTools(t *testing.T) {
	inner := &countingGate{decision: rig.DecisionAllow}
	composed := gate.AllowThenAsk([]string{"sandbox_status", "read_file"}, inner)

	decision, err := composed.Approve(context.Background(), sandboxStubTool{name: "sandbox_status"}, rig.ToolCall{Name: "sandbox_status"})
	if err != nil || decision != rig.DecisionAllow {
		t.Fatalf("decision = %q, err = %v; want allow", decision, err)
	}
	if inner.calls != 0 {
		t.Fatalf("inner gate calls = %d, want 0 for an allowlisted tool", inner.calls)
	}
}

func TestAllowThenAskDelegatesEverythingElse(t *testing.T) {
	inner := &countingGate{decision: rig.DecisionDeny}
	composed := gate.AllowThenAsk(gate.SandboxReadOnlyTools, inner)

	decision, err := composed.Approve(context.Background(), sandboxStubTool{name: "request_sandbox_access"}, rig.ToolCall{Name: "request_sandbox_access"})
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if decision != rig.DecisionDeny {
		t.Fatalf("decision = %q, want the inner decision", decision)
	}
	if inner.calls != 1 {
		t.Fatalf("inner gate calls = %d, want 1", inner.calls)
	}
}

func TestAllowThenAskWithoutInnerFailsClosed(t *testing.T) {
	composed := gate.AllowThenAsk(nil, nil)
	// fail closed = deny + 理由（不是 error）：error 会让 agent 中止整个 run。
	decision, err := composed.Approve(context.Background(), sandboxStubTool{name: "bash"}, rig.ToolCall{Name: "bash"})
	if err != nil {
		t.Fatalf("缺少 inner 应当 deny 而不是 error: %v", err)
	}
	if decision != rig.DecisionDeny {
		t.Fatalf("decision = %q, want deny", decision)
	}
	explainer, ok := composed.(interface {
		ExplainDenial(rig.Tool, rig.ToolCall) string
	})
	if !ok || !strings.Contains(explainer.ExplainDenial(sandboxStubTool{name: "bash"}, rig.ToolCall{Name: "bash"}), "没有配置审批通道") {
		t.Fatalf("拒绝理由应说明没有审批通道")
	}
}

func TestSandboxReadOnlyToolsContainsNoFootprintTools(t *testing.T) {
	allowed := make(map[string]bool, len(gate.SandboxReadOnlyTools))
	for _, name := range gate.SandboxReadOnlyTools {
		allowed[name] = true
	}
	if !allowed["sandbox_status"] {
		t.Fatal("sandbox_status must be auto-allowable: it is read-only")
	}
	for _, risky := range []string{"bash", "pwsh", "write_file", "edit_file", "request_sandbox_access", "job_start", "terminal_send"} {
		if allowed[risky] {
			t.Fatalf("%q must never be auto-allowed by SandboxReadOnlyTools", risky)
		}
	}
}

func TestSandboxGrantPromptRendersPathAndReason(t *testing.T) {
	call := rig.ToolCall{
		Name:      "request_sandbox_access",
		Arguments: `{"path":"/home/me/.some-tool","reason":"the CLI keeps its OAuth credentials here"}`,
	}
	prompt := gate.SandboxGrantPrompt(sandboxStubTool{name: "request_sandbox_access"}, call)
	for _, want := range []string{"/home/me/.some-tool", "the CLI keeps its OAuth credentials here", "writable surface"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestSandboxGrantPromptFallsBackForOtherTools(t *testing.T) {
	if prompt := gate.SandboxGrantPrompt(sandboxStubTool{name: "bash"}, rig.ToolCall{Name: "bash", Arguments: `{}`}); prompt != "" {
		t.Fatalf("prompt = %q, want empty for non-grant tools", prompt)
	}
	// 畸形参数不应让审批文案消失或 panic。
	if prompt := gate.SandboxGrantPrompt(sandboxStubTool{name: "request_sandbox_access"}, rig.ToolCall{Name: "request_sandbox_access", Arguments: `not-json`}); prompt != "" {
		t.Fatalf("prompt = %q, want empty for malformed arguments", prompt)
	}
}
