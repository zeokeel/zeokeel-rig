package kit_test

// 逐命令提权的**审批行为**（kit 侧的适配器）：谁能批准、谁不能、留下什么痕迹。
//
// 这里用真实零配置 kit + fake runner 走完整路径（模型请求 → 门禁 → 工具 → 提权
// 审批 → 执行），因为要证明的正是"装配层没有把这条路走错"。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/events"
	"github.com/zeokeel/zeokeel-rig/gate"
	"github.com/zeokeel/zeokeel-rig/kit"
	"github.com/zeokeel/zeokeel-rig/sandbox"
)

const escalationJustification = "needs HKCU DPAPI credentials to store a login token"

// escalationConfig 构造一个"沙箱 + runner + 提权参数"的最小组合。
func escalationConfig(t *testing.T, asker rig.Asker, approval kit.ApprovalMode, writer *strings.Builder) (kit.Config, *kitRunner, *scriptProvider) {
	t.Helper()
	root := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithAnyCommandAllowed(true))
	runner := &kitRunner{}
	scripts := [][]kit.Chunk{
		toolCallScript(kit.ToolCall{
			ID:   "call_escalate",
			Name: "bash",
			Arguments: `{"command":"echo escalated-ok","sandbox_permissions":"require_escalated","justification":"` +
				escalationJustification + `"}`,
		}),
		textScript("done"),
	}
	provider := &scriptProvider{scripts: scripts}
	cfg := kit.Config{
		Model:         "test-model",
		Provider:      provider,
		Root:          root,
		Policy:        policy,
		SandboxRunner: runner,
		Asker:         asker,
		Approval:      approval,
		Gate:          gate.AllowAll(), // 门禁放行到工具层，提权审批才是边界
	}
	if writer != nil {
		cfg.EventWriter = writer
	}
	return cfg, runner, provider
}

