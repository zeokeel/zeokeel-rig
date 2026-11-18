package kit

// 逐命令提权的**审批实现**。
//
// 工具侧（tools/builtin）只声明"我要人批准这一条命令"；"问谁、能不能问、怎么
// 记账"全部在这里。三条硬规则：
//
//  1. **自动档自己不能给自己提权**：approval=auto 是"不问人"的档位，而提权的
//     本质是"让人决定是否跳出沙箱"——所以 auto 下一律拒绝（并告诉模型为什么）。
//  2. **没有审批通道就拒绝**：没有 Asker 时不存在"有人会看到这个请求"，返回
//     明确的"让用户在应用里切到 ask 或自己手动执行"。
//  3. **每次都留痕**：允许与拒绝都发 `sandbox.escalation` 事件并写 logger，
//     因为这是唯一能让进程跑出沙箱的入口。

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/events"
	"github.com/zeokeel/zeokeel-rig/tools/builtin"
)

// escalationPayload 是 `sandbox.escalation` 事件的 payload（审计用）。
type escalationPayload struct {
	Tool          string   `json:"tool"`
	Argv          []string `json:"argv"`
	Decision      string   `json:"decision"` // allowed / denied
	Reason        string   `json:"reason,omitempty"`
	Justification string   `json:"justification,omitempty"`
	Mode          string   `json:"mode,omitempty"`
	Risk          string   `json:"risk,omitempty"`
	RuleID        string   `json:"rule_id,omitempty"`
}

// escalationApprover 把 rig.Asker + 审批档位实现成 builtin.Escalator。
type escalationApprover struct {
	asker      rig.Asker
	mode       ApprovalMode
	customGate bool
	bridge     *eventBridge
}

// newEscalationApprover 构造审批通道。cfg.Gate 非 nil（宿主完全接管门禁）时
// 仍要求 Asker：提权永远是"人批准"，没有 Asker 就没有通道。
func newEscalationApprover(asker rig.Asker, mode ApprovalMode, customGate bool, bridge *eventBridge) *escalationApprover {
	return &escalationApprover{asker: asker, mode: mode, customGate: customGate, bridge: bridge}
}

// Available 报告"这个组合里提权是否真的能走通"。它决定沙箱拒绝建议里能不能
// 让模型去试 sandbox_permissions——指向一个必然被拒的参数同样是死路。
func (a *escalationApprover) Available() bool {
	if a == nil || a.asker == nil {
		return false
	}
	return a.mode == ApprovalAsk
}

// Escalate 实现 builtin.Escalator。
func (a *escalationApprover) Escalate(ctx context.Context, request builtin.EscalationRequest) (bool, error) {
	if a == nil {
		return false, fmt.Errorf("no escalation channel is configured")
	}
	// 1) 自动/never 档：不给模型"绕开人工"的路。
	//
	// 即使宿主接了自己的 Gate 也照样按 Approval 判：显式写了 auto/never 就是
	// "不要人工判断"，那么提权（本质是一次人工判断）必然不成立。
	{
		switch a.mode {
		case ApprovalAuto:
			a.record(request, "denied", "approval=auto never approves sandbox escalation", nil)
			return false, fmt.Errorf(
				"approval=auto cannot approve running a command outside the sandbox: that needs a human decision. " +
					"Ask the user to switch the app to approval=ask and retry, or run this command themselves")
		case ApprovalNever:
			a.record(request, "denied", "approval=never rejects every tool call", nil)
			return false, fmt.Errorf(
				"approval=never rejects every tool call, including sandbox escalation")
		}
	}
	// 2) 没有审批通道。
	if a.asker == nil {
		a.record(request, "denied", "no approval channel", nil)
		return false, fmt.Errorf(
			"there is no approval channel in this app, so nobody can approve running this command outside the sandbox. " +
				"Ask the user to enable approvals (or switch to approval=ask) and retry, or run this command themselves")
	}

	// 3) 问人。文案必须让人能判断：完整 argv + 模型给的理由 + 当前的边界。
	question := escalationQuestion(request)
	answer, err := a.asker.Ask(ctx, question, []string{gateAskAllow, gateAskDeny})
	if err != nil {
		a.record(request, "denied", "approval prompt failed: "+err.Error(), nil)
		return false, fmt.Errorf("the approval prompt failed: %w", err)
	}
	if isAllowAnswer(answer) {
		a.record(request, "allowed", "approved by the user", nil)
		return true, nil
	}
	reason := strings.TrimSpace(answer)
	if reason == "" {
		reason = "the user declined"
	}
	a.record(request, "denied", reason, nil)
	return false, nil
}

