package gate_test

// DSH 对齐后的审批语义回归（审批只有 ask / never 两档，危险命令不存在"硬拒"）。
//
// 覆盖 Lead 指定的矩阵：四个能执行任意命令的工具名（bash/pwsh/job_start/terminal_send）
// × ask（有 Asker / 无 Asker）× auto。判据只有一条：**谁能批准，谁就有权决定**；
// 命令文本不参与默认预设的判定。

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/gate"
)

// dshShellTools 是能执行任意命令的工具名（参数里带命令文本的四种形态）。
var dshShellTools = []string{"bash", "pwsh", "job_start", "terminal_send"}

// dshDangerousCommand 是"以前会被硬拒"的代表性命令。
const dshDangerousCommand = "rm -rf /"

// dshAsker 记录调用次数并返回固定答案。
type dshAsker struct {
	mu     sync.Mutex
	answer string
	calls  int
}

func (a *dshAsker) Ask(context.Context, string, []string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	return a.answer, nil
}

func (a *dshAsker) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

// dshCall 构造一次"带危险命令的 shell 调用"。
func dshCall(tool string) rig.ToolCall {
	return rig.ToolCall{ID: "dsh-1", Name: tool, Arguments: `{"command":"` + dshDangerousCommand + `"}`}
}

// TestDSHAskAsksHumanForDangerousCommand 是本次语义变更的核心：
// ask 档下危险命令**必须问人**（而不是 DecisionDeny），人批准就执行。
func TestDSHAskAsksHumanForDangerousCommand(t *testing.T) {
	for _, tool := range dshShellTools {
		t.Run(tool, func(t *testing.T) {
			asker := &dshAsker{answer: gate.AskAllow}
			g := gate.PersonalAsk(gate.Ask(asker))
			decision, err := approve(t, g, stubTool{name: tool}, dshCall(tool))
			if err != nil {
				t.Fatalf("不该返回 Go error（会中止 run）: %v", err)
			}
			if decision != rig.DecisionAllow {
				t.Fatalf("decision = %q, want allow（人批准了）", decision)
			}
			if got := asker.count(); got != 1 {
				t.Fatalf("Asker 被调用 %d 次, want 1", got)
			}
		})
	}
}

// TestDSHAskDeniedByHumanDenies 同一矩阵：人拒绝则拒绝。
func TestDSHAskDeniedByHumanDenies(t *testing.T) {
	for _, tool := range dshShellTools {
		t.Run(tool, func(t *testing.T) {
			asker := &dshAsker{answer: gate.AskDeny}
			g := gate.PersonalAsk(gate.Ask(asker))
			decision, err := approve(t, g, stubTool{name: tool}, dshCall(tool))
			if err != nil {
				t.Fatalf("Go error: %v", err)
			}
			if decision != rig.DecisionDeny {
				t.Fatalf("decision = %q, want deny（审批人拒绝）", decision)
			}
			if got := asker.count(); got != 1 {
				t.Fatalf("Asker 被调用 %d 次, want 1", got)
			}
		})
	}
}

// TestDSHAutoIsAllowAllAndNeverAsks：auto 档放行危险命令，且不调用任何审批通道。
func TestDSHAutoIsAllowAllAndNeverAsks(t *testing.T) {
	// auto 不接收 inner，因此结构上不可能问人；仍然接一个记录型 Asker 以钉住"0 次"。
	asker := &dshAsker{answer: gate.AskDeny}
	g := gate.PersonalAuto()
	for _, tool := range dshShellTools {
		t.Run(tool, func(t *testing.T) {
			decision, err := approve(t, g, stubTool{name: tool}, dshCall(tool))
			if err != nil {
				t.Fatalf("Go error: %v", err)
			}
			if decision != rig.DecisionAllow {
				t.Fatalf("decision = %q, want allow（auto = AllowAll）", decision)
			}
		})
	}
	if got := asker.count(); got != 0 {
		t.Fatalf("auto 档 Asker 调用 %d 次, want 0", got)
	}
	if reason := explainDenial(g, stubTool{name: "bash"}, dshCall("bash")); reason != "" {
		t.Fatalf("放行的调用不应有拒绝理由: %q", reason)
	}
}

// TestDSHAskWithoutAskerFailsClosed：没有审批通道时危险命令被拒，并说明"没有通道"。
func TestDSHAskWithoutAskerFailsClosed(t *testing.T) {
	g := gate.PersonalAskWithoutAsker()
	for _, tool := range dshShellTools {
		t.Run(tool, func(t *testing.T) {
			call := dshCall(tool)
			decision, err := approve(t, g, stubTool{name: tool}, call)
			if err != nil {
				t.Fatalf("必须是 deny 而不是 error: %v", err)
			}
			if decision != rig.DecisionDeny {
				t.Fatalf("decision = %q, want deny", decision)
			}
			reason := explainDenial(g, stubTool{name: tool}, call)
			if !strings.Contains(reason, "没有可用的审批通道") {
				t.Fatalf("理由应说明没有可用的审批通道，得到 %q", reason)
			}
		})
	}
}

