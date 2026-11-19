// Package compact 提供上下文压缩实现：确定性的窗口裁剪（WindowCompactor）和
// 基于模型摘要的压缩（LLMCompactor）。两者都实现根包 rig.Compactor，宿主通过
// kit.Config 或 agent.AgentSpec 配置 ContextBudget 后由 agent 自动调用。
//
// 压缩只作用于下一轮请求的内存对话；是否把压缩结果持久化替换历史由宿主决定。
package compact

import (
	"context"
	"fmt"
	"strconv"
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
	// BudgetTruncatedMarker 是"为放进上下文预算而截断正文"的尾注。
	BudgetTruncatedMarker = "\n…[truncated to fit the context budget]"
)

// WindowCompactor 是确定性的上下文压缩器：保留所有 system 消息、最近 KeepLast
// 条非 system 消息，并在两者之间插入一条摘要消息。它不调用模型，适合作为
// LLMCompactor 的 fallback 或低延迟场景。
type WindowCompactor struct {
	// KeepLast 保留的最近非 system 消息数；<=0 时使用 DefaultKeepLast。
	KeepLast int
	// ToolResultKeepLast 保留最近 N 条工具结果原文，更早的替换成占位符（保住
	// assistant tool_calls 与 tool 消息的配对，只丢内容，能显著省 token）。
	// >0 时生效；0（默认）表示不裁剪工具结果；负数与 0 等价。
	ToolResultKeepLast int
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
	system, summaries, rest := splitConversation(messages)
	priorCount, priorBody := summarizePrior(summaries)
	dropped := 0
	if len(rest) > keepLast {
		dropped = boundary(rest, keepLast)
	}
	// 无论是否需要丢消息，都要过一遍预算：整段对话本身超预算（一次性贴了大文件）
	// 时，"压缩"必须仍然把 token 压下来，否则等于没发生。
	recent := CloneMessages(rest[dropped:])
	if c.ToolResultKeepLast > 0 {
		recent = PruneToolResults(recent, c.ToolResultKeepLast)
	}
	if dropped == 0 {
		// 本轮没有丢弃消息：没有新摘要可写，但旧摘要不能凭空消失——原样保留
		// （放在普通 system 消息之后），计数与正文都不动。
		return fitBudget(system, summaries, priorCount, 0, priorBody, recent, budget), nil
	}
	// 有丢弃：旧摘要正文并入新摘要，计数累计（40 → 46，而不是只看本轮 6 条）。
	return fitBudget(system, nil, priorCount, dropped, priorBody, recent, budget), nil
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

// splitConversation 把输入切成三段：普通 system 消息、此前的压缩摘要（原样保留时
// 要用它）、以及可参与压缩的其余消息。
//
// 旧摘要**不再被无条件丢弃**：压缩器要么原样保留它，要么把它的正文与计数并入
// 新摘要。丢弃它就等于把上一轮的语义内容从上下文里抹掉。
func splitConversation(messages []rig.Message) (system, summaries, rest []rig.Message) {
	system = make([]rig.Message, 0, len(messages))
	rest = make([]rig.Message, 0, len(messages))
	for _, message := range messages {
		if IsCompactionSummary(message) {
			summaries = append(summaries, message)
			continue
		}
		if message.Role == rig.RoleSystem {
			system = append(system, message)
			continue
		}
		rest = append(rest, message)
	}
	return system, summaries, rest
}

// splitSummary 解析一条压缩摘要消息，返回它累计的消息数与正文。
//
// 摘要内容形如 `Earlier conversation compacted (40 messages)`，有正文时是
// `前缀帧` + 空行 + 正文（见 CompactionSummaryContent）。ok 为 false 表示内容
// 不带该前缀（手工构造的摘要），调用方应按"没有可信计数"处理。
func splitSummary(message rig.Message) (count int, body string, ok bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(message.Text()), SummaryPrefix)
	if !found {
		return 0, "", false
	}
	number, after, found := strings.Cut(rest, " messages)")
	if !found {
		return 0, "", false
	}
	count, err := strconv.Atoi(strings.TrimSpace(number))
	if err != nil || count < 0 {
		return 0, "", false
	}
	return count, strings.TrimSpace(after), true
}

// summarizePrior 把此前所有压缩摘要汇总成"累计消息数 + 合并后的正文"。
//
// 计数取各条之和（正常情况只有一条，因为压缩会替换而不是累加摘要消息）；
// 正文按出现顺序用空行连接，因此后续压缩不会丢失更早的语义内容。
func summarizePrior(summaries []rig.Message) (count int, body string) {
	bodies := make([]string, 0, len(summaries))
	for _, message := range summaries {
		parsedCount, parsedBody, ok := splitSummary(message)
		if !ok {
			// 前缀不完整的手工摘要：正文原样保留，计数不参与累计。
			if text := strings.TrimSpace(message.Text()); text != "" {
				bodies = append(bodies, text)
			}
			continue
		}
		count += parsedCount
		if parsedBody != "" {
			bodies = append(bodies, parsedBody)
		}
	}
	return count, strings.Join(bodies, "\n\n")
}

// mergeSummaryBody 合并旧摘要正文与本轮新生成的正文，跳过空串。
func mergeSummaryBody(previous, next string) string {
	previous = strings.TrimSpace(previous)
	next = strings.TrimSpace(next)
	switch {
	case previous == "":
		return next
	case next == "":
		return previous
	default:
		return previous + "\n\n" + next
	}
}