// gateAskAllow/gateAskDeny 复刻 gate 包的规范选项字符串（kit 不 import gate 的
// 内部常量，避免把门禁实现细节绑死；这两串是宿主 UI 的通用约定）。
const (
	gateAskAllow = "allow"
	gateAskDeny  = "deny"
)

// isAllowAnswer 判断宿主的回答是否算"批准"。只有明确同意才算：默认拒绝。
func isAllowAnswer(answer string) bool {
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "allow", "allow-always", "allow_once", "yes", "y", "true", "approve", "approved":
		return true
	default:
		return false
	}
}

// escalationQuestion 渲染给人看的审批文案。
func escalationQuestion(request builtin.EscalationRequest) string {
	var builder strings.Builder
	builder.WriteString("The agent asks to run ONE command OUTSIDE the OS sandbox (sandbox escalation).\n\n")
	builder.WriteString("Tool: ")
	builder.WriteString(request.Tool)
	builder.WriteString("\nCommand: ")
	builder.WriteString(request.Command)
	builder.WriteString("\nArgv: ")
	builder.WriteString(request.ArgvLine())
	if request.Workdir != "" {
		builder.WriteString("\nWorking directory: ")
		builder.WriteString(request.Workdir)
	}
	builder.WriteString("\n\nWhy (agent's justification): ")
	builder.WriteString(request.Justification)
	builder.WriteString("\n\n")
	builder.WriteString("What still applies if you allow: sandbox policy (writable roots, protected paths), ")
	builder.WriteString("command allow/deny rules and dangerous-command checks. Only the OS-level confinement is skipped.")
	if request.Risk != "" && request.Risk != "unknown" {
		builder.WriteString("\nDetected risk: ")
		builder.WriteString(request.Risk)
		if request.RuleID != "" {
			builder.WriteString(" (")
			builder.WriteString(request.RuleID)
			builder.WriteString(")")
		}
	}
	builder.WriteString("\n\nAllow this command to run outside the sandbox?")
	return builder.String()
}

// record 发审计事件并写 logger（允许与拒绝都记）。
func (a *escalationApprover) record(request builtin.EscalationRequest, decision, reason string, _ any) {
	payload := escalationPayload{
		Tool:          request.Tool,
		Argv:          request.Argv,
		Decision:      decision,
		Reason:        summarize(reason, 200),
		Justification: request.Justification,
		Mode:          string(request.Mode),
		Risk:          request.Risk,
		RuleID:        request.RuleID,
	}
	if a.bridge != nil {
		a.bridge.emit(events.KindSandboxEscalation, payload)
	}
	level := slog.LevelInfo
	if decision != "allowed" {
		level = slog.LevelWarn
	}
	slog.Log(context.Background(), level, "kit: sandbox escalation",
		"tool", request.Tool,
		"decision", decision,
		"reason", payload.Reason,
		"argv", request.ArgvLine(),
		"justification", request.Justification,
		"mode", payload.Mode,
		"risk", request.Risk)
}

// summarize 把理由压成一行短摘要（事件 payload 不该被一句长回答撑大）。
func summarize(text string, limit int) string {
	collapsed := strings.Join(strings.Fields(text), " ")
	if len(collapsed) <= limit {
		return collapsed
	}
	cut := limit
	for cut > 0 && !isRuneStart(collapsed[cut]) {
		cut--
	}
	return collapsed[:cut] + "…"
}

// isRuneStart 报告 b 是否是 UTF-8 字符的首字节。
func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
