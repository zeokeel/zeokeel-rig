package openai

// Fuzz targets for the streaming decoder. The OpenAI-compatible SSE format is
// attacker-influenced input (a gateway, proxy, or compromised endpoint controls
// it), so arbitrary bytes must never panic the provider: malformed events are
// skipped or reported as errors, never a crash.
//
// Both targets are deterministic: they only decode in-memory bytes, with no
// network, clock or randomness. Inputs are capped so one execution stays
// bounded, and the decoder itself enforces maxSSELine.

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// fuzzMaxInput bounds one fuzz input. A single SSE event never legitimately
// needs more than this; truncating keeps each execution's memory and runtime
// bounded while still exercising the decoder's own 4 MiB line limit.
const fuzzMaxInput = 1 << 20

// FuzzParseSSE exercises the SSE line classifier (sseData) and the per-event
// JSON decoder (handleEvent) directly on arbitrary lines. A malformed data line
// must be ignored or reported, and a decoded event must only queue chunks that
// the input actually encoded.
func FuzzParseSSE(f *testing.F) {
	seeds := []string{
		"",
		": keep-alive comment",
		"event: message",
		"data:",
		"data: ",
		"data: [DONE]",
		`data: {"choices":[{"delta":{"content":"hello"}}]}`,
		`data: {"choices":[{"delta":{"content":"a","reasoning_content":"because"},"finish_reason":"stop"}]}`,
		`data:{"choices":[{"delta":{"content":"no-space-after-colon"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a\"}"}}]}}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8,"prompt_tokens_details":{"cached_tokens":2}}}`,
		`data: {"choices":[],"usage":{"prompt_tokens":1,"prompt_cache_hit_tokens":1}}`,
		`data: {"error":{"message":"boom","type":"server_error","code":500}}`,
		`data: {"error":{}}`,
		"data: {not json",
		"data: {\"choices\":[]}\r",
		"data: null",
		"data: []",
		"data: {\"choices\":[{}]}",
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}",
		"data:" + strings.Repeat("x", 4096),
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, line string) {
		if len(line) > fuzzMaxInput {
			line = line[:fuzzMaxInput]
		}
		payload, ok := sseData(line)
		if !ok || payload == doneData {
			return
		}
		stream := &sseStream{}
		if err := stream.handleEvent(payload); err != nil {
			return // an in-band error event is a valid outcome, not a panic
		}
		// One queued chunk needs at least several input bytes; this catches an
		// accidental unbounded fan-out from a tiny payload.
		if len(stream.pending) > len(payload)+1 {
			t.Fatalf("queued %d chunks from a %d-byte payload", len(stream.pending), len(payload))
		}
	})
}

// FuzzStreamDecode drives the complete stream decoder (newSSEStream + Recv) with
// arbitrary response bodies. Recv must always terminate with io.EOF or an error
// (a malformed stream is tolerated by design) and must never panic or spin.
func FuzzStreamDecode(f *testing.F) {
	seeds := [][]byte{
		nil,
		[]byte(""),
		[]byte("\n\n"),
		[]byte("data: [DONE]\n\n"),
		[]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"),
		[]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n"),
		[]byte("data: {\"choices\":[{\"delta\":{\"content\":\"a\"}},{\"delta\":{\"content\":\"b\"}}]}\n\ndata: [DONE]\n\n"),
		[]byte(": comment\n\nevent: ping\ndata: {\"choices\":[]}\n\ndata: [DONE]\n\n"),
		[]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"f\",\"arguments\":\"{\"}}]}}]}\n\ndata: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"a\\\":1}\"}}]}}]}\n\ndata: [DONE]\n\n"),
		[]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2,\"total_tokens\":3}}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"),
		[]byte("data: {\"error\":{\"message\":\"boom\",\"type\":\"server_error\"}}\n\n"),
		[]byte("data: {broken\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"),
		[]byte("data: {\"choices\":[{\"delta\":{\"content\":\"" + strings.Repeat("x", 1024) + "\"}}]}\n\ndata: [DONE]\n\n"),
		[]byte("data: [DONE]"),
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > fuzzMaxInput {
			data = data[:fuzzMaxInput]
		}
		response := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(bytes.NewReader(data)),
		}
		stream := newSSEStream(context.Background(), response, false)
		// Every non-EOF Recv result consumes at least one input byte (a pending
		// chunk or a finish reason), so this bound is a real termination check,
		// not an arbitrary iteration cap.
		maxRecv := len(data) + 8
		for i := 0; i <= maxRecv; i++ {
			_, err := stream.Recv()
			if err != nil {
				return // io.EOF or a tolerated stream error ends the stream
			}
		}
		t.Fatalf("stream produced more than %d chunks for %d input bytes", maxRecv, len(data))
	})
}
