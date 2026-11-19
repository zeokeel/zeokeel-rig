package gate_test

// 拒绝理由通道 + PersonalDefault 的 TDD 用例。
//
// 设计目标：个人客户端最怕"模型被拒后反复重试同一条命令"，所以门禁要把
// "命中哪条规则 / 谁拒绝的 / 该怎么办"通过可选接口 ExplainDenial 交给 agent，
// 由 agent 拼进模型可见的工具错误。

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/gate"
)

// denialCountingGate 统计 Approve 次数并返回固定决定（不实现理由通道）。
type denialCountingGate struct {
	mu       sync.Mutex
	calls    int
	decision rig.Decision
}

func (g *denialCountingGate) Approve(context.Context, rig.Tool, rig.ToolCall) (rig.Decision, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	return g.decision, nil
}

func (g *denialCountingGate) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

// denialExplainingGate 是实现了理由通道的内层门禁。
type denialExplainingGate struct {
	mu       sync.Mutex
	calls    int
	decision rig.Decision
	reason   string
}

func (g *denialExplainingGate) Approve(context.Context, rig.Tool, rig.ToolCall) (rig.Decision, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	return g.decision, nil
}

func (g *denialExplainingGate) ExplainDenial(rig.Tool, rig.ToolCall) string { return g.reason }

func (g *denialExplainingGate) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

// bashInvocation 构造一次 bash 调用。
func bashInvocation(command string) (rig.Tool, rig.ToolCall) {
	return stubTool{name: "bash"}, rig.ToolCall{
		ID: "call_1", Name: "bash", Arguments: `{"command":` + quoteJSON(command) + `}`,
	}
}

// explainDenial 调用门禁的可选理由接口；不实现时返回空串。
func explainDenial(g rig.Gate, tool rig.Tool, call rig.ToolCall) string {
	explainer, ok := g.(interface {
		ExplainDenial(rig.Tool, rig.ToolCall) string
	})
	if !ok {
		return ""
	}
	return explainer.ExplainDenial(tool, call)
}

// TestPersonalDefaultBranches 断言 PersonalDefault 的四种决策分支与 inner 调用次数。
func TestPersonalDefaultBranches(t *testing.T) {
	tests := []struct {
		name      string
		tool      rig.Tool
		call      rig.ToolCall
		inner     rig.Decision
		want      rig.Decision
		wantCalls int
	}{
		{
			name: "只读工具免审批",
			tool: stubTool{name: "read_file"}, call: rig.ToolCall{Name: "read_file", Arguments: `{"path":"a.md"}`},
			inner: rig.DecisionDeny, want: rig.DecisionAllow, wantCalls: 0,
		},
		{
			// DSH 对齐：免问只看工具名，命令文本不再参与判定——只读命令也要问人。
			name: "只读命令也要审批",
			tool: stubTool{name: "bash"}, call: rig.ToolCall{Name: "bash", Arguments: `{"command":"ls -la"}`},
			inner: rig.DecisionDeny, want: rig.DecisionDeny, wantCalls: 1,
		},
		{
			// DSH 对齐：危险命令同样交给 inner（人批准就执行），默认预设不再硬拒。
			name: "危险命令交给 inner（放行）",
			tool: stubTool{name: "bash"}, call: rig.ToolCall{Name: "bash", Arguments: `{"command":"rm -rf /"}`},
			inner: rig.DecisionAllow, want: rig.DecisionAllow, wantCalls: 1,
		},
		{
			name: "可变命令交给 inner（放行）",
			tool: stubTool{name: "bash"}, call: rig.ToolCall{Name: "bash", Arguments: `{"command":"rm -rf ./build"}`},
			inner: rig.DecisionAllow, want: rig.DecisionAllow, wantCalls: 1,
		},
		{
			name: "可变命令交给 inner（拒绝）",
			tool: stubTool{name: "bash"}, call: rig.ToolCall{Name: "bash", Arguments: `{"command":"rm -rf ./build"}`},
			inner: rig.DecisionDeny, want: rig.DecisionDeny, wantCalls: 1,
		},
		{
			name: "非命令工具交给 inner",
			tool: stubTool{name: "write_file"}, call: rig.ToolCall{Name: "write_file", Arguments: `{"path":"a.md","content":"x"}`},
			inner: rig.DecisionDeny, want: rig.DecisionDeny, wantCalls: 1,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			inner := &denialCountingGate{decision: testCase.inner}
			g := gate.PersonalDefault(inner)
			decision, err := approve(t, g, testCase.tool, testCase.call)
			if err != nil {
				t.Fatalf("Approve: %v", err)
			}
			if decision != testCase.want {
				t.Fatalf("decision = %q, want %q", decision, testCase.want)
			}
			if got := inner.count(); got != testCase.wantCalls {
				t.Fatalf("inner 调用 %d 次, want %d", got, testCase.wantCalls)
			}
		})
	}
}

