package openai

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
)

const (
	// maxSSELine 限制单条 SSE 行的长度。工具调用参数片段可能很大，因此该上限
	// 远高于 bufio.Scanner 的默认值。
	maxSSELine = 4 << 20
	// doneData 是 OpenAI 流式 API 定义的终止载荷。
	doneData = "[DONE]"
)

// sseStream 把一个 SSE 响应解码为 rig.Chunk 序列。
//
// 分片顺序：正文、推理与工具调用片段在其 SSE 事件到达时立即发出；用量在结束原因
// 之前上报；结束原因是 io.EOF 之前的最后一个数据分片。
type sseStream struct {
	ctx     context.Context
	body    io.ReadCloser
	scanner *bufio.Scanner

	pending      []rig.Chunk
	finishReason string
	streamDone   bool
	eofSent      bool
	closed       bool
}

var _ rig.Stream = (*sseStream)(nil)

// newSSEStream 包装一个已建立的流式响应。调用方把 resp.Body 的所有权转移给
// 该流。
func newSSEStream(ctx context.Context, resp *http.Response) *sseStream {
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxSSELine)
	return &sseStream{
		ctx:     ctx,
		body:    resp.Body,
		scanner: scanner,
	}
}

// Recv 实现 rig.Stream。它在最后一个分片之后返回 io.EOF，并在 io.EOF 或出错时
// 释放响应体。
func (s *sseStream) Recv() (rig.Chunk, error) {
	for {
		if err := s.ctx.Err(); err != nil {
			s.close()
			return rig.Chunk{}, canceledError(err)
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
			if s.eofSent {
				s.close()
				return rig.Chunk{}, io.EOF
			}
			s.eofSent = true
			s.close()
			return rig.Chunk{}, io.EOF
		}
		if !s.scanner.Scan() {
			if err := s.scanner.Err(); err != nil {
				s.close()
				if ctxErr := s.ctx.Err(); ctxErr != nil {
					return rig.Chunk{}, canceledError(ctxErr)
				}
				return rig.Chunk{}, fmt.Errorf("openai: read stream: %w", err)
			}
			// 没有 [DONE] 标记的干净 EOF 也视为流结束：部分 OpenAI 兼容服务器
			// 会直接关闭连接，而不是发送终止事件。
			s.streamDone = true
			continue
		}
		payload, ok := sseData(s.scanner.Text())
		if !ok {
			continue // 空行、注释、非 data 字段与格式错误的行
		}
		if payload == doneData {
			s.streamDone = true
			continue
		}
		if err := s.handleEvent(payload); err != nil {
			s.close()
			return rig.Chunk{}, err
		}
	}
}

// Close 释放响应体。Recv 会在 io.EOF/出错时调用它；提前放弃该流的消费者也可以
// 直接调用它。
func (s *sseStream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.body.Close()
}

// close 是忽略（无意义的）返回错误的 Close。
func (s *sseStream) close() { _ = s.Close() }

// sseData 提取一条 SSE `data:` 行的载荷。对于空行、注释行、非 data 字段以及
// 格式错误的行返回 false，从而保证一个坏事件永远不会中断整个响应。
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
	if value == "" {
		return "", false
	}
	return value, true
}

// handleEvent 解码一个 data 载荷并把其中的分片排队。格式错误的 JSON 载荷会被
// 容忍（跳过该事件）：一个坏事件不能中断整个响应。
func (s *sseStream) handleEvent(payload string) error {
	var event streamEvent
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return nil
	}
	if event.Error != nil {
		return fmt.Errorf("openai: stream error: %s", event.Error.text())
	}
	for _, choice := range event.Choices {
		delta := choice.Delta
		chunk := rig.Chunk{
			Delta:     delta.Content,
			Reasoning: firstNonEmpty(delta.ReasoningContent, delta.Reasoning),
		}
		for _, call := range delta.ToolCalls {
			chunk.ToolCalls = append(chunk.ToolCalls, rig.ToolCallDelta{
				Index:     call.Index,
				ID:        call.ID,
				Name:      call.Function.Name,
				Arguments: call.Function.Arguments,
			})
		}
		if chunk.Delta != "" || chunk.Reasoning != "" || len(chunk.ToolCalls) > 0 {
			s.pending = append(s.pending, chunk)
		}
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			s.finishReason = *choice.FinishReason
		}
	}
	if event.Usage != nil {
		s.pending = append(s.pending, rig.Chunk{Usage: usageFromWire(event.Usage)})
	}
	return nil
}

// usageFromWire 把 OpenAI/DeepSeek 的用量对象转换为 rig.Usage。Rig 只承载
// prompt/completion/total/cached 四类 token。
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
	return out
}

// firstNonEmpty 返回第一个非空字符串，使 provider 同时接受 reasoning_content
// （DeepSeek/OpenAI）与 reasoning（网关）两种拼写。
func firstNonEmpty(values ...string) string {
	return cmp.Or(values...)
}
