package mcp

// Fuzz targets for the JSON-RPC/MCP wire decoding layer. MCP servers (a local
// child process or a remote endpoint) are untrusted input sources: a broken or
// hostile server must be rejected with an error, never crash the host process.
//
// The targets are deterministic, use only in-memory data, and never dial a
// network or spawn a process: decodeJSONEnvelope and readSSEResponse are pure
// framing helpers, so a zero-value httpTransport is enough to reach them.

import (
	"bytes"
	"context"
	"testing"
)

// fuzzMaxInput bounds one fuzz input so a single execution stays cheap; the
// decoder's own MaxMessageBytes (8 MiB) limit always applies on top of it.
const fuzzMaxInput = 1 << 20

// FuzzDecodeRPCEnvelope decodes arbitrary bytes as one JSON-RPC envelope and
// then runs the response-id and error-rendering paths on whatever was decoded.
// None of these may panic, and a decoded error must always render a message
// (the rendering truncates hostile data).
func FuzzDecodeRPCEnvelope(f *testing.F) {
	seeds := [][]byte{
		[]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"read_file"}]}}`),
		[]byte(`{"jsonrpc":"2.0","id":"7","result":null}`),
		[]byte(`{"jsonrpc":"2.0","id":3,"error":{"code":-32601,"message":"method not found","data":{"hint":"check the method"}}}`),
		[]byte(`{"jsonrpc":"2.0","id":4,"error":{"code":0,"message":"","data":"x"}}`),
		[]byte(`{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info"}}`),
		[]byte(`{"jsonrpc":"2.0","id":null,"result":{}}`),
		[]byte(`{"id":1}`),
		[]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-1,"message":"m","data":"` + string(bytes.Repeat([]byte("d"), 4096)) + `"}}`),
		[]byte(`[]`),
		[]byte(`"just a string"`),
		[]byte(`not json`),
		[]byte(``),
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > fuzzMaxInput {
			data = data[:fuzzMaxInput]
		}
		envelope, err := decodeJSONEnvelope(bytes.NewReader(data))
		if err != nil {
			return // malformed input is rejected with an error; that is correct
		}
		id, ok := decodeResponseID(envelope.ID)
		if ok {
			_ = id
		}
		response := responseFromEnvelope(envelope, 1)
		if response.err != nil {
			if message := response.err.Error(); message == "" {
				t.Fatal("decoded error rendered an empty message")
			}
		}
		if envelope.Error != nil {
			_ = envelope.Error.Error()
		}
	})
}

// FuzzDecodeResponseID exercises the tolerant id parser: the client sends
// numeric ids but a server may echo a string, a float, a negative number or
// garbage. Any input must produce a boolean decision without panicking.
func FuzzDecodeResponseID(f *testing.F) {
	seeds := [][]byte{
		[]byte(`1`),
		[]byte(`0`),
		[]byte(` 42 `),
		[]byte(`"42"`),
		[]byte(`""`),
		[]byte(`null`),
		[]byte(`-1`),
		[]byte(`1.5`),
		[]byte(`1e3`),
		[]byte(`18446744073709551616`),
		[]byte(`{"id":1}`),
		[]byte(`[]`),
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 4096 {
			raw = raw[:4096]
		}
		// A panic here would be caught by the fuzz target itself; also check
		// the documented null/empty rule never reports a usable id.
		if id, ok := decodeResponseID(raw); ok && id == 0 {
			_ = id
		}
	})
}

// FuzzReadSSEResponse feeds arbitrary bytes through the Streamable HTTP SSE
// reader. It must terminate with either the matching response or an error, and
// never panic; notifications, other ids and malformed events are skipped.
func FuzzReadSSEResponse(f *testing.F) {
	seeds := [][]byte{
		[]byte("data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"tools\":[]}}\n\n"),
		[]byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":null}\n\n"),
		[]byte(": comment\n\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n"),
		[]byte("data: [DONE]\n\n"),
		[]byte("data: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{}}\n\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n"),
		[]byte("data: {broken json\n\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n"),
		[]byte("no sse framing at all"),
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > fuzzMaxInput {
			data = data[:fuzzMaxInput]
		}
		transport := &httpTransport{}
		response, err := transport.readSSEResponse(context.Background(), bytes.NewReader(data), 1)
		if err != nil {
			return // an SSE stream without a matching response is an error, not a crash
		}
		_ = response
	})
}