// TestPersonalDefaultNilInnerFailsClosed 断言 inner 为 nil 时仍然"只读放行、危险拒绝、
// 其余报配置错误"（fail closed）。
func TestPersonalDefaultNilInnerFailsClosed(t *testing.T) {
	g := gate.PersonalDefault(nil)

	decision, err := approve(t, g, stubTool{name: "read_file"}, rig.ToolCall{Name: "read_file"})
	if err != nil || decision != rig.DecisionAllow {
		t.Fatalf("只读工具: decision=%q err=%v", decision, err)
	}

	// 只读**命令**不是免问对象（免问只看工具名），没有 inner 时与其它调用一样 deny。
	tool, call := bashInvocation("ls -la")
	if decision, err := approve(t, g, tool, call); err != nil || decision != rig.DecisionDeny {
		t.Fatalf("只读命令: decision=%q err=%v, want deny（没有审批通道）", decision, err)
	}

	dangerTool, dangerCall := bashInvocation("rm -rf /")
	if decision, err := approve(t, g, dangerTool, dangerCall); err != nil || decision != rig.DecisionDeny {
		t.Fatalf("危险命令: decision=%q err=%v", decision, err)
	}

	// 可变命令 + nil inner：deny + 理由（**不是** error——error 会中止整个 run，
	// 见包文档"失败模式"与 TestPersonalDefaultNilDeniesAndKeepsRunAlive）。
	mutatingTool, mutatingCall := bashInvocation("rm -rf ./build")
	if decision, err := approve(t, g, mutatingTool, mutatingCall); err != nil || decision != rig.DecisionDeny {
		t.Fatalf("可变命令 + nil inner: decision=%q err=%v, want deny（不是 error）", decision, err)
	}
}

// TestCommandRiskGateExplainDenialGivesRuleID 断言危险命令的理由里含规则 id 与中文
// 说明与可操作建议。
func TestCommandRiskGateExplainDenialGivesRuleID(t *testing.T) {
	// DSH 对齐后 PersonalDefault 不再自带命令级判定，这里显式组合以测试 CommandRiskGate 本身。
	g := gate.CommandRiskGate(gate.PersonalDefault(gate.DenyAll()))
	tool, call := bashInvocation("rm -rf /")
	if decision, err := approve(t, g, tool, call); err != nil || decision != rig.DecisionDeny {
		t.Fatalf("危险命令应当拒绝: decision=%q err=%v", decision, err)
	}
	reason := explainDenial(g, tool, call)
	if !strings.Contains(reason, "rm.recursive-root") {
		t.Fatalf("理由里没有规则 id: %q", reason)
	}
	if !strings.Contains(reason, "危险命令") {
		t.Fatalf("理由里没有中文说明: %q", reason)
	}
	if !strings.Contains(reason, "；") && !strings.Contains(reason, "，") {
		t.Fatalf("理由里应当有建议部分: %q", reason)
	}
	t.Logf("真实理由（rm -rf /）: %s", reason)
}

