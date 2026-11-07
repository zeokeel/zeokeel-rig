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
	// maxSSELine bounds one SSE line. Tool-call argument fragments can be
	// large, so this is far above the bufio.Scanner default.
	maxSSELine = 4 << 20
	// doneData is the terminal payload defined by the OpenAI streaming API.
	doneData = "[DONE]"
)

// sseStream decodes one SSE response into rig.Chunk values.
//
// Chunk ordering: content, reasoning and tool-call deltas are emitted as soon
// as their SSE event arrives; usage is reported before the finish reason; the
// finish reason is the last data chunk before io.EOF.
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

// newSSEStream wraps an established streaming response. The caller transfers
// ownership of resp.Body to the stream.
func newSSEStream(ctx context.Context, resp *http.Response) *sseStream {
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxSSELine)
	return &sseStream{
		ctx:     ctx,
		body:    resp.Body,
		scanner: scanner,
	}
}

// Recv implements rig.Stream. It returns io.EOF after the final chunk and
// releases the response body on io.EOF or error.
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
			// A clean EOF without a [DONE] marker is the end of the stream:
			// some OpenAI-compatible servers close the connection instead of
			// sending the terminal event.
			s.streamDone = true
			continue
		}
		payload, ok := sseData(s.scanner.Text())
		if !ok {
			continue // blank lines, comments, non-data fields and malformed lines
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

// Close releases the response body. Recv calls it on io.EOF/error; a consumer
// that abandons the stream early may call it directly.
func (s *sseStream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.body.Close()
}

// close implements Close without returning the (uninteresting) error.
func (s *sseStream) close() { _ = s.Close() }

// sseData extracts the payload of one SSE `data:` line. It reports false for
// blank lines, comment lines, non-data fields and malformed lines so a broken
// event never aborts a whole response.
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

// handleEvent decodes one data payload and queues its chunks. A malformed JSON
// payload is tolerated (the event is skipped): one bad event must not abort the
// response.
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

// usageFromWire translates the OpenAI/DeepSeek usage object into rig.Usage.
// Rig only carries prompt/completion/total/cached tokens.
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

// firstNonEmpty returns the first non-empty string, letting the provider accept
// both the reasoning_content (DeepSeek/OpenAI) and reasoning (gateway)
// spellings.
func firstNonEmpty(values ...string) string {
	return cmp.Or(values...)
}
