package builtin

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// 本文件实现"逐命令提权"：让一次命令在**人工批准**后绕过 OS 沙箱执行。
//
// 为什么需要它：OS 沙箱能挡住的只是"路径与系统调用"，而真实软件还会写**非路径
// 资源**——Windows 的 HKCU 注册表（DPAPI 凭据）、macOS 的 keychain、某些 CLI 的
// 系统级 socket。这些目标没有任何"路径授权"能救：用户装/登录一个工具时，agent 只有
// 两条路，要么提权一次并让人确认，要么告诉用户"我做不到"。
//
// # 边界（提权**不**放宽任何策略）
//
//	提权唯一的作用是"这一次 spawn 不再包 osrunner"，其它一律照旧：
//	  - sandbox.Policy 的可写面、保护路径、命令允许/禁止名单：不变；
//	  - 命令风险判定（ClassifyCommand）：**危险命令即使提权也拒绝**；
//	  - workdir 校验、输出上限、超时：不变；
//	  - 审批：必须由人 allow；自动档（auto）与 never 档一律拒绝。
//
// 换句话说，提权是"对**执行隔离**的一次豁免"，不是"对策略的一次豁免"。

// EscalationRequireEscalated 是 sandbox_permissions 唯一支持的取值（线协议沿用
// 这个名字，客户端无需为不同实现分叉）。
const EscalationRequireEscalated = "require_escalated"

// MinJustificationRunes 是提权理由的最小长度（字符数，不是字节）。
//
// 为什么要门槛：提权请求会弹到人面前，"我要写注册表"和"要装东西"对人的决策质量
// 完全不同。10 个字符刚好逼出一句能看懂的话，又不至于让模型凑字数。
const MinJustificationRunes = 10

// EscalationRequest 是一次提权请求。它同时是审批 UI 的输入与审计记录。
type EscalationRequest struct {
	// Tool 是发起请求的工具名（"bash" / "pwsh"）。
	Tool string
	// Command 是模型给出的原始命令文本（审批 UI 要原样显示）。
	Command string
	// Argv 是即将执行的完整 argv（含 shell 包装），审批人看清"到底要跑什么"。
	Argv []string
	// Workdir 是解析后的工作目录。
	Workdir string
	// Justification 是模型给的理由（已校验 ≥MinJustificationRunes 个字符）。
	Justification string
	// Mode 是当前沙箱模式（read-only/workspace-write/danger-full-access）。
	Mode sandbox.Mode
	// Program 是命令分类器解析出的程序名（可能为空）。
	Program string
	// Risk 是命令风险等级（提权时不会是 dangerous——那种请求已被拒绝）。
	Risk string
	// RuleID 是分类器命中的规则 id（可能为空）。
	RuleID string
}

// ArgvLine 把 argv 渲染成一行可读文本（审批文案与事件 payload 共用）。
func (r EscalationRequest) ArgvLine() string { return strings.Join(r.Argv, " ") }

// Escalator 是宿主的提权审批通道。
//
// 实现必须**阻塞等待人工结论**（它是审批，不是策略判断）；返回 error 表示审批
// 基础设施故障（此时工具拒绝执行，绝不默认放行）。allowed=false 时工具返回可读
// 错误且不执行命令。
//
// 接口刻意定义在 tools/builtin 内、不 import gate/agent：工具只描述"我要人批准
// 什么"，怎么问人、问谁、如何记账由宿主（kit 的适配器）决定。
type Escalator interface {
	Escalate(ctx context.Context, req EscalationRequest) (allowed bool, err error)
}

// escalationHintText 是追加到 bash/pwsh 描述末尾的提权提示（模型可见，英文）。
//
// 三条信息缺一不可：① 有一个参数能让**这一条命令**在沙箱外运行；② 需要人工批准、
// 必须给理由（`justification` 就是理由）；③ 只用于"沙箱挡住了正常需求"，不是
// 绕过策略的口子。整句连同前导空格**恰好 130 字节**——它是每个回合都付的常驻成本，
// 有 TestEscalationHintBudget 钉住上限。
const escalationHintText = ` Legit need blocked? Retry that one command with sandbox_permissions:"require_escalated"+justification; human-approved, no bypass.`