// TestCommandRiskGateExplainDenialOnlyForDangerous 断言非拒绝场景不给理由（避免把
// 只读命令的原因塞进别处的拒绝文案）。这里用 AllowAll 作 inner，隔离出本门禁自己的
// 贡献：只有危险命令才有理由。
func TestCommandRiskGateExplainDenialOnlyForDangerous(t *testing.T) {
	g := gate.CommandRiskGate(gate.PersonalDefault(gate.AllowAll()))
	tool, call := bashInvocation("ls -la")
	if reason := explainDenial(g, tool, call); reason != "" {
		t.Fatalf("只读命令不应产生拒绝理由: %q", reason)
	}
	fileTool := stubTool{name: "write_file"}
	if reason := explainDenial(g, fileTool, rig.ToolCall{Name: "write_file"}); reason != "" {
		t.Fatalf("非命令工具不应产生理由: %q", reason)
	}
	// 可变但非危险的命令由 inner 放行：同样没有理由。
	mutatingTool, mutatingCall := bashInvocation("rm -rf ./build")
	if decision, err := approve(t, g, mutatingTool, mutatingCall); err != nil || decision != rig.DecisionAllow {
		t.Fatalf("可变命令应放行: decision=%q err=%v", decision, err)
	}
	if reason := explainDenial(g, mutatingTool, mutatingCall); reason != "" {
		t.Fatalf("放行的命令不应产生理由: %q", reason)
	}
}

// TestCommandRiskGateExplainDelegatesToInner 断言内层门禁的理由能穿透组合门禁。
func TestCommandRiskGateExplainDelegatesToInner(t *testing.T) {
	inner := &denialExplainingGate{decision: rig.DecisionDeny, reason: "用户暂时拒绝：先看改动计划"}
	g := gate.PersonalDefault(inner)
	tool, call := bashInvocation("rm -rf ./build")
	if decision, err := approve(t, g, tool, call); err != nil || decision != rig.DecisionDeny {
		t.Fatalf("可变命令应交给 inner 拒绝: decision=%q err=%v", decision, err)
	}
	if reason := explainDenial(g, tool, call); !strings.Contains(reason, "用户暂时拒绝") {
		t.Fatalf("inner 的理由没有穿透组合层: %q", reason)
	}
	if got := inner.count(); got != 1 {
		t.Fatalf("inner 应当只被调用一次（ExplainDenial 不得重复触发审批），实际 %d 次", got)
	}
}

// TestReviewGateExplainDenialUsesReviewReason 断言自动审查拒绝时，审查者给的
// Reason 会出现在理由里。
func TestReviewGateExplainDenialUsesReviewReason(t *testing.T) {
	reviewer := gate.ToolReviewerFunc(func(context.Context, gate.ToolReviewRequest) (gate.ToolReviewDecision, error) {
		return gate.ToolReviewDecision{Allow: false, Reason: "改动会删除迁移文件，先确认备份"}, nil
	})
	g := gate.Review(reviewer)
	tool := stubTool{name: "write_file"}
	call := rig.ToolCall{Name: "write_file", Arguments: `{"path":"migrations/001.sql","content":"x"}`}
	if decision, err := approve(t, g, tool, call); err != nil || decision != rig.DecisionDeny {
		t.Fatalf("审查拒绝: decision=%q err=%v", decision, err)
	}
	reason := explainDenial(g, tool, call)
	if !strings.Contains(reason, "先确认备份") {
		t.Fatalf("审查者的理由没有透出: %q", reason)
	}
}

// TestAskGateExplainDenialMentionsTool 断言人工拒绝也有理由（含工具名与"别重试"）。
func TestAskGateExplainDenialMentionsTool(t *testing.T) {
	asker := &fakeAsker{fn: func(context.Context, string, []string) (string, error) {
		return gate.AskDeny, nil
	}}
	g := gate.Ask(asker)
	tool, call := bashInvocation("rm -rf ./build")
	if decision, err := approve(t, g, tool, call); err != nil || decision != rig.DecisionDeny {
		t.Fatalf("人工拒绝: decision=%q err=%v", decision, err)
	}
	reason := explainDenial(g, tool, call)
	if reason == "" {
		t.Fatal("人工拒绝应当给出理由")
	}
	if !strings.Contains(reason, "bash") {
		t.Fatalf("理由里应当提到被拒的工具: %q", reason)
	}
	t.Logf("真实理由（人工拒绝）: %s", reason)
}