// TestDSHCommandRiskGateStillAvailable：CommandRiskGate 作为**非默认** API 仍然可用。
func TestDSHCommandRiskGateStillAvailable(t *testing.T) {
	g := gate.CommandRiskGate(gate.AllowAll())
	for _, tool := range []string{"bash", "pwsh"} {
		t.Run(tool, func(t *testing.T) {
			decision, err := approve(t, g, stubTool{name: tool}, dshCall(tool))
			if err != nil {
				t.Fatalf("Go error: %v", err)
			}
			if decision != rig.DecisionDeny {
				t.Fatalf("显式组合 CommandRiskGate 时危险命令仍应被拒，得到 %q", decision)
			}
			reason := explainDenial(g, stubTool{name: tool}, dshCall(tool))
			if !strings.Contains(reason, "危险命令") {
				t.Fatalf("理由应说明是危险命令: %q", reason)
			}
		})
	}
	// 只读命令仍然被这个门禁免问放行。
	readOnly := rig.ToolCall{Name: "bash", Arguments: `{"command":"ls -la"}`}
	if decision, err := approve(t, g, stubTool{name: "bash"}, readOnly); err != nil || decision != rig.DecisionAllow {
		t.Fatalf("只读命令应放行: decision=%q err=%v", decision, err)
	}
}

// TestDSHReadOnlyToolsStillSkipApproval：只读**工具**在 ask 档仍然免问。
func TestDSHReadOnlyToolsStillSkipApproval(t *testing.T) {
	readOnlyTools := []string{
		"read_file", "list_dir", "glob", "grep", "read_image", "search",
		"inspect", "sandbox_status", "working_directory",
		"tool_search", "skill", "memory", "todo_write",
		"session_search", "session_event_read",
		"spill_read", "job_list", "job_output", "terminal_read",
	}
	for _, name := range readOnlyTools {
		t.Run(name, func(t *testing.T) {
			asker := &dshAsker{answer: gate.AskDeny}
			g := gate.PersonalAsk(gate.Ask(asker))
			decision, err := approve(t, g, stubTool{name: name}, rig.ToolCall{Name: name, Arguments: `{}`})
			if err != nil {
				t.Fatalf("Go error: %v", err)
			}
			if decision != rig.DecisionAllow {
				t.Fatalf("只读工具 %s 应免问放行，得到 %q", name, decision)
			}
			if got := asker.count(); got != 0 {
				t.Fatalf("只读工具 %s 被问 %d 次, want 0", name, got)
			}
		})
	}

	// 写工具仍然问人。
	asker := &dshAsker{answer: gate.AskDeny}
	g := gate.PersonalAsk(gate.Ask(asker))
	if decision, err := approve(t, g, stubTool{name: "write_file"}, rig.ToolCall{Name: "write_file", Arguments: `{"path":"a","content":"b"}`}); err != nil || decision != rig.DecisionDeny {
		t.Fatalf("write_file 应被问并拒绝: decision=%q err=%v", decision, err)
	}
	if got := asker.count(); got != 1 {
		t.Fatalf("write_file Asker 调用 %d 次, want 1", got)
	}
}

// TestDSHDecisionMatrix 打印并断言 Lead 需要的决策矩阵（四个工具名 × ask/auto）。
func TestDSHDecisionMatrix(t *testing.T) {
	allowAsker := &dshAsker{answer: gate.AskAllow}
	askGate := gate.PersonalAsk(gate.Ask(allowAsker))
	noAsker := gate.PersonalAskWithoutAsker()
	auto := gate.PersonalAuto()

	for _, tool := range dshShellTools {
		call := dshCall(tool)
		askDecision, err := approve(t, askGate, stubTool{name: tool}, call)
		if err != nil {
			t.Fatalf("%s: ask: %v", tool, err)
		}
		noAskerDecision, err := approve(t, noAsker, stubTool{name: tool}, call)
		if err != nil {
			t.Fatalf("%s: ask-no-Asker: %v", tool, err)
		}
		autoDecision, err := approve(t, auto, stubTool{name: tool}, call)
		if err != nil {
			t.Fatalf("%s: auto: %v", tool, err)
		}
		t.Logf("%-14s ask=%s (asked 1) | ask-no-Asker=%s | auto=%s (asked 0)",
			tool, askDecision, noAskerDecision, autoDecision)

		if askDecision != rig.DecisionAllow {
			t.Fatalf("%s: ask 应放行（人批准）", tool)
		}
		if noAskerDecision != rig.DecisionDeny {
			t.Fatalf("%s: 无通道应拒绝", tool)
		}
		if autoDecision != rig.DecisionAllow {
			t.Fatalf("%s: auto 应放行", tool)
		}
	}
	if got := allowAsker.count(); got != len(dshShellTools) {
		t.Fatalf("ask 档 Asker 调用 %d 次, want %d", got, len(dshShellTools))
	}
}

// TestDSHPersonalDefaultEqualsPersonalAsk 钉住别名关系与"默认不含命令分类"。
func TestDSHPersonalDefaultEqualsPersonalAsk(t *testing.T) {
	call := dshCall("bash")
	asker := &dshAsker{answer: gate.AskAllow}
	askDecision, err := approve(t, gate.PersonalAsk(gate.Ask(asker)), stubTool{name: "bash"}, call)
	if err != nil {
		t.Fatal(err)
	}
	defaultDecision, err := approve(t, gate.PersonalDefault(gate.Ask(asker)), stubTool{name: "bash"}, call)
	if err != nil {
		t.Fatal(err)
	}
	if askDecision != defaultDecision || askDecision != rig.DecisionAllow {
		t.Fatalf("PersonalAsk=%q PersonalDefault=%q, want 都是 allow", askDecision, defaultDecision)
	}
	// 两个构造函数都不该返回一个"命令级门禁"：只读命令同样要问人。
	if got := asker.count(); got != 2 {
		t.Fatalf("Asker 调用 %d 次, want 2（每次都需要审批）", got)
	}
}
