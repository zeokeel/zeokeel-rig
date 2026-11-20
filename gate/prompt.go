package gate

import (
	"strings"

	"github.com/zeokeel/zeokeel-rig"
)

// 本文件是"审批提示链"的组合原语：把多段审批文案按顺序拼成模型/审批人看到的
// 那一个问题。
//
// 典型用法是"先展示 diff，再问是否允许"：
//
//	gate.Ask(host.Asker, gate.WithAskPrompt(gate.PromptChain(
//		gate.DiffPrompt(gate.WithDiffRoot(root)),
//		gate.SandboxGrantPrompt,
//	)))
//
// 段与段之间用空行分隔；返回空串的段被跳过（DiffPrompt 对非写类工具就是空串，
// 这样"不该展示 diff 的工具"不会留下空白段）。全部为空时返回空串，AskGate 会
// 回退到它自己的默认文案（工具名 + 参数），行为与不设 prompt 完全一致。

// Prompt 是审批文案渲染器：给定工具与调用，返回要展示给审批人的那一段文本。
//
// 返回空串表示"这段不适用"（PromptChain 会跳过它）。实现必须并发安全且不得有
// 副作用：它会在审批等待之前被调用，可能同时服务于多个 Run。
type Prompt func(tool rig.Tool, call rig.ToolCall) string

// PromptChain 按顺序组合多条 prompt：非空段以空行拼接，顺序即参数顺序。
//
// 不传任何 prompt 时返回的函数总是返回空串（等价于"不设 prompt"）。
func PromptChain(prompts ...Prompt) Prompt {
	chain := append([]Prompt(nil), prompts...)
	return func(tool rig.Tool, call rig.ToolCall) string {
		segments := make([]string, 0, len(chain))
		for _, prompt := range chain {
			if prompt == nil {
				continue
			}
			segment := strings.TrimSpace(prompt(tool, call))
			if segment == "" {
				continue
			}
			segments = append(segments, segment)
		}
		return strings.Join(segments, "\n\n")
	}
}
