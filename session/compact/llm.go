package compact

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

// LLM 摘要压缩默认值。
const (
	DefaultToolResultKeepLast = 4
	DefaultLLMSummaryChars    = 4000
	DefaultLLMSummaryTimeout  = 20 * time.Second
	// LLMCompactorSystemPrompt 是摘要请求的 system 指令；导出以便测试 fake
	// provider 识别压缩请求。
	LLMCompactorSystemPrompt = "You are a conversation summarizer. Summarize the provided conversation so another assistant can continue it. " +
		"Preserve decisions, user preferences, file paths, tool outcomes, unresolved questions and the current task. " +
		"Reply with the summary text only, without preamble."
)

// LLMCompactor 用配置的 Provider 写摘要，再保留最近 KeepLast 条非 system 消息。
// 摘要失败（provider 错误、空答案、超时）时回退到 Fallback；Fallback 为 nil 时
// 使用 WindowCompactor。调用方 context 被取消时直接返回错误，不回退。
type LLMCompactor struct {
	Provider           rig.Provider
	Model              string
	Timeout            time.Duration
	KeepLast           int
	ToolResultKeepLast int
	MaxSummaryChars    int
	Fallback           rig.Compactor
}

var _ rig.Compactor = LLMCompactor{}

// Compact 实现 rig.Compactor。
func (c LLMCompactor) Compact(ctx context.Context, sessionID string, messages []rig.Message, budget int) ([]rig.Message, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
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
	if len(rest) <= keepLast {
		// 不需要摘要；但整段对话超预算时仍要收紧（先裁工具结果，再截正文）。
		// 旧摘要原样保留：没有新摘要可写不等于可以把它丢掉。
		recent := CloneMessages(rest)
		if c.ToolResultKeepLast >= 0 {
			recent = pruneToolResults(recent, c.toolResultKeepLast())
		}
		return fitBudget(system, summaries, priorCount, 0, priorBody, recent, budget), nil
	}
	dropped := boundary(rest, keepLast)
	if dropped == 0 {
		return CloneMessages(messages), nil
	}
	summarizeCtx := ctx
	timeout := c.Timeout
	if timeout == 0 {
		timeout = DefaultLLMSummaryTimeout
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		summarizeCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	summary, err := c.summarize(summarizeCtx, sessionID, rest[:dropped], summaries)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return c.fallback().Compact(ctx, sessionID, CloneMessages(messages), budget)
	}
	summary = c.truncateSummary(summary)
	if summary == "" {
		return c.fallback().Compact(ctx, sessionID, CloneMessages(messages), budget)
	}
	// 旧摘要正文并入新摘要：摘要链只保留一条消息，但语义内容不能断。
	mergedBody := mergeSummaryBody(priorBody, summary)
	// 窗口先按配置裁剪工具结果，再逐级降档（4 → 2 → 全部）直到放进预算：
	// 工具结果通常是窗口里最占地方的部分，而且它的"配对"必须保留，所以只能
	// 替换内容、不能删消息。
	recentBase := CloneMessages(rest[dropped:])
	ladder := []func([]rig.Message) []rig.Message{
		func(messages []rig.Message) []rig.Message {
			if c.ToolResultKeepLast < 0 {
				return messages
			}
			return pruneToolResults(messages, c.toolResultKeepLast())
		},
		func(messages []rig.Message) []rig.Message { return pruneToolResults(messages, 2) },
		func(messages []rig.Message) []rig.Message { return pruneAllToolResults(messages) },
	}
	var out []rig.Message
	for _, apply := range ladder {
		out = fitBudget(system, nil, priorCount, dropped, mergedBody, apply(CloneMessages(recentBase)), budget)
		if budget <= 0 || rig.EstimateTokens(out) <= budget {
			return out, nil
		}
	}
	// 连工具结果全裁掉都放不下：交给 provider 报上下文超限（预算只是估算）。
	return out, nil
}

// summarize 让模型为 old 写摘要。prior 是此前的压缩摘要（可能为空），它被原样
// 前置到请求里：摘要必须覆盖"此前已压缩的内容 + 本轮要丢弃的内容"，否则第二次
// 压缩就会把上一轮的语义内容整段丢给模型看不见。
func (c LLMCompactor) summarize(ctx context.Context, sessionID string, old, prior []rig.Message) (string, error) {
	if c.Provider == nil {
		return "", errors.New("rig: LLMCompactor has no provider")
	}
	request := make([]rig.Message, 0, len(old)+len(prior)+2)
	request = append(request, rig.NewMessage(rig.RoleSystem, LLMCompactorSystemPrompt))
	request = append(request, CloneMessages(prior)...)
	request = append(request, pruneToolResults(CloneMessages(old), 0)...)
	request = append(request, rig.NewMessage(rig.RoleUser,
		fmt.Sprintf("Summarize the conversation above in at most %d characters so it can replace that part of the context.", c.maxSummaryChars())))
	stream, err := c.Provider.Stream(ctx, rig.Request{
		Model:    c.Model,
		Messages: request,
	})
	if err != nil {
		return "", err
	}
	if stream == nil {
		return "", errors.New("rig: LLMCompactor provider returned no stream")
	}
	var builder strings.Builder
	for {
		chunk, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", err
		}
		if chunk.Delta != "" {
			builder.WriteString(chunk.Delta)
		}
	}
	return builder.String(), nil
}

func (c LLMCompactor) fallback() rig.Compactor {
	if c.Fallback != nil {
		return c.Fallback
	}
	return WindowCompactor{KeepLast: c.KeepLast}
}

func (c LLMCompactor) toolResultKeepLast() int {
	if c.ToolResultKeepLast <= 0 {
		return DefaultToolResultKeepLast
	}
	return c.ToolResultKeepLast
}

func (c LLMCompactor) maxSummaryChars() int {
	if c.MaxSummaryChars <= 0 {
		return DefaultLLMSummaryChars
	}
	return c.MaxSummaryChars
}

func (c LLMCompactor) truncateSummary(summary string) string {
	summary = strings.TrimSpace(summary)
	runes := []rune(summary)
	if len(runes) > c.maxSummaryChars() {
		summary = string(runes[:c.maxSummaryChars()])
	}
	return summary
}

func pruneToolResults(messages []rig.Message, keepLast int) []rig.Message {
	return PruneToolResults(messages, keepLast)
}

// pruneAllToolResults 把窗口内所有工具结果替换成占位符；消息本身（角色、
// ToolCallID）保留，因此 assistant tool_calls 与 tool 结果的配对不变。
func pruneAllToolResults(messages []rig.Message) []rig.Message {
	out := CloneMessages(messages)
	for index := range out {
		if out[index].Role == rig.RoleTool {
			out[index].Content = []rig.ContentPart{rig.TextPart(ToolResultPrunedMarker)}
		}
	}
	return out
}
