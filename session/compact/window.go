// Package compact 提供上下文压缩实现：确定性的窗口裁剪（WindowCompactor）和
// 基于模型摘要的压缩（LLMCompactor）。两者都实现根包 rig.Compactor，宿主通过
// kit.Config 或 agent.AgentSpec 配置 ContextBudget 后由 agent 自动调用。
//
// 压缩只作用于下一轮请求的内存对话；是否把压缩结果持久化替换历史由宿主决定。
package compact

import (
	"context"
	"fmt"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
)

// 默认值。
const (
	// DefaultKeepLast 是 WindowCompactor 默认保留的最近非 system 消息数。
	DefaultKeepLast = 8
	// DefaultImageKeepLast 是压缩后仍保留图片的最新消息数。
	DefaultImageKeepLast = 2
	// ImageOffloadPlaceholder 替换较早图片的文本占位符。
	ImageOffloadPlaceholder = "[image omitted to recover context]"
	// SummaryPrefix 是压缩摘要消息的稳定前缀；重复压缩会替换旧摘要而不是累积。
	SummaryPrefix = "Earlier conversation compacted ("
	// ToolResultPrunedMarker 替换较早工具结果的占位符。
	ToolResultPrunedMarker = "[tool result pruned]"
)

// WindowCompactor 是确定性的上下文压缩器：保留所有 system 消息、最近 KeepLast
// 条非 system 消息，并在两者之间插入一条摘要消息。它不调用模型，适合作为
// LLMCompactor 的 fallback 或低延迟场景。
type WindowCompactor struct {
	// KeepLast 保留的最近非 system 消息数；<=0 时使用 DefaultKeepLast。
	KeepLast int
}

var _ rig.Compactor = WindowCompactor{}

// Compact 实现 rig.Compactor。
func (c WindowCompactor) Compact(ctx context.Context, sessionID string, messages []rig.Message, budget int) ([]rig.Message, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	if len(messages) == 0 {
		return nil, nil
	}
	messages = OffloadOldImages(messages, DefaultImageKeepLast)
	keepLast := c.KeepLast
	if keepLast <= 0 {
		keepLast = DefaultKeepLast
	}
	system, rest := splitConversation(messages)
	if len(rest) <= keepLast {
		return CloneMessages(messages), nil
	}
	dropped := boundary(rest, keepLast)
	if dropped == 0 {
		return CloneMessages(messages), nil
	}
	recent := CloneMessages(rest[dropped:])
	out := make([]rig.Message, 0, len(system)+1+len(recent))
	out = append(out, system...)
	out = append(out, SummaryMessage(dropped, ""))
	out = append(out, recent...)
	return out, nil
}

// SummaryMessage 构造压缩摘要消息；summary 为空时只包含稳定的消息计数前缀。
func SummaryMessage(dropped int, summary string) rig.Message {
	return rig.NewMessage(rig.RoleSystem, CompactionSummaryContent(dropped, summary))
}

// CompactionSummaryContent 渲染稳定的摘要内容。若 summary 已带前缀则原样返回，
// 避免手工压缩产生重复前缀。
func CompactionSummaryContent(dropped int, summary string) string {
	summary = strings.TrimSpace(summary)
	if strings.HasPrefix(summary, SummaryPrefix) {
		return summary
	}
	frame := fmt.Sprintf("%s%d messages)", SummaryPrefix, dropped)
	if summary == "" {
		return frame
	}
	return frame + "\n\n" + summary
}

// IsCompactionSummary 报告消息是否是此前压缩产生的摘要（system 或 assistant
// 角色都接受，兼容手动压缩持久化后的形态）。
func IsCompactionSummary(message rig.Message) bool {
	if message.Role != rig.RoleSystem && message.Role != rig.RoleAssistant {
		return false
	}
	return strings.HasPrefix(message.Text(), SummaryPrefix)
}

// splitConversation 保留普通 system 消息，丢弃旧摘要，返回可参与压缩的其余消息。
func splitConversation(messages []rig.Message) (system, rest []rig.Message) {
	system = make([]rig.Message, 0, len(messages))
	rest = make([]rig.Message, 0, len(messages))
	for _, message := range messages {
		if IsCompactionSummary(message) {
			continue
		}
		if message.Role == rig.RoleSystem {
			system = append(system, message)
			continue
		}
		rest = append(rest, message)
	}
	return system, rest
}

// boundary 计算保留窗口的起点；若起点落在工具结果块中间，则向前扩到该块的
// 第一条 tool 消息，保证 assistant tool_calls 与全部结果一起保留。
func boundary(rest []rig.Message, keepLast int) int {
	dropped := len(rest) - keepLast
	if dropped <= 0 {
		return 0
	}
	if dropped >= len(rest) {
		return len(rest)
	}
	for dropped > 0 && rest[dropped].Role == rig.RoleTool {
		dropped--
	}
	return dropped
}

// OffloadOldImages 把保留窗口之外的图片分片替换为文本占位符；最新 keepLast 条
// 含图片消息保持不变。返回新切片，不修改调用方消息。
func OffloadOldImages(messages []rig.Message, keepLast int) []rig.Message {
	if keepLast < 0 {
		keepLast = 0
	}
	if keepLast == 0 {
		keepLast = DefaultImageKeepLast
	}
	imageIndexes := make([]int, 0)
	for i, message := range messages {
		for _, part := range message.Content {
			if part.Type == rig.PartImageURL {
				imageIndexes = append(imageIndexes, i)
				break
			}
		}
	}
	if len(imageIndexes) <= keepLast {
		return messages
	}
	offload := make(map[int]bool, len(imageIndexes)-keepLast)
	for _, index := range imageIndexes[:len(imageIndexes)-keepLast] {
		offload[index] = true
	}
	out := make([]rig.Message, len(messages))
	for i, message := range messages {
		out[i] = cloneMessage(message)
		if !offload[i] {
			continue
		}
		parts := make([]rig.ContentPart, 0, len(message.Content))
		for _, part := range message.Content {
			if part.Type == rig.PartImageURL {
				parts = append(parts, rig.TextPart(ImageOffloadPlaceholder))
				continue
			}
			parts = append(parts, part)
		}
		out[i].Content = parts
	}
	return out
}

// PruneToolResults 把最近 keepLast 条之外的工具结果替换为稳定占位符；keepLast
// <=0 时使用 DefaultToolResultKeepLast。
func PruneToolResults(messages []rig.Message, keepLast int) []rig.Message {
	if keepLast < 0 {
		return CloneMessages(messages)
	}
	if keepLast == 0 {
		keepLast = 4
	}
	out := CloneMessages(messages)
	seen := 0
	for i := len(out) - 1; i >= 0; i-- {
		if out[i].Role != rig.RoleTool {
			continue
		}
		seen++
		if seen <= keepLast {
			continue
		}
		out[i].Content = []rig.ContentPart{rig.TextPart(ToolResultPrunedMarker)}
	}
	return out
}

// CloneMessages 深拷贝消息切片（包括 Content 与 ToolCalls 切片）。
func CloneMessages(messages []rig.Message) []rig.Message {
	out := make([]rig.Message, len(messages))
	for i, message := range messages {
		out[i] = cloneMessage(message)
	}
	return out
}

func cloneMessage(message rig.Message) rig.Message {
	out := message
	if message.Content != nil {
		out.Content = append([]rig.ContentPart(nil), message.Content...)
	}
	if message.ToolCalls != nil {
		out.ToolCalls = append([]rig.ToolCall(nil), message.ToolCalls...)
	}
	return out
}
