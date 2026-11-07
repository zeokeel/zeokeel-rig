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
	system, rest := splitConversation(messages)
	if len(rest) <= keepLast {
		return CloneMessages(messages), nil
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
	summary, err := c.summarize(summarizeCtx, sessionID, rest[:dropped])
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
	recent := CloneMessages(rest[dropped:])
	if c.ToolResultKeepLast >= 0 {
		recent = pruneToolResults(recent, c.toolResultKeepLast())
	}
	out := make([]rig.Message, 0, len(system)+1+len(recent))
	out = append(out, system...)
	out = append(out, SummaryMessage(dropped, summary))
	out = append(out, recent...)
	return out, nil
}

func (c LLMCompactor) summarize(ctx context.Context, sessionID string, old []rig.Message) (string, error) {
	if c.Provider == nil {
		return "", errors.New("rig: LLMCompactor has no provider")
	}
	request := make([]rig.Message, 0, len(old)+2)
	request = append(request, rig.NewMessage(rig.RoleSystem, LLMCompactorSystemPrompt))
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
