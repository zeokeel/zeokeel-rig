package gate

import (
	"github.com/zeokeel/zeokeel-rig"
)

// 本文件提供**个人 agent 客户端**的推荐默认门禁组合。
//
// # 与 DSH 对齐：只有两档审批
//
// 审批档位与 DSH（DeepSeek Harness）一致，只有两个：**ask**（逐调用问人）与
// **never**（全拒）。**不存在"危险命令硬拒"这一档**：`rm -rf /`、`curl … | sh`
// 这类命令在 ask 档下与其它可变调用一样**走人工审批**，由人决定；在 never 档下与
// 其它一切调用一样被拒。
//
// 为什么去掉命令级风险门禁（历史说明）：早期默认组合把 CommandRiskGate 放在最外层，
// 危险命令不弹窗、直接拒绝。它有两个问题：①判定完全依赖进程内的规则表，规则表漏掉
// 一种写法（例如命令走 `job_start` 而不是 `bash`）就等于放行，多判一种写法就是误拒；
// ②"直接拒绝"让人在真正需要时没有出路，模型也只能反复试错。现在的判据只有一条：
// **谁能批准，谁就有权决定**。CommandRiskGate 仍然完整保留（宿主可自行组合），只是
// 默认预设不再使用它；本包其余文档里凡是出现"危险命令直接拒绝"的段落，指的都是
// **显式组合了 CommandRiskGate** 的场景。

// PersonalDefault 返回个人客户端推荐的默认门禁组合：
//
//	gate.PersonalDefault(gate.Ask(host.Asker))
//
// 等价于：
//
//	gate.AllowThenAsk(gate.SandboxReadOnlyTools, inner)
//
// 两层各管一件事：
//
//  1. **工具级白名单**（AllowThenAsk + SandboxReadOnlyTools）：read_file、glob、
//     grep 这类明确只读的工具**免审批**；
//  2. **人工审批**（inner）：其余一切（写文件、改配置、bash/pwsh/后台任务/终端）交给
//     宿主审批通道——**包括 `rm -rf /` 这类危险命令**，它们不再被硬拒绝，而是问人。
//
// 免问是按**工具名**的，因此 bash/pwsh/job_start/terminal_send 这些能执行任意命令的
// 工具永远不在免问名单里：它们的每一次调用都会问人（无论命令文本写的是什么）。
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
	return AllowThenAsk(SandboxReadOnlyTools, inner)
}

// PersonalAsk 是 PersonalDefault 的别名，对应"审批两档"里的 ask 档：只读工具免问，
// 其余（含危险命令）交给人工审批（inner 通常是 gate.Ask(host.Asker)）。
//
//	g := gate.PersonalAsk(gate.Ask(host.Asker))
//
// 与 PersonalDefault 完全等价，命名只是为了和档位映射（PersonalAsk/PersonalAuto/
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
// 语义：只读工具照常免问放行；其余调用（**包括危险命令**）一律拒绝，并明确告诉模型
// "没有审批通道，请让用户接上 Ask 或改用 auto"——而不是声称处于 approval=never
// （那会让模型反复重试或断言错误的状态）。危险命令在这里与普通调用得到同一条理由：
// 既然没有通道，就不存在"谁批准了这条命令"。
//
// 它适合作为 kit 里 ApprovalAsk 在 cfg.Asker == nil 时的默认值。
func PersonalAskWithoutAsker() rig.Gate {
	return PersonalAsk(DenyAllWithReason(DenyAllNoChannelReason))
}

// PersonalAuto 对应"审批两档"里的 auto 档：**不问人，也不做任何命令分类**。
//
//	g := gate.PersonalAuto()   // == gate.AllowAll()
//
// 与 DSH 对齐：auto 的语义就是"不要让审批介入"，因此不再附带一层"危险命令硬拒"。
// 它放行 `rm -rf /`、`mkfs`、`curl … | sh`——这正是"不问人"的含义；需要保护就换 ask
// 档（PersonalAsk，由人决定），或由宿主自己叠 `CommandRiskGate`/`sandbox` 策略。
//
// 注意：它不弹窗、也不询问；需要人确认时用 PersonalAsk。
func PersonalAuto() rig.Gate {
	return AllowAll()
}

// PersonalNever 对应"审批两档"里的 never 档：拒绝所有工具调用，并给出理由
// （"approval=never"），避免模型把"被拒绝"误读成"暂时故障"而反复重试。
func PersonalNever() rig.Gate {
	return DenyAll()
}