// TestAskGateExplainDenialEmptyOnAllow 断言放行时不给理由（理由只属于拒绝）。
func TestAskGateExplainDenialEmptyOnAllow(t *testing.T) {
	asker := &fakeAsker{fn: func(context.Context, string, []string) (string, error) {
		return gate.AskAllow, nil
	}}
	g := gate.Ask(asker)
	tool, call := bashInvocation("rm -rf ./build")
	if decision, err := approve(t, g, tool, call); err != nil || decision != rig.DecisionAllow {
		t.Fatalf("人工放行: decision=%q err=%v", decision, err)
	}
	if reason := explainDenial(g, tool, call); reason != "" {
		t.Fatalf("放行不应产生拒绝理由: %q", reason)
	}
}

// TestReviewGateDenialReasonsArePerCall 断言并发下理由不会串台（-race 也要过）。
func TestReviewGateDenialReasonsArePerCall(t *testing.T) {
	reviewer := gate.ToolReviewerFunc(func(_ context.Context, request gate.ToolReviewRequest) (gate.ToolReviewDecision, error) {
		return gate.ToolReviewDecision{Allow: false, Reason: "拒绝 " + request.Tool}, nil
	})
	g := gate.Review(reviewer)

	const workers = 8
	var wait sync.WaitGroup
	for index := 0; index < workers; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			name := "tool_a"
			if index%2 == 1 {
				name = "tool_b"
			}
			tool := stubTool{name: name}
			call := rig.ToolCall{Name: name, Arguments: `{"x":1}`}
			if decision, err := g.Approve(context.Background(), tool, call); err != nil || decision != rig.DecisionDeny {
				t.Errorf("Approve: decision=%q err=%v", decision, err)
				return
			}
			reason := explainDenial(g, tool, call)
			if !strings.Contains(reason, name) {
				t.Errorf("并发下理由串台: 期望提到 %q，实际 %q", name, reason)
			}
		}(index)
	}
	wait.Wait()
}

// TestExplainDenialDoesNotChangeDecisions 断言解释器只读不影响 Approve 的结果与次数。
func TestExplainDenialDoesNotChangeDecisions(t *testing.T) {
	inner := &denialCountingGate{decision: rig.DecisionAllow}
	g := gate.PersonalDefault(inner)
	tool, call := bashInvocation("rm -rf ./build")
	for i := 0; i < 3; i++ {
		_ = explainDenial(g, tool, call)
	}
	decision, err := approve(t, g, tool, call)
	if err != nil || decision != rig.DecisionAllow {
		t.Fatalf("decision=%q err=%v", decision, err)
	}
	if inner.count() != 1 {
		t.Fatalf("inner 调用 %d 次, want 1（ExplainDenial 不应触发审批）", inner.count())
	}
}

// TestPersonalDefaultInnerErrorPropagates 断言 inner 的错误原样穿透。
func TestPersonalDefaultInnerErrorPropagates(t *testing.T) {
	boom := errors.New("asker unavailable")
	g := gate.PersonalDefault(gate.Func(func(context.Context, rig.Tool, rig.ToolCall) (rig.Decision, error) {
		return "", boom
	}))
	tool, call := bashInvocation("rm -rf ./build")
	if _, err := approve(t, g, tool, call); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
}

// ---------------------------------------------------------------------------
// 静态门禁的理由文案（AllowTools / DenyAll）与三档审批映射
// ---------------------------------------------------------------------------

// TestAllowToolsExplainDenial 断言白名单拒绝给出可行动的中文理由。
func TestAllowToolsExplainDenial(t *testing.T) {
	g := gate.AllowTools("read_file", "glob")
	tool, call := bashInvocation("rm -rf ./build")
	if decision, err := approve(t, g, tool, call); err != nil || decision != rig.DecisionDeny {
		t.Fatalf("bash 不在白名单里，应当拒绝: decision=%q err=%v", decision, err)
	}
	const want = "工具 bash 不在当前允许的白名单里；改用只读方式（read_file/glob/grep），或请用户在设置里放开这个工具"
	if reason := explainDenial(g, tool, call); reason != want {
		t.Fatalf("理由文案 = %q, want %q", reason, want)
	}
	// 放行的调用不应产生理由（理由只属于拒绝）。
	readTool := stubTool{name: "read_file"}
	if reason := explainDenial(g, readTool, rig.ToolCall{Name: "read_file"}); reason != "" {
		t.Fatalf("放行的工具不应有拒绝理由: %q", reason)
	}
}