// TestEscalationApprovedByHuman 断言"人在 ask 档批准"之后命令真的执行，且
// 审计事件带上 argv 与理由。
func TestEscalationApprovedByHuman(t *testing.T) {
	asker := &auditAsker{answer: "allow"}
	var writer strings.Builder
	cfg, runner, _ := escalationConfig(t, asker, kit.ApprovalAsk, &writer)
	cfg.Gate = nil // 走 kit 的 PersonalAsk：只读免问、其余问人
	a := mustNew(t, cfg)

	if _, _, err := a.RunText(context.Background(), "s1", "please escalate", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	if asker.asked == 0 {
		t.Fatal("应当弹出审批")
	}
	if !strings.Contains(asker.question, escalationJustification) {
		t.Fatalf("审批文案必须带上理由: %q", asker.question)
	}
	if !strings.Contains(asker.question, "echo escalated-ok") {
		t.Fatalf("审批文案必须带上完整命令: %q", asker.question)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("获批后不该再经过 runner（calls=%d）", len(runner.calls))
	}

	// 审计事件。
	var found map[string]any
	for _, envelope := range wiringDecodeAll(t, []byte(writer.String())) {
		if envelope.Kind != events.KindSandboxEscalation {
			continue
		}
		if err := json.Unmarshal(envelope.Payload, &found); err != nil {
			t.Fatalf("payload: %v", err)
		}
	}
	if found == nil {
		t.Fatalf("没有发出 %s 事件:\n%s", events.KindSandboxEscalation, writer.String())
	}
	if found["decision"] != "allowed" || found["tool"] != "bash" {
		t.Fatalf("payload = %v", found)
	}
	if found["justification"] != escalationJustification {
		t.Fatalf("payload 里的 justification = %v", found["justification"])
	}
	argv, _ := found["argv"].([]any)
	if len(argv) == 0 || !strings.Contains(strings.Join(anyStrings(argv), " "), "echo escalated-ok") {
		t.Fatalf("payload 里的 argv = %v", found["argv"])
	}
}

// TestEscalationDeniedByHumanLeavesEvent 断言人拒绝时命令不跑，但**照样留痕**。
func TestEscalationDeniedByHumanLeavesEvent(t *testing.T) {
	asker := &auditAsker{answer: "deny"}
	var writer strings.Builder
	cfg, runner, _ := escalationConfig(t, asker, kit.ApprovalAsk, &writer)
	cfg.Gate = nil
	a := mustNew(t, cfg)

	if _, _, err := a.RunText(context.Background(), "s1", "please escalate", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("被拒绝时不该 spawn")
	}
	var found map[string]any
	for _, envelope := range wiringDecodeAll(t, []byte(writer.String())) {
		if envelope.Kind == events.KindSandboxEscalation {
			_ = json.Unmarshal(envelope.Payload, &found)
		}
	}
	if found == nil || found["decision"] != "denied" {
		t.Fatalf("拒绝也必须留痕，payload = %v\n%s", found, writer.String())
	}
}

// TestEscalationAutoModeRefuses 断言自动档不能自己给自己提权，且**不会去问人**。
func TestEscalationAutoModeRefuses(t *testing.T) {
	asker := &auditAsker{answer: "allow"}
	cfg, runner, provider := escalationConfig(t, asker, kit.ApprovalAuto, nil)
	cfg.Gate = nil // 走 kit 内置的 PersonalAuto：不问人、全部放行
	a := mustNew(t, cfg)

	text, _, err := a.RunText(context.Background(), "s1", "please escalate", nil)
	if err != nil {
		t.Fatalf("RunText: %v", err)
	}
	_ = text
	if asker.asked != 0 {
		t.Fatalf("auto 档不该弹审批（asked=%d）", asker.asked)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("auto 档的提权必须被拒绝，不该 spawn")
	}
	// 模型看到的错误必须解释"为什么 auto 不行"。
	message, ok := findToolMessage(provider.request(1).Messages, "bash")
	if !ok {
		t.Fatalf("没有工具消息")
	}
	if !strings.Contains(message.Text(), "approval=auto") {
		t.Fatalf("模型应当被告知 auto 不能提权，实际: %q", message.Text())
	}
}

// TestEscalationNeverModeRefuses 断言 never 档一律拒绝。
func TestEscalationNeverModeRefuses(t *testing.T) {
	asker := &auditAsker{answer: "allow"}
	cfg, runner, _ := escalationConfig(t, asker, kit.ApprovalNever, nil)
	cfg.Gate = gate.AllowAll() // 门禁放行，让"never 档"这条判定单独生效
	a := mustNew(t, cfg)
	if _, _, err := a.RunText(context.Background(), "s1", "please escalate", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	if asker.asked != 0 || len(runner.calls) != 0 {
		t.Fatalf("never 档：asked=%d runner=%d", asker.asked, len(runner.calls))
	}
}

// TestEscalationWithoutAskerRefuses 断言没有审批通道时明确拒绝并告诉模型怎么办。
func TestEscalationWithoutAskerRefuses(t *testing.T) {
	cfg, runner, provider := escalationConfig(t, nil, kit.ApprovalAsk, nil)
	cfg.Gate = gate.AllowAll()
	a := mustNew(t, cfg)
	if _, _, err := a.RunText(context.Background(), "s1", "please escalate", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("没有审批通道时不该 spawn")
	}
	message, ok := findToolMessage(provider.request(1).Messages, "bash")
	if !ok {
		t.Fatalf("没有工具消息")
	}
	if !strings.Contains(message.Text(), "approval channel") {
		t.Fatalf("应当说明没有审批通道: %q", message.Text())
	}
}

// TestDenialAdviceFollowsWiring 断言沙箱拒绝的建议与实际装配一致。
func TestDenialAdviceFollowsWiring(t *testing.T) {
	root := t.TempDir()

	// ① 默认：有授权工具 + 有提权通道（Asker 存在且 ask 档）。
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithAnyCommandAllowed(true))
	if _, err := kit.New(kit.Config{
		Model: "m", Provider: &scriptProvider{}, Root: root, Policy: policy, Asker: &auditAsker{answer: "allow"},
	}); err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	got := policy.DenialOptions()
	if !got.HasGrantTool || !got.HasEscalation {
		t.Fatalf("DenialOptions = %+v, want both true", got)
	}
	denial := sandbox.DetectDenial("cannot create /home/me/.dws/.data.lock: Access is denied.",
		[]string{"access is denied"}, policy, sandbox.EnforcementPartial)
	if denial == nil {
		t.Fatal("Denial = nil")
	}
	note := denial.Note()
	if !strings.Contains(note, "request_sandbox_access") {
		t.Fatalf("有授权工具时应给出申请可写根的建议:\n%s", note)
	}
	if !strings.Contains(note, `sandbox_permissions:"require_escalated"`) {
		t.Fatalf("有提权通道时应给出提权建议:\n%s", note)
	}

	// ② 关掉授权工具 + auto 档：两条路都没有，只能让用户去设置里开。
	off := false
	policy2 := sandbox.New(sandbox.WithRoots(root), sandbox.WithAnyCommandAllowed(true))
	if _, err := kit.New(kit.Config{
		Model: "m", Provider: &scriptProvider{}, Root: root, Policy: policy2,
		Asker: &auditAsker{answer: "allow"}, Approval: kit.ApprovalAuto, SandboxGrants: &off,
	}); err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	got2 := policy2.DenialOptions()
	if got2.HasGrantTool || got2.HasEscalation {
		t.Fatalf("DenialOptions = %+v, want both false", got2)
	}
	denial2 := sandbox.DetectDenial("cannot create /home/me/.dws/.data.lock: Access is denied.",
		[]string{"access is denied"}, policy2, sandbox.EnforcementPartial)
	note2 := denial2.Note()
	for _, banned := range []string{"request_sandbox_access", "sandbox_permissions"} {
		if strings.Contains(note2, banned) {
			t.Fatalf("没有通道时不许点名 %s:\n%s", banned, note2)
		}
	}
	if !strings.Contains(note2, "app settings") {
		t.Fatalf("应让用户去设置里开启:\n%s", note2)
	}
}

// anyStrings 把 []any 转成 []string（事件 payload 解码后的 argv）。
func anyStrings(values []any) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if text, ok := value.(string); ok {
			out = append(out, text)
		}
	}
	return out
}