// summaryContent 渲染带计数帧的摘要内容。它不做 CompactionSummaryContent 的
// "已有前缀就原样返回"判断：这里的 body 已经从旧摘要里剥掉过帧，再命中前缀只会
// 丢掉累计后的计数。
func summaryContent(count int, body string) string {
	frame := fmt.Sprintf("%s%d messages)", SummaryPrefix, count)
	body = strings.TrimSpace(body)
	if body == "" {
		return frame
	}
	return frame + "\n\n" + body
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

// fitBudget 把"system + 摘要 + recent"压进 budget。
//
// 摘要有两种形态：
//   - dropped == 0：没有新摘要要写，但 prior（此前的压缩摘要，原样）必须保留在
//     普通 system 消息之后——否则旧摘要正文会在"窗口没超但整体超预算"时凭空消失；
//   - dropped > 0：写一条新摘要，计数为 priorCount+dropped（累计，而不是只算本轮），
//     正文为 body（已包含旧摘要正文）。
//
// 三轮收紧，按"破坏性从小到大"：
//  1. 从窗口头部**整块**丢弃：每次前进到下一个非 tool 消息，因此永远不会把
//     assistant tool_calls 与它的 tool 结果拆开（孤立 tool 消息会让 provider
//     直接 400，这是最贵的一种失败）；
//  2. 仍超预算时，按"从旧到新"的顺序截断消息正文（保留头部并加
//     BudgetTruncatedMarker）；最后一条消息（当前用户输入）不截；
//  3. 再超就原样返回：预算只是估算，真正的裁判是 provider，硬截会把用户的
//     当前请求改坏。
//
// 第一轮让 dropped 从 0 变成正数时，prior 会被并入新摘要（计数累计、正文保留），
// 因此任何时刻最多只有一条摘要消息。
//
// budget <= 0 表示不限（历史行为）。返回的切片是新切片，不修改入参。
func fitBudget(system, prior []rig.Message, priorCount, dropped int, body string, recent []rig.Message, budget int) []rig.Message {
	build := func(recent []rig.Message, dropped int) []rig.Message {
		out := make([]rig.Message, 0, len(system)+1+len(recent))
		out = append(out, CloneMessages(system)...)
		switch {
		case dropped > 0:
			out = append(out, rig.NewMessage(rig.RoleSystem, summaryContent(priorCount+dropped, body)))
		case len(prior) > 0:
			out = append(out, CloneMessages(prior)...)
		}
		out = append(out, recent...)
		return out
	}
	out := build(recent, dropped)
	if budget <= 0 {
		return out
	}
	// 第一轮：整块丢头部。
	for len(recent) > 1 && rig.EstimateTokens(out) > budget {
		next := 1
		for next < len(recent) && recent[next].Role == rig.RoleTool {
			next++
		}
		if next >= len(recent) {
			break // 只剩一个 tool 块：不能拆，交给第二/三轮
		}
		recent = recent[next:]
		dropped += next
		out = build(recent, dropped)
	}
	// 第二轮：从旧到新截断正文（system / 摘要 / 最后一条消息除外）。
	if rig.EstimateTokens(out) > budget {
		protected := len(system)
		if dropped > 0 {
			protected++
		}
		shrinkMessages(out, budget, protected)
	}
	return out
}

// shrinkMessages 按从旧到新的顺序截断消息正文，直到估算 token 落回 budget。
//
// protected 是前部不能动的消息数（system 块 + 摘要）；最后一条消息永远不截——
// 那是用户当前的问题，改坏它等于改坏需求。
func shrinkMessages(messages []rig.Message, budget, protected int) {
	for index := protected; index < len(messages)-1; index++ {
		over := rig.EstimateTokens(messages) - budget
		if over <= 0 {
			return
		}
		text := messages[index].Text()
		if text == "" {
			continue
		}
		tokens := rig.EstimateTextTokens(text)
		if messages[index].Role == rig.RoleUser && tokens <= budget {
			// 用户消息是需求本身：除非它一个人就撑爆预算，否则不改写它。
			continue
		}
		keep := tokens - over
		if keep < 0 {
			keep = 0
		}
		messages[index].Content = []rig.ContentPart{rig.TextPart(truncateToTokens(text, keep))}
	}
}

// truncateToTokens 把 text 截到"估算 token <= keep"，按 rune 边界回退，并追加
// BudgetTruncatedMarker。keep <= 0 时只留标记。
func truncateToTokens(text string, keep int) string {
	if keep <= 0 {
		return strings.TrimSpace(BudgetTruncatedMarker)
	}
	if rig.EstimateTextTokens(text) <= keep {
		return text
	}
	runes := []rune(text)
	cut := len(runes)
	for cut > 0 && rig.EstimateTextTokens(string(runes[:cut])) > keep {
		// 按 1/8 步长回退，避免对长文本做 O(n) 次全量估算。
		step := cut / 8
		if step < 1 {
			step = 1
		}
		cut -= step
	}
	// 回退可能多切了一些，向前补回能放下的部分。
	for cut < len(runes) && rig.EstimateTextTokens(string(runes[:cut+1])) <= keep {
		cut++
	}
	if cut <= 0 {
		return strings.TrimSpace(BudgetTruncatedMarker)
	}
	return strings.TrimRight(string(runes[:cut]), "\n") + BudgetTruncatedMarker
}
