package agent

import (
	"fmt"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
)

// 本文件实现"审批拒绝理由"通道：让门禁在拒绝一次工具调用时，把**原因与建议**交给
// agent，由 agent 拼进模型可见的工具错误。
//
// 为什么需要它：个人 agent 客户端最常见的坏循环是"模型被拒 → 看不到原因 → 原样重试
// 同一条命令"。rig.Gate 的契约只有 Decision，因此 agent 引入一个**可选**接口：实现
// 它的门禁多给一段解释，不实现的门禁行为完全不变。

// DenialExplainer 是 rig.Gate 的**可选**扩展：门禁在拒绝一次调用后解释原因。
//
// # 为什么定义在 agent，而不是根包 rig
//
//   - 根包 rig 必须保持 stdlib-only 的最小契约层：rig.Gate 只有一个方法，任何
//     "如何解释拒绝、解释成什么语言"都是实现细节，不该进契约；
//   - 解释的**唯一消费者**是 agent（它负责把拒绝渲染成 role=tool 消息），按 Go 的
//     惯例接口应定义在使用方（"accept interfaces where you use them"）；
//   - 不会造成 import 环：agent 只依赖 rig；门禁（gate 包）只要提供**同名同签名**
//     的方法就自动满足本接口——Go 的结构化类型让双方都不必互相 import。
//
// # 契约
//
//   - 只在 Approve 返回 DecisionDeny 之后被调用（放行时不调用，避免副作用）；
//   - 返回空串（或全空白）表示"没有理由"，agent 退回历史文案；
//   - 在运行 goroutine 上同步调用：必须足够快、并发安全，且不得阻塞等待用户输入；
//   - 文本会**原样进入模型上下文**，应写成"给人看的中文短句 + 可操作建议"，且不要
//     回显密钥类参数（例如完整的 token）。
type DenialExplainer interface {
	ExplainDenial(tool rig.Tool, call rig.ToolCall) string
}

// explainDenial 调用门禁的可选解释接口；未实现或返回空白时返回空串。
func explainDenial(g rig.Gate, tool rig.Tool, call rig.ToolCall) string {
	if g == nil {
		return ""
	}
	explainer, ok := g.(DenialExplainer)
	if !ok {
		return ""
	}
	return strings.TrimSpace(explainer.ExplainDenial(tool, call))
}

// deniedToolCallMessage 渲染一次审批拒绝的模型可见文案。
//
// 两种形态：
//
//	有解释：rig: tool call denied: <原因> (do not repeat the same call unchanged; …)
//	无解释：rig: tool call denied by approval gate: <tool>        ← 历史文案，逐字节不变
//
// 第二种必须保持不变：既有宿主、提示词与快照都依赖它，而且"没理由"时多说一句反而
// 是噪声。
func deniedToolCallMessage(g rig.Gate, tool rig.Tool, call rig.ToolCall) string {
	if reason := explainDenial(g, tool, call); reason != "" {
		return fmt.Sprintf("rig: tool call denied: %s (do not repeat the same call unchanged; "+
			"change the approach, explain the blocker to the user, or ask for permission)", reason)
	}
	return fmt.Sprintf("rig: tool call denied by approval gate: %s", call.Name)
}