// escalationAvailable 报告"这个组合里提权真的能走通"。
//
// 三个条件全部满足才算可用，缺一就是"指向一个必然失败的参数"：
//   - 宿主接了提权通道（Escalator）——没接时运行期会直接拒绝；
//   - 通道自报可用（kit 的 approver 在"没有 Asker"或"approval=auto/never"时
//     会 Available()==false，此时它也非 nil，不能只判非 nil）；
//   - 真的挂了 OS runner 且策略不是 danger-full-access——没有沙箱就没有"跳出
//     沙箱"这回事；
//   - 策略不是 read-only——那一档**每条**提权都会被硬拒（read-only 的命令一律在
//     沙箱内跑，见 escalation.go 的判定），广告它只是在指向一个必然失败的操作。
func escalationAvailable(options Options, policy *sandbox.Policy) bool {
	if options.Escalator == nil || options.Runner == nil {
		return false
	}
	if policy != nil && (policy.Mode() == sandbox.ModeDangerFullAccess || policy.Mode() == sandbox.ModeReadOnly) {
		return false
	}
	if probe, ok := options.Escalator.(interface{ Available() bool }); ok {
		return probe.Available()
	}
	return true
}

// escalationHint 返回应追加到描述末尾的提权提示；不可用时返回空串。
//
// 返回空串而不是"此组合不支持提权"的另一种措辞：常驻工具的描述是每回合都付的
// 钱，而"不可用"这件事在**被拒绝的那一刻**由 sandbox.Denial.NextSteps 说（它会
// 明确告诉模型"让用户在设置里开启提权"）。同一份 description 不能对两种组合说
// 同样的话，但也没必要为不可用的组合多付一份字数。
func escalationHint(options Options, policy *sandbox.Policy) string {
	if !escalationAvailable(options, policy) {
		return ""
	}
	return escalationHintText
}

// sandboxPermissionsDescription 返回 sandbox_permissions 字段的说明。
//
// 不可用时不描述"怎么提权"，而是如实说"这个组合里调它会被拒绝"——否则 schema
// 本身就在指向一个必然失败的操作。
func sandboxPermissionsDescription(available bool) string {
	if !available {
		return "Not available in this composition: setting it makes the call fail (no escalation channel or no OS sandbox runner)."
	}
	return `Set to "require_escalated" to run this one command outside the OS sandbox (human approval required; policy rules still apply).`
}

// justificationDescription 返回 justification 字段的说明（与上面对齐）。
func justificationDescription(available bool) string {
	if !available {
		return "Only meaningful with sandbox_permissions (unavailable in this composition)."
	}
	return "Why this command cannot run inside the sandbox; required with sandbox_permissions, at least 10 characters, read by the approver."
}

// escalationOutcome 是一次提权判定的结果。
type escalationOutcome struct {
	// Escalated 为真表示"这条命令已获准在沙箱外执行"。
	Escalated bool
	// Note 是追加到工具结果末尾的审计说明（空表示不加）。
	Note string
}

// escalationInput 是 resolveEscalation 的输入（两个 shell 工具共用）。
type escalationInput struct {
	tool               string
	command            string
	sandboxPermissions string
	justification      string
	argv               []string
	workdir            string
	policy             *sandbox.Policy
	windows            bool
}

