package gate_test

// "已沙箱化的命令免审批"（AllowSandboxedCommands）的条件表。
//
// 免问只在**全部**条件成立时发生；任何一条不成立都必须原样交回 inner（照旧问人）。
// 这个文件把每条条件单独钉住——只有组合测试的话，某一条悄悄失效时很难看出来。

import (
	"context"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/gate"
	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// sandboxFakeRunner 是 sandbox.Runner 的测试替身（可配置自报的覆盖程度）。
type sandboxFakeRunner struct {
	enforcement sandbox.Enforcement
	confined    int
}

func (r *sandboxFakeRunner) Confine(argv []string, policy *sandbox.Policy) (sandbox.ConfinedCommand, error) {
	r.confined++
	return sandbox.ConfinedCommand{Argv: append([]string(nil), argv...), Enforcement: r.enforcement}, nil
}

func (r *sandboxFakeRunner) RunnerName() string                     { return "fake" }
func (r *sandboxFakeRunner) RunnerEnforcement() sandbox.Enforcement { return r.enforcement }
func (r *sandboxFakeRunner) RunnerLimits() string                   { return "" }

// sandboxCountingGate 记录 inner 被调用次数并放行。
type sandboxCountingGate struct{ calls int }

func (g *sandboxCountingGate) Approve(ctx context.Context, tool rig.Tool, call rig.ToolCall) (rig.Decision, error) {
	g.calls++
	return rig.DecisionAllow, nil
}

// sandboxShellTool 是最小的 shell 工具替身。
type sandboxShellTool struct{ name string }

func (t *sandboxShellTool) Name() string           { return t.name }
func (t *sandboxShellTool) Description() string    { return "shell" }
func (t *sandboxShellTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (t *sandboxShellTool) Invoke(context.Context, rig.ToolCall) (rig.ToolResult, error) {
	return rig.ToolResult{}, nil
}

// decide 跑一次门禁判定。
func sandboxDecide(t *testing.T, g rig.Gate, name, arguments string) rig.Decision {
	t.Helper()
	decision, err := g.Approve(context.Background(), &sandboxShellTool{name: name}, rig.ToolCall{
		ID: "c1", Name: name, Arguments: arguments,
	})
	if err != nil {
		t.Fatalf("Approve(%q): %v", name, err)
	}
	return decision
}

// TestAllowSandboxedCommandsConditions 是条件表的正面（全部满足 → 免问）。
func TestAllowSandboxedCommandsConditions(t *testing.T) {
	dir := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(dir), sandbox.WithMode(sandbox.ModeWorkspaceWrite), sandbox.WithAnyCommandAllowed(true))
	inner := &sandboxCountingGate{}
	var observed []gate.AskEvent
	g := gate.AllowSandboxedCommands(gate.SandboxedOptions{
		Runner:   &sandboxFakeRunner{enforcement: sandbox.EnforcementFull},
		Policy:   policy,
		Observer: func(event gate.AskEvent) { observed = append(observed, event) },
	}, inner)

	if decision := sandboxDecide(t, g, "bash", `{"command":"mkdir -p build"}`); decision != rig.DecisionAllow {
		t.Fatalf("decision = %q, want allow", decision)
	}
	if inner.calls != 0 {
		t.Fatalf("免问时不该调用 inner，实际调用 %d 次", inner.calls)
	}
	if len(observed) != 1 {
		t.Fatalf("观察点 = %d, want 1（免问也要留痕）", len(observed))
	}
	if observed[0].Reason != gate.SandboxedAllowReason || observed[0].Decision != rig.DecisionAllow {
		t.Fatalf("观察点 = %+v", observed[0])
	}
	if observed[0].Stage != gate.AskStageDecided {
		t.Fatalf("stage = %q, want decided（没有在等谁）", observed[0].Stage)
	}

	// DSH 对齐：命令文本不再参与免问判定。围栏已生效时，"看起来危险"的命令与
	// 普通命令一样免问（危险与否由人/策略层决定，不由规则表决定）。
	inner2 := &sandboxCountingGate{}
	g2 := gate.AllowSandboxedCommands(gate.SandboxedOptions{
		Runner: &sandboxFakeRunner{enforcement: sandbox.EnforcementFull},
		Policy: policy,
	}, inner2)
	for _, args := range []string{`{"command":"rm -rf /"}`, `{}`, `{"command":`} {
		if decision := sandboxDecide(t, g2, "bash", args); decision != rig.DecisionAllow {
			t.Fatalf("args=%s: decision=%q, want allow（只看是否已隔离）", args, decision)
		}
	}
	if inner2.calls != 0 {
		t.Fatalf("免问时不该调用 inner，实际调用 %d 次", inner2.calls)
	}
}

// TestAllowSandboxedCommandsFallsThrough 是条件表的反面：任何一条不满足都交给 inner。
func TestAllowSandboxedCommandsFallsThrough(t *testing.T) {
	dir := t.TempDir()
	workspaceWrite := sandbox.New(sandbox.WithRoots(dir), sandbox.WithMode(sandbox.ModeWorkspaceWrite), sandbox.WithAnyCommandAllowed(true))
	dangerFull := sandbox.New(sandbox.WithRoots(dir), sandbox.WithMode(sandbox.ModeDangerFullAccess), sandbox.WithAnyCommandAllowed(true))

	cases := []struct {
		name    string
		opts    gate.SandboxedOptions
		tool    string
		args    string
		explain string
	}{
		{
			name:    "没有 runner",
			opts:    gate.SandboxedOptions{Policy: workspaceWrite},
			tool:    "bash",
			args:    `{"command":"mkdir -p build"}`,
			explain: "没有围栏就没有免问的理由",
		},
		{
			name:    "runner 自报 none（不强制）",
			opts:    gate.SandboxedOptions{Runner: &sandboxFakeRunner{enforcement: sandbox.EnforcementNone}, Policy: workspaceWrite},
			tool:    "bash",
			args:    `{"command":"mkdir -p build"}`,
			explain: "runner 不能强制时等于没有围栏",
		},
		{
			name:    "没有策略",
			opts:    gate.SandboxedOptions{Runner: &sandboxFakeRunner{enforcement: sandbox.EnforcementFull}},
			tool:    "bash",
			args:    `{"command":"mkdir -p build"}`,
			explain: "没有策略就没有可强制的效果集",
		},
		{
			name:    "danger-full-access",
			opts:    gate.SandboxedOptions{Runner: &sandboxFakeRunner{enforcement: sandbox.EnforcementFull}, Policy: dangerFull},
			tool:    "bash",
			args:    `{"command":"mkdir -p build"}`,
			explain: "那个模式没有沙箱",
		},
		{
			name:    "文件工具",
			opts:    gate.SandboxedOptions{Runner: &sandboxFakeRunner{enforcement: sandbox.EnforcementFull}, Policy: workspaceWrite},
			tool:    "write_file",
			args:    `{"path":"a.txt","content":"x"}`,
			explain: "文件工具不经 OS runner，谈不上「已隔离」",
		},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			inner := &sandboxCountingGate{}
			var observed int
			opts := testCase.opts
			opts.Observer = func(gate.AskEvent) { observed++ }
			g := gate.AllowSandboxedCommands(opts, inner)
			if decision := sandboxDecide(t, g, testCase.tool, testCase.args); decision != rig.DecisionAllow {
				t.Fatalf("decision = %q, want allow（来自 inner）", decision)
			}
			if inner.calls != 1 {
				t.Fatalf("inner 调用 %d 次, want 1（%s）", inner.calls, testCase.explain)
			}
			if observed != 0 {
				t.Fatalf("没免问就发观察点 = %d 次", observed)
			}
		})
	}
}

