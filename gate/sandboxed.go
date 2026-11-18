package gate

// 本文件实现"已沙箱化的命令免审批"：`AllowSandboxedCommands`。
//
// # 为什么
//
// 沙箱与审批是两种**独立**的成本。一条命令已经被 OS 层限制到"只能写工作区、不能
// 连网"时，再要求人工逐条同意是把两个成本叠加：用户会为了少点几十次弹窗而直接关掉
// 审批（连危险命令的拦截也一起丢了）。因此把"已沙箱化"当作免审批的**前提**，而不是
// 额外条件——安全性由围栏保证，而不是由点击次数保证。
//
// # 免问判据（全部满足才免问，否则完全交给 inner）
//
//  1. 工具是 shell 类（默认 `bash`/`pwsh`）——文件工具不经 OS runner，谈不上"已隔离"；
//  2. `Runner != nil` 且它自报能强制（`RunnerStatus` 的 Enforcement 不是 none）；
//  3. `Policy != nil` 且模式不是 `danger-full-access`（那个模式没有围栏，自然不免问）；
//  4. 命令**不是** dangerous（危险命令在更外层就被拒；这里再判一次是纵深防御）；
//  5. 命令能解析出来（解析不了的交给 inner，不猜）。
//
// 注意这里**不重复调用** `Runner.Confine`：Confine 的真实成败由工具在 spawn 之前
// 决定，失败即 fail closed（命令不执行、工具返回可读错误），因此不会出现"以为在
// 沙箱里却裸跑"。门禁只做"会不会被包住"的静态判断（runner 有能力 + 策略不是
// danger-full-access + 工具确实会走 runner）。
//
// # 位置
//
// 它必须放在 `CommandRiskGate` **之内**、`Ask`（或 `PersonalAsk`）**之外**：
//
//	gate.PersonalDefault(inner)              // = CommandRiskGate(AllowThenAsk(readOnly, inner))
//	// inner = AllowSandboxedCommands(opts, Ask(asker))
//
// 这样危险命令仍然在最外层被拒、只读命令仍然在 AllowThenAsk 被放行，只有"可变但不
// 危险、且会被 runner 包住"的那一类才走本门禁的免问路径。

import (
	"context"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// SandboxedAllowReason 是免问决策的理由字符串（进 approval.response 的 reason 字段，
// 也出现在诊断里）。UI 可以据此显示"这条没问是因为已隔离"。
const SandboxedAllowReason = "sandboxed"

// SandboxedOptions 配置 AllowSandboxedCommands。
type SandboxedOptions struct {
	// Runner 是宿主的 OS 沙箱实现；nil 表示没有围栏（不免问）。
	Runner sandbox.Runner
	// Policy 是当前策略；nil 或 danger-full-access 时不免问。
	Policy *sandbox.Policy
	// ShellTools 覆盖 shell 类工具名（默认 "bash"、"pwsh"）。
	ShellTools []string
	// Observer 在作出"免问放行"决策时被调用一次（可选）。宿主用它把决策写进事件
	// 流（approval.response decision=allow reason=sandboxed），从而让"为什么没弹窗"
	// 可审计。回调必须快且不得阻塞。
	Observer func(AskEvent)
}

// AllowSandboxedCommands 返回"已沙箱化的命令免问"门禁：满足判据时直接放行并留痕，
// 否则原样交给 inner。
//
// inner 为 nil 时，未命中判据的调用返回 deny（与其它门禁"没有通道"的约定一致）。
func AllowSandboxedCommands(opts SandboxedOptions, inner rig.Gate) rig.Gate {
	shellTools := make(map[string]struct{}, len(opts.ShellTools))
	if len(opts.ShellTools) == 0 {
		shellTools["bash"] = struct{}{}
		shellTools["pwsh"] = struct{}{}
	} else {
		for _, name := range opts.ShellTools {
			if name != "" {
				shellTools[name] = struct{}{}
			}
		}
	}
	return funcGate{
		fn: func(ctx context.Context, tool rig.Tool, call rig.ToolCall) (rig.Decision, error) {
			if reason, ok := sandboxedAllowance(opts, shellTools, tool, call); ok {
				if opts.Observer != nil {
					opts.Observer(AskEvent{
						Stage:    AskStageDecided,
						Tool:     toolCallName(tool, call),
						Args:     call.Arguments,
						Decision: rig.DecisionAllow,
						Reason:   SandboxedAllowReason,
					})
				}
				_ = reason
				return rig.DecisionAllow, nil
			}
			if inner == nil {
				return rig.DecisionDeny, nil
			}
			return inner.Approve(ctx, tool, call)
		},
		explain: func(tool rig.Tool, call rig.ToolCall) string {
			if _, ok := sandboxedAllowance(opts, shellTools, tool, call); ok {
				return "" // 免问放行：没有拒绝，也就没有理由。
			}
			if explainer, ok := inner.(interface {
				ExplainDenial(tool rig.Tool, call rig.ToolCall) string
			}); ok {
				return explainer.ExplainDenial(tool, call)
			}
			return ""
		},
	}
}

// sandboxedAllowance 判定一次调用是否能因为"已沙箱化"而免问，返回可读理由。
func sandboxedAllowance(opts SandboxedOptions, shellTools map[string]struct{}, tool rig.Tool, call rig.ToolCall) (string, bool) {
	name := toolCallName(tool, call)
	if _, ok := shellTools[name]; !ok {
		return "", false // 文件工具不经 runner
	}
	if opts.Runner == nil {
		return "", false
	}
	if opts.Policy == nil || opts.Policy.Mode() == sandbox.ModeDangerFullAccess {
		return "", false
	}
	if _, enforcement, _ := sandbox.RunnerStatus(opts.Runner); enforcement == sandbox.EnforcementNone {
		return "", false
	}
	assessment, ok := AssessToolCommand(tool, call)
	if !ok || assessment.Risk == sandbox.RiskDangerous {
		return "", false
	}
	return SandboxedAllowReason, true
}