// TestDenyAllExplainDenial 断言"拒绝所有"模式也解释原因（否则模型只会空转重试）。
func TestDenyAllExplainDenial(t *testing.T) {
	g := gate.DenyAll()
	tool, call := bashInvocation("ls -la")
	if decision, err := approve(t, g, tool, call); err != nil || decision != rig.DecisionDeny {
		t.Fatalf("DenyAll: decision=%q err=%v", decision, err)
	}
	const want = `当前处于"拒绝所有工具调用"模式（approval=never）；请让用户切换审批模式`
	if reason := explainDenial(g, tool, call); reason != want {
		t.Fatalf("理由文案 = %q, want %q", reason, want)
	}
}

// TestPersonalAskIsPersonalDefault 断言 PersonalAsk 与 PersonalDefault 语义一致
// （前者是可读的别名：审批三档里的 ask 档）。
func TestPersonalAskIsPersonalDefault(t *testing.T) {
	cases := []struct {
		name  string
		tool  rig.Tool
		call  rig.ToolCall
		inner rig.Decision
		want  rig.Decision
		calls int
	}{
		{"只读工具", stubTool{name: "read_file"}, rig.ToolCall{Name: "read_file"}, rig.DecisionDeny, rig.DecisionAllow, 0},
		{"只读命令也要问", stubTool{name: "bash"}, rig.ToolCall{Name: "bash", Arguments: `{"command":"ls"}`}, rig.DecisionDeny, rig.DecisionDeny, 1},
		{"危险命令交给审批", stubTool{name: "bash"}, rig.ToolCall{Name: "bash", Arguments: `{"command":"rm -rf /"}`}, rig.DecisionAllow, rig.DecisionAllow, 1},
		{"可变命令", stubTool{name: "bash"}, rig.ToolCall{Name: "bash", Arguments: `{"command":"rm -rf ./build"}`}, rig.DecisionDeny, rig.DecisionDeny, 1},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			askInner := &denialCountingGate{decision: testCase.inner}
			defaultInner := &denialCountingGate{decision: testCase.inner}
			askDecision, askErr := approve(t, gate.PersonalAsk(askInner), testCase.tool, testCase.call)
			defaultDecision, defaultErr := approve(t, gate.PersonalDefault(defaultInner), testCase.tool, testCase.call)
			if askErr != nil || defaultErr != nil {
				t.Fatalf("Approve: ask=%v default=%v", askErr, defaultErr)
			}
			if askDecision != testCase.want || defaultDecision != testCase.want {
				t.Fatalf("decision: ask=%q default=%q, want %q", askDecision, defaultDecision, testCase.want)
			}
			if askInner.count() != testCase.calls || defaultInner.count() != testCase.calls {
				t.Fatalf("inner 调用: ask=%d default=%d, want %d", askInner.count(), defaultInner.count(), testCase.calls)
			}
		})
	}
}

// TestPersonalAutoIsAllowAll 是 DSH 对齐后的回归守卫：auto 不问人，也不做命令分类。
func TestPersonalAutoIsAllowAll(t *testing.T) {
	g := gate.PersonalAuto()

	// ① 只读命令放行。
	tool, call := bashInvocation("ls -la")
	if decision, err := approve(t, g, tool, call); err != nil || decision != rig.DecisionAllow {
		t.Fatalf("ls 应当放行: decision=%q err=%v", decision, err)
	}

	// ② 危险命令同样放行（auto = AllowAll），且不产生拒绝理由。
	dangerTool, dangerCall := bashInvocation("rm -rf /")
	if decision, err := approve(t, g, dangerTool, dangerCall); err != nil || decision != rig.DecisionAllow {
		t.Fatalf("rm -rf / 在 auto 下应放行（DSH 对齐）: decision=%q err=%v", decision, err)
	}
	if reason := explainDenial(g, dangerTool, dangerCall); reason != "" {
		t.Fatalf("放行的调用不应有拒绝理由: %q", reason)
	}

	// ③ 写工具同样放行。
	writeTool := stubTool{name: "write_file"}
	writeCall := rig.ToolCall{Name: "write_file", Arguments: `{"path":"a.md","content":"x"}`}
	if decision, err := approve(t, g, writeTool, writeCall); err != nil || decision != rig.DecisionAllow {
		t.Fatalf("write_file 在 auto 下应当放行: decision=%q err=%v", decision, err)
	}
}