// TestAllowSandboxedCommandsOnlyShellTools：默认只对 bash/pwsh 生效，可覆盖。
func TestAllowSandboxedCommandsOnlyShellTools(t *testing.T) {
	dir := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(dir), sandbox.WithMode(sandbox.ModeWorkspaceWrite), sandbox.WithAnyCommandAllowed(true))
	runner := &sandboxFakeRunner{enforcement: sandbox.EnforcementFull}

	inner := &sandboxCountingGate{}
	g := gate.AllowSandboxedCommands(gate.SandboxedOptions{Runner: runner, Policy: policy}, inner)
	// pwsh 也是 shell 类：即使命令解析用 bash 的语义，判定层只按工具名放行。
	if decision := sandboxDecide(t, g, "pwsh", `{"command":"mkdir -p build"}`); decision != rig.DecisionAllow || inner.calls != 0 {
		t.Fatalf("pwsh 应当免问：decision=%q inner=%d", decision, inner.calls)
	}

	// 覆盖工具名之后，bash 不再享受这个待遇。
	inner2 := &sandboxCountingGate{}
	g2 := gate.AllowSandboxedCommands(gate.SandboxedOptions{
		Runner: runner, Policy: policy, ShellTools: []string{"custom_shell"},
	}, inner2)
	if decision := sandboxDecide(t, g2, "bash", `{"command":"mkdir -p build"}`); decision != rig.DecisionAllow || inner2.calls != 1 {
		t.Fatalf("覆盖 ShellTools 后 bash 应当交给 inner：decision=%q inner=%d", decision, inner2.calls)
	}
}

// TestAllowSandboxedCommandsExplain：免问路径没有拒绝理由；未命中时透传 inner 的理由。
func TestAllowSandboxedCommandsExplain(t *testing.T) {
	dir := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(dir), sandbox.WithMode(sandbox.ModeWorkspaceWrite), sandbox.WithAnyCommandAllowed(true))
	g := gate.AllowSandboxedCommands(gate.SandboxedOptions{
		Runner: &sandboxFakeRunner{enforcement: sandbox.EnforcementFull},
		Policy: policy,
	}, gate.DenyAllWithReason("inner says no"))
	explainer, ok := g.(interface {
		ExplainDenial(tool rig.Tool, call rig.ToolCall) string
	})
	if !ok {
		t.Fatal("门禁应当支持 ExplainDenial")
	}
	if reason := explainer.ExplainDenial(&sandboxShellTool{name: "bash"}, rig.ToolCall{Name: "bash", Arguments: `{"command":"mkdir x"}`}); reason != "" {
		t.Fatalf("免问的调用不该有拒绝理由：%q", reason)
	}
	// 未命中（没有 runner 的场景换成 runner 为 nil）。
	g2 := gate.AllowSandboxedCommands(gate.SandboxedOptions{Policy: policy}, gate.DenyAllWithReason("inner says no"))
	explainer2 := g2.(interface {
		ExplainDenial(tool rig.Tool, call rig.ToolCall) string
	})
	if reason := explainer2.ExplainDenial(&sandboxShellTool{name: "bash"}, rig.ToolCall{Name: "bash", Arguments: `{"command":"mkdir x"}`}); !strings.Contains(reason, "inner says no") {
		t.Fatalf("未命中时应透传 inner 的理由：%q", reason)
	}
}
