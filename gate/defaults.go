package gate

import (
	"github.com/zeokeel/zeokeel-rig"
)

// 本文件提供**个人 agent 客户端**的推荐默认门禁组合。
//
// 产品定位是单用户的个人客户端：最重要的不是"多层审批"，而是既别让人被无意义的
// 弹窗打断、也别让模型在危险命令上反复试错。因此默认组合按"能力从弱到强"叠加：

// PersonalDefault 返回个人客户端推荐的默认门禁组合：
//
//	gate.PersonalDefault(gate.Ask(host.Asker))
//
// 等价于：
//
//	gate.CommandRiskGate(gate.AllowThenAsk(gate.SandboxReadOnlyTools, inner))
//
// 三层各管一件事（顺序不能反）：
//
//  1. **命令级风险**（最外层 CommandRiskGate）：bash/pwsh 的命令先做
//     read-only/mutating/dangerous 判定——只读命令（ls、git status、grep…）**免审批**，
//     危险命令（rm -rf /、mkfs、curl|sh…）**直接拒绝**并给出规则 id 与建议；
//  2. **工具级白名单**（AllowThenAsk + SandboxReadOnlyTools）：read_file、glob、
//     grep 这类明确只读的工具**免审批**；
//  3. **人工审批**（inner）：其余一切（写文件、改配置、可变命令）交给宿主审批通道。
//
// 为什么 CommandRiskGate 必须在最外层：AllowThenAsk 只看工具名，一旦把 bash 放行，
// 其中的 `ls` 与 `rm -rf /` 就无差别了；反过来若把 CommandRiskGate 放在内层，则每条
// bash 命令都会先弹一次窗才轮到风险判定。
//
// ⚠️ inner 不要传 nil——但**它不会再让会话死掉**：PersonalDefault(nil) 在"需要审批
// 但没有内层门禁"时返回 DecisionDeny + 理由（NoApprovalChannelReason /
// NoApprovalChannelToolReason），模型看到"没有配置审批通道，本次调用未执行"并可以换
// 办法，run 继续。
//
// 为什么仍然不要传 nil：那意味着**每次需要审批的调用都必然被拒**——用户会看到模型反复
// 被拒、功能不可用。没有审批通道时的正确写法是给一个"拒绝但有理由"的兜底门禁：
//
//	g := gate.PersonalDefault(gate.DenyAll())        // 拒绝一切，理由说明 approval=never
//	g := gate.PersonalAskWithoutAsker()              // 更好：理由说明"没有审批通道"
//
// 历史说明：这个分支曾经返回 Go error，agent 见到 error 会**中止整个 run**（模型没有
// 下一轮、用户看到"运行失败"）。审计 2C 认定这是最差的失败模式，已改为 deny + 理由。
func PersonalDefault(inner rig.Gate) rig.Gate {
	return CommandRiskGate(AllowThenAsk(SandboxReadOnlyTools, inner))
}

// PersonalAsk 是 PersonalDefault 的别名，对应"审批三档"里的 ask 档：只读免问、
// 危险命令直接拒绝、其余交给人工审批（inner 通常是 gate.Ask(host.Asker)）。
//
//	g := gate.PersonalAsk(gate.Ask(host.Asker))
//
// 与 PersonalDefault 完全等价，命名只是为了和三档映射（PersonalAsk/PersonalAuto/
// PersonalNever）对齐。
//
// 若宿主**没有 Asker**（没有审批通道），用 gate.PersonalAskWithoutAsker()，不要用
// PersonalAsk(gate.DenyAll())：后者的文案会说"approval=never"，与 ask 模式的真实
// 状态不符（真实情况是"没有通道可问"），会误导模型。
//
// 传 nil 也不会中断 run（返回 deny + "没有配置审批通道"理由），但那样每次需要审批的
// 调用都会被拒——属于配置疏漏，不是可用状态。
func PersonalAsk(inner rig.Gate) rig.Gate {
	return PersonalDefault(inner)
}

// PersonalAskWithoutAsker 是"ask 模式但没有审批通道"的正确兜底：
//
//	g := gate.PersonalAskWithoutAsker()
//	// == gate.PersonalAsk(gate.DenyAllWithReason(gate.DenyAllNoChannelReason))
//
// 语义：只读工具与只读命令照常免问放行；危险命令按规则拒绝（理由带规则 id）；
// 其余调用拒绝，并明确告诉模型"没有审批通道，请让用户接上 Ask 或改用 auto"——而不是
// 声称处于 approval=never（那会让模型反复重试或断言错误的状态）。
//
// 它适合作为 kit 里 ApprovalAsk 在 cfg.Asker == nil 时的默认值。
func PersonalAskWithoutAsker() rig.Gate {
	return PersonalAsk(DenyAllWithReason(DenyAllNoChannelReason))
}

// PersonalAuto 对应"审批三档"里的 auto 档：**不问人**，但危险命令仍然被拒。
//
//	g := gate.PersonalAuto()   // == gate.CommandRiskGate(gate.AllowAll())
//
// 为什么 auto 不等于裸 AllowAll：裸 AllowAll 会放行 `rm -rf /`、`mkfs`、`curl … |
// sh` 这类不可撤销的破坏性命令——个人客户端里一次误判就等于数据丢失。PersonalAuto
// 保留命令级判定：只读命令与一切常规操作照常执行（不打断人），只有明确危险的动作
// 直接拒绝，并把"命中哪条规则"作为工具错误回给模型，让它换个做法而不是重试。
//
// 注意：它不弹窗、也不询问；需要人确认时用 PersonalAsk。
func PersonalAuto() rig.Gate {
	return CommandRiskGate(AllowAll())
}

// PersonalNever 对应"审批三档"里的 never 档：拒绝所有工具调用，并给出理由
// （"approval=never"），避免模型把"被拒绝"误读成"暂时故障"而反复重试。
func PersonalNever() rig.Gate {
	return DenyAll()
}
