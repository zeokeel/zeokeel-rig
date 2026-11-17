package local

// 本文件是 SSE 流解码：把 {BaseURL}{ChatPath} 的流式响应变成 rig.Chunk 序列。
//
// 与 provider/openai 的差别都在"容忍度"上（见 Quirks）：本地服务可能不发 [DONE]、
// 可能把 tool_calls 的 arguments 发成对象、可能用 function_call 旧字段、也可能把
// content 发成分片数组。这些差异在这里被吸收，agent 侧看到的永远是干净的 rig.Chunk。

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
)

const (
	// maxSSELine 限制单条 SSE 行；工具调用参数片段可能很大。
	maxSSELine = 4 << 20
	// doneData 是 OpenAI 流式 API 的终止载荷。
	doneData = "[DONE]"
)

// stream 是 rig.Stream 的实现。
type stream struct {
	ctx     context.Context
	body    io.ReadCloser
	scanner *bufio.Scanner
	quirks  quirks

	pending      []rig.Chunk
	finishReason string
	streamDone   bool
	closed       bool

	sawFinishReason bool
	sawTerminator   bool
}

var _ rig.Stream = (*stream)(nil)

// newStream 包装已建立的流式响应（所有权转移给返回的 stream）。
func newStream(ctx context.Context, body io.ReadCloser, q quirks) *stream {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxSSELine)
	return &stream{ctx: ctx, body: body, scanner: scanner, quirks: q}
}

// Recv 实现 rig.Stream。
func (s *stream) Recv() (rig.Chunk, error) {
	for {
		if err := s.ctx.Err(); err != nil {
			s.close()
			return rig.Chunk{}, canceledError("local", err)
		}
		if len(s.pending) > 0 {
			chunk := s.pending[0]
			s.pending = s.pending[1:]
			return chunk, nil
		}
		if s.streamDone {
			if s.finishReason != "" {
				chunk := rig.Chunk{FinishReason: s.finishReason}
				s.finishReason = ""
				return chunk, nil
			}
			s.close()
			return rig.Chunk{}, io.EOF
		}
		if !s.scanner.Scan() {
			if err := s.scanner.Err(); err != nil {
				s.close()
				if ctxErr := s.ctx.Err(); ctxErr != nil {
					return rig.Chunk{}, canceledError("local", ctxErr)
				}
				return rig.Chunk{}, fmt.Errorf("local: read stream: %w", err)
			}
			// 本地服务常常直接关连接而不发 [DONE]：默认容忍（Quirks.RequireDone
			// 打开时才把"既无 finish_reason 也无 [DONE]"当成截断）。
			if s.quirks.requireDone && !s.sawFinishReason && !s.sawTerminator {
				s.close()
				return rig.Chunk{}, fmt.Errorf("%w: the connection closed before finish_reason or [DONE]; the response may be truncated", ErrIncompleteStream)
			}
			s.streamDone = true
			continue
		}
		payload, ok := sseData(s.scanner.Text())
		if !ok {
			continue
		}
		if payload == doneData {
			s.sawTerminator = true
			s.streamDone = true
			continue
		}
		if err := s.handleEvent(payload); err != nil {
			s.close()
			return rig.Chunk{}, err
		}
	}
}

// Close 释放响应体（Recv 在 EOF/错误时也会调用）。
func (s *stream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.body.Close()
}

func (s *stream) close() { _ = s.Close() }

// sseData 提取一条 SSE `data:` 行的载荷；空行、注释、非 data 字段与损坏行返回 false，
// 因此一个坏事件永远不会中断整个响应。
func sseData(line string) (string, bool) {
	line = strings.TrimSuffix(line, "\r")
	if line == "" || strings.HasPrefix(line, ":") {
		return "", false
	}
	field, value, found := strings.Cut(line, ":")
	if !found || field != "data" {
		return "", false
	}
	value = strings.TrimPrefix(value, " ")
	if strings.TrimSpace(value) == "" {
		return "", false
	}
	return value, true
}

// handleEvent 解码一个 data 载荷并把分片排队。坏 JSON 会被跳过（容忍），带内 error
// 对象会变成 Recv 的错误（本地服务用它在流中报错）。
func (s *stream) handleEvent(payload string) error {
	var event streamEvent
	if err := unmarshalEvent(payload, &event); err != nil {
		return nil
	}
	if event.Error != nil {
		return fmt.Errorf("local: stream error: %s", event.Error.text())
	}
	for _, choice := range event.Choices {
		delta := choice.Delta
		if delta.isEmpty() && choice.Message != nil {
			// 少数服务在流里发 message 而不是 delta（把整条消息放在最后一个事件）。
			delta = *choice.Message
		}
		chunk := rig.Chunk{
			Delta:     delta.Content.text(),
			Reasoning: s.reasoningOf(delta),
		}
		for position, call := range delta.ToolCalls {
			// index 缺失时按出现顺序补齐（见 wire.go 的说明）。
			index := position
			if call.Index != nil {
				index = *call.Index
			}
			chunk.ToolCalls = append(chunk.ToolCalls, rig.ToolCallDelta{
				Index:     index,
				ID:        call.ID,
				Name:      call.Function.Name,
				Arguments: call.Function.Arguments.value,
			})
		}
		// 旧式 function_call（单数、没有 index）：当成 index 0 的调用。
		if call := delta.FunctionCall; call != nil {
			chunk.ToolCalls = append(chunk.ToolCalls, rig.ToolCallDelta{
				Index:     0,
				ID:        call.ID,
				Name:      call.Name,
				Arguments: call.Arguments.value,
			})
		}
		if chunk.Delta != "" || chunk.Reasoning != "" || len(chunk.ToolCalls) > 0 {
			s.pending = append(s.pending, chunk)
		}
		if choice.FinishReason != nil && strings.TrimSpace(*choice.FinishReason) != "" {
			s.finishReason = s.normalizeFinishReason(*choice.FinishReason)
			s.sawFinishReason = true
		}
	}
	if event.Usage != nil {
		s.pending = append(s.pending, rig.Chunk{Usage: usageFromWire(event.Usage)})
	}
	return nil
}

// reasoningOf 依次尝试 reasoning_content / reasoning / 配置的额外键名。
func (s *stream) reasoningOf(delta streamDelta) string {
	if text := delta.ReasoningContent.text(); text != "" {
		return text
	}
	if text := delta.Reasoning.text(); text != "" {
		return text
	}
	for _, key := range s.quirks.reasoningKeys {
		if text := delta.Extra[key].text(); text != "" {
			return text
		}
	}
	return ""
}

// normalizeFinishReason 把服务端变体归一化成 stop / length / tool_calls。
func (s *stream) normalizeFinishReason(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if alias, ok := s.quirks.finishReasonAliases[normalized]; ok && alias != "" {
		return alias
	}
	return normalized
}

// usageFromWire 把用量对象转成 rig.Usage（只承载四类 token）。
func usageFromWire(usage *streamUsage) *rig.Usage {
	if usage == nil {
		return nil
	}
	out := &rig.Usage{
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		TotalTokens:      usage.TotalTokens,
	}
	switch {
	case usage.PromptTokensDetails != nil:
		out.CachedTokens = usage.PromptTokensDetails.CachedTokens
	case usage.PromptCacheHitTokens > 0:
		out.CachedTokens = usage.PromptCacheHitTokens
	}
	if out.TotalTokens == 0 {
		out.TotalTokens = out.PromptTokens + out.CompletionTokens
	}
	return out
}