// TestPersonalNeverDeniesEverythingWithReason 断言 never 档拒绝一切并解释原因。
func TestPersonalNeverDeniesEverythingWithReason(t *testing.T) {
	g := gate.PersonalNever()
	tool, call := bashInvocation("ls -la")
	if decision, err := approve(t, g, tool, call); err != nil || decision != rig.DecisionDeny {
		t.Fatalf("never 档应当拒绝: decision=%q err=%v", decision, err)
	}
	if reason := explainDenial(g, tool, call); !strings.Contains(reason, "approval=never") {
		t.Fatalf("理由应说明审批模式: %q", reason)
	}
}

// ---------------------------------------------------------------------------
// DenyAllWithReason / PersonalAskWithoutAsker
// ---------------------------------------------------------------------------

// TestDenyAllWithReasonUsesGivenText 断言自定义理由逐字生效，空理由回落到内置文案。
func TestDenyAllWithReasonUsesGivenText(t *testing.T) {
	const custom = "当前审批模式只放行只读操作；请宿主接上审批通道"
	tool, call := bashInvocation("ls -la")

	g := gate.DenyAllWithReason(custom)
	if decision, err := approve(t, g, tool, call); err != nil || decision != rig.DecisionDeny {
		t.Fatalf("DenyAllWithReason 应当拒绝: decision=%q err=%v", decision, err)
	}
	if reason := explainDenial(g, tool, call); reason != custom {
		t.Fatalf("理由 = %q, want %q", reason, custom)
	}

	// 空理由（含全空白）必须回落到内置文案，而不是空串——空串会让模型退回历史文案，
	// 等于这次改造白做。
	const builtin = "当前没有可用的审批通道（宿主未提供 Asker）；请接上 Ask，或把审批模式改为 auto"
	for _, reason := range []string{"", "   ", "\n\t"} {
		g := gate.DenyAllWithReason(reason)
		if got := explainDenial(g, tool, call); got != builtin {
			t.Fatalf("空理由应回落到内置文案：reason=%q got=%q want=%q", reason, got, builtin)
		}
	}
}

// TestDenyAllWithReasonDeniesLikeDenyAll 断言它与 DenyAll 的决策完全一致（只改文案）。
func TestDenyAllWithReasonDeniesLikeDenyAll(t *testing.T) {
	withReason := gate.DenyAllWithReason("自定义理由")
	plain := gate.DenyAll()

	calls := []struct {
		tool rig.Tool
		call rig.ToolCall
	}{
		{stubTool{name: "read_file"}, rig.ToolCall{Name: "read_file", Arguments: `{"path":"a.md"}`}},
		{stubTool{name: "bash"}, rig.ToolCall{Name: "bash", Arguments: `{"command":"ls"}`}},
		{stubTool{name: "bash"}, rig.ToolCall{Name: "bash", Arguments: `{"command":"rm -rf /"}`}},
	}
	for _, item := range calls {
		first, firstErr := approve(t, withReason, item.tool, item.call)
		second, secondErr := approve(t, plain, item.tool, item.call)
		if firstErr != nil || secondErr != nil {
			t.Fatalf("Approve: %v / %v", firstErr, secondErr)
		}
		if first != second || first != rig.DecisionDeny {
			t.Fatalf("决策不一致: withReason=%q denyAll=%q", first, second)
		}
	}
}