// resolveEscalation 解析并执行一次提权请求。
//
// 返回的 error 会作为工具错误回给模型（不是 Go error）：模型能读懂"为什么没批准"，
// 从而换做法而不是原样重试。
//
// 调用时机：**必须在策略校验（只读/允许名单/命令语法）之后**、spawn 之前。这样
// "策略不许"永远优先于"人也许批准"，不会出现"提权绕过只读策略"。
func resolveEscalation(ctx context.Context, opts Options, in escalationInput) (escalationOutcome, error) {
	permissions := strings.TrimSpace(in.sandboxPermissions)
	justification := strings.TrimSpace(in.justification)
	if permissions == "" {
		if justification != "" {
			return escalationOutcome{}, fmt.Errorf(
				"%s: justification requires sandbox_permissions=%q", in.tool, EscalationRequireEscalated)
		}
		return escalationOutcome{}, nil
	}
	// 没有提权通道：保持 v0.20 的逐字节兼容错误。**先判这一条**（而不是先校验
	// 取值），这样"没接提权通道"的组合里，任何 sandbox_permissions 取值都得到
	// 同一条旧错误——向后兼容是逐字节的，不随参数怎么写出变。
	if opts.Escalator == nil {
		return escalationOutcome{}, fmt.Errorf(
			"%s: sandbox_permissions is not available in this composition (no sandboxing executor to escalate)", in.tool)
	}
	if !strings.EqualFold(permissions, EscalationRequireEscalated) {
		return escalationOutcome{}, fmt.Errorf(
			"%s: unsupported sandbox_permissions %q (the only supported value is %q)",
			in.tool, permissions, EscalationRequireEscalated)
	}
	if utf8.RuneCountInString(justification) < MinJustificationRunes {
		return escalationOutcome{}, fmt.Errorf(
			"%s: justification is required with sandbox_permissions=%q and must say why this command cannot run inside the sandbox (at least %d characters)",
			in.tool, EscalationRequireEscalated, MinJustificationRunes)
	}
	// 没有 OS 沙箱 runner：命令本来就跑在沙箱外，提权没有意义（且不该给它一个
	// "绕过审批"的口子——直接拒绝，让人去接 runner）。
	if opts.Runner == nil {
		return escalationOutcome{}, fmt.Errorf(
			"%s: sandbox_permissions requires an OS sandbox runner; this composition already runs commands unconfined", in.tool)
	}
	// read-only 档不允许提权：那一档的**唯一**写围栏就是 OS 沙箱，跳过它就等于把
	// 只读会话变成全权会话。提权是用来突破"某条命令要写沙箱外的东西"，不是用来
	// 改变档位本身的；要放宽请切 workspace-write 或申请一个可写目录。
	if in.policy != nil && in.policy.ReadOnly() {
		return escalationOutcome{}, fmt.Errorf(
			"%s: sandbox_permissions cannot be used in read-only mode; read-only commands always run inside the OS sandbox "+
				"(switch to workspace-write, or ask the user to grant one writable directory)", in.tool)
	}

	// 危险命令即使提权也拒绝：提权只豁免"执行隔离"，不豁免"破坏性"。
	assessment := sandbox.ClassifyShellCommandOnPlatform(in.command, in.windows)
	if assessment.Risk == sandbox.RiskDangerous {
		return escalationOutcome{}, fmt.Errorf(
			"%s: refusing to escalate a dangerous command (%s: %s); escalation only skips the OS sandbox, destructive commands stay blocked",
			in.tool, assessment.Detail, assessment.Reason)
	}

	request := EscalationRequest{
		Tool:          in.tool,
		Command:       in.command,
		Argv:          append([]string(nil), in.argv...),
		Workdir:       in.workdir,
		Justification: justification,
		Mode:          modeOf(in.policy),
		Program:       assessment.Program,
		Risk:          assessment.Risk.String(),
		RuleID:        assessment.Detail,
	}
	allowed, err := opts.Escalator.Escalate(ctx, request)
	if err != nil {
		return escalationOutcome{}, fmt.Errorf("%s: sandbox escalation could not be approved: %w", in.tool, err)
	}
	if !allowed {
		return escalationOutcome{}, fmt.Errorf(
			"%s: the user did not approve running this command outside the sandbox; "+
				"do not retry the same command with sandbox_permissions again. Try a workspace-local path or a tool-specific flag first, "+
				"or ask the user to run it themselves", in.tool)
	}
	return escalationOutcome{
		Escalated: true,
		Note: fmt.Sprintf("[sandbox: escalated — this command ran outside the OS sandbox with human approval "+
			"(mode %s; policy roots and command rules still applied)]", modeOf(in.policy)),
	}, nil
}

// modeOf 返回策略的沙箱模式（nil 策略按 workspace-write 展示）。
func modeOf(policy *sandbox.Policy) sandbox.Mode {
	if policy == nil {
		return sandbox.ModeWorkspaceWrite
	}
	return policy.Mode()
}

// applyEscalationNote 把提权审计说明追加到工具结果末尾。
func applyEscalationNote(result rig.ToolResult, outcome escalationOutcome) rig.ToolResult {
	if !outcome.Escalated || outcome.Note == "" {
		return result
	}
	if strings.TrimSpace(result.Content) == "" {
		result.Content = outcome.Note
		return result
	}
	result.Content += "\n" + outcome.Note
	return result
}
