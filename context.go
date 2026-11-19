package rig

import (
	"context"
	"strings"
	"unicode/utf8"
)

// Compactor 在上下文接近预算时压缩会话历史。
//
// messages 是按时间顺序排列的对话副本（含 system 消息），budget 是估算的 token
// 预算（见 EstimateTokens）。返回的消息会替换下一轮请求中的对话；实现不得修改
// 或持有调用方传入的切片。实现必须可跨会话并发使用。
//
// 压缩策略由宿主选择：WindowCompactor 做确定性的窗口裁剪；
// session/compact.LLMCompactor 先做窗口裁剪再用模型写摘要。agent 只在
// AgentSpec.ContextBudget > 0 且配置了 Compactor 时调用。
type Compactor interface {
	Compact(ctx context.Context, sessionID string, messages []Message, budget int) ([]Message, error)
}

// EstimateTokens 粗略估算一组消息的 prompt token 数。ASCII 文本按约 4 字符/
// token 计，CJK 文本按约 1 字符/token 计，图片按固定 token 计；推理文本与工具
// 调用参数也计入，因为兼容 OpenAI 的 provider 会把它们随请求发送。
func EstimateTokens(messages []Message) int {
	tokens := 0
	for _, message := range messages {
		for _, part := range message.Content {
			switch part.Type {
			case PartImageURL:
				tokens += 1024
			case PartText:
				tokens += EstimateTextTokens(part.Text)
			}
		}
		tokens += EstimateTextTokens(message.Reasoning)
		if message.ToolCallID != "" || message.Name != "" {
			tokens += (len(message.ToolCallID) + len(message.Name) + 3) / 4
		}
		for _, call := range message.ToolCalls {
			tokens += (len(call.ID) + len(call.Name) + len(call.Arguments) + 3) / 4
		}
	}
	return tokens
}

// EstimateTextTokens 粗略估算一段文本的 token 数：CJK 字符约 1 token，其余
// 字符约 4 字符/token。
func EstimateTextTokens(text string) int {
	if text == "" {
		return 0
	}
	tokens, ascii := 0, 0
	for _, r := range text {
		if r < utf8.RuneSelf {
			ascii++
			continue
		}
		if ascii > 0 {
			tokens += (ascii + 3) / 4
			ascii = 0
		}
		if isCJKRune(r) {
			tokens++
		} else {
			tokens += 1 // 非 ASCII、非 CJK 字符按保守的 1 token 计
		}
	}
	if ascii > 0 {
		tokens += (ascii + 3) / 4
	}
	return tokens
}

func isCJKRune(r rune) bool {
	return (r >= 0x3040 && r <= 0x30FF) || // 日文假名
		(r >= 0x3400 && r <= 0x4DBF) || // CJK 扩展 A
		(r >= 0x4E00 && r <= 0x9FFF) || // CJK 统一表意文字
		(r >= 0xAC00 && r <= 0xD7AF) || // 韩文音节
		(r >= 0xF900 && r <= 0xFAFF) // CJK 兼容表意文字
}

// SpillPolicy 控制超过阈值的工具结果如何落盘。落盘后模型只看到摘要与文件路径，
// 避免每轮重发完整工具输出；agent 在记录 tool 消息前应用该策略。
type SpillPolicy struct {
	// Dir 是落盘目录（相对路径相对宿主工作目录）；为空时使用
	// <系统临时目录>/zeokeel-rig-spill。
	Dir string
	// ThresholdBytes 是触发落盘的最小内容字节数；<=0 时使用 16 KiB。
	ThresholdBytes int
	// MaxFiles 是保留的落盘结果上限（先进先出）；<=0 时不清理。
	//
	// 设了 Store 之后它同样生效（每条新结果写入后按该上限回收一次），不是只在
	// 文件后端生效。
	MaxFiles int
	// Store 非 nil 时，超大结果存进宿主自己的存储（数据库行、对象存储），
	// 而不是落成目录里的文件；模型看到的是句柄而不是路径。
	//
	// 读回路径：模型看到的句柄必须能读回来，否则"结果太大，已存起来"就等于
	// "结果永久丢失"。`tools/full.All` 在 AllOptions.SpillStore 非 nil 时会注册
	// `spill_read` 工具，由它调本接口的 Load。宿主自己组装工具集时，需要给模型
	// 一个等价的读回入口（或直接用自己的 UI 呈现）。
	//
	// 回收：实现必须提供 GC；agent 层在每次 Save 之后按 MaxFiles 调一次
	// （见 agent/spill.go）。直接使用本接口、不经过 agent 的宿主需要自己定期
	// 调 GC，否则存储会无界增长。
	Store SpillStore
}

// SpillSummaryPrefix 是落盘后替换工具结果的稳定前缀，便于回放与测试识别。
const SpillSummaryPrefix = "[tool result spilled to file]"

// IsSpilledToolResult 报告一条工具结果是否已被 SpillPolicy 替换为摘要。
func IsSpilledToolResult(content string) bool {
	return strings.HasPrefix(content, SpillSummaryPrefix)
}