// TestPersonalAskWithoutAskerBranches 断言"ask 模式但没有审批通道"的四分支：
// 只读免问、危险命令给规则 id、其余拒绝并说明"没有通道"、且绝不出现 approval=never。
func TestPersonalAskWithoutAskerBranches(t *testing.T) {
	g := gate.PersonalAskWithoutAsker()
	const builtin = "当前没有可用的审批通道（宿主未提供 Asker）；请接上 Ask，或把审批模式改为 auto"

	// ① 只读工具免问。
	readTool := stubTool{name: "read_file"}
	if decision, err := approve(t, g, readTool, rig.ToolCall{Name: "read_file"}); err != nil || decision != rig.DecisionAllow {
		t.Fatalf("只读工具应放行: decision=%q err=%v", decision, err)
	}
	if reason := explainDenial(g, readTool, rig.ToolCall{Name: "read_file"}); reason != "" {
		t.Fatalf("放行的调用不应有理由: %q", reason)
	}

	// ② 只读命令不再是免问对象：没有通道 → 拒绝 + 内置文案。
	readTool2, readCall := bashInvocation("ls -la")
	if decision, err := approve(t, g, readTool2, readCall); err != nil || decision != rig.DecisionDeny {
		t.Fatalf("只读命令应拒绝（没有审批通道）: decision=%q err=%v", decision, err)
	}
	if reason := explainDenial(g, readTool2, readCall); reason != builtin {
		t.Fatalf("只读命令理由 = %q, want %q", reason, builtin)
	}

	// ③ 可变命令：拒绝 + "没有审批通道"的内置文案（不是 approval=never）。
	mutTool, mutCall := bashInvocation("rm -rf ./build")
	if decision, err := approve(t, g, mutTool, mutCall); err != nil || decision != rig.DecisionDeny {
		t.Fatalf("可变命令应拒绝: decision=%q err=%v", decision, err)
	}
	if reason := explainDenial(g, mutTool, mutCall); reason != builtin {
		t.Fatalf("可变命令理由 = %q, want %q", reason, builtin)
	}

	// ④ 危险命令：与普通调用同一条理由——没有通道，就没有"谁批准了它"。
	dangerTool, dangerCall := bashInvocation("rm -rf /")
	if decision, err := approve(t, g, dangerTool, dangerCall); err != nil || decision != rig.DecisionDeny {
		t.Fatalf("危险命令应拒绝: decision=%q err=%v", decision, err)
	}
	if reason := explainDenial(g, dangerTool, dangerCall); reason != builtin {
		t.Fatalf("危险命令理由 = %q, want %q（DSH 对齐后不再给规则 id）", reason, builtin)
	}

	// ⑤ 其它工具（写文件）：拒绝 + 内置文案。
	writeTool := stubTool{name: "write_file"}
	writeCall := rig.ToolCall{Name: "write_file", Arguments: `{"path":"a.md","content":"x"}`}
	if decision, err := approve(t, g, writeTool, writeCall); err != nil || decision != rig.DecisionDeny {
		t.Fatalf("写工具应拒绝: decision=%q err=%v", decision, err)
	}
	reason := explainDenial(g, writeTool, writeCall)
	if reason != builtin {
		t.Fatalf("写工具理由 = %q, want %q", reason, builtin)
	}
	if strings.Contains(reason, "approval=never") {
		t.Fatalf("理由不应声称 approval=never（真实状态是 ask 但没有通道）: %q", reason)
	}
}

// TestPersonalAskWithDenyAllMisleads 记录"为什么需要 PersonalAskWithoutAsker"：
// 用 DenyAll 兜底时文案会说 approval=never，与 ask 模式的真实状态不符。
func TestPersonalAskWithDenyAllMisleads(t *testing.T) {
	g := gate.PersonalAsk(gate.DenyAll())
	writeTool := stubTool{name: "write_file"}
	writeCall := rig.ToolCall{Name: "write_file", Arguments: `{"path":"a.md","content":"x"}`}
	if decision, err := approve(t, g, writeTool, writeCall); err != nil || decision != rig.DecisionDeny {
		t.Fatalf("写工具应拒绝: decision=%q err=%v", decision, err)
	}
	if reason := explainDenial(g, writeTool, writeCall); !strings.Contains(reason, "approval=never") {
		t.Fatalf("DenyAll 兜底的文案应当是 approval=never（这就是要换掉它的原因）: %q", reason)
	}
}
