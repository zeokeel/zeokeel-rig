package lsp

// Fuzz targets for the LSP wire layer: the Content-Length framing reader and the
// result parsers. A language server is an untrusted child process, so a broken
// or hostile stream must come back as an error, never a panic, and a decoded
// message must be re-encodable.
//
// The targets are deterministic and in-memory only: no subprocess, no network.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"testing"
)

// fuzzMaxInput bounds one fuzz input so a single execution stays cheap. The
// reader's own maxMessageBytes (16 MiB) cap always applies on top of this.
const fuzzMaxInput = 1 << 20

// FuzzReadMessage parses arbitrary bytes as a Content-Length framed JSON-RPC
// message. Invalid headers and bodies must be rejected with an error; anything
// that decodes must survive a re-encode without panicking.
func FuzzReadMessage(f *testing.F) {
	seeds := [][]byte{
		[]byte("Content-Length: 2\r\n\r\n{}"),
		[]byte("Content-Length: 2\n\n{}"),
		[]byte("Content-Length: 0\r\n\r\n"),
		[]byte("Content-Length: 5\r\n\r\n{\"id\""),
		[]byte("Content-Length: 999999\r\n\r\n{}"),
		[]byte("Content-Type: application/vscode-jsonrpc; charset=utf-8\r\nContent-Length: 43\r\n\r\n{\"jsonrpc\":\"2.0\",\"method\":\"initialize\"}"),
		[]byte("Content-Length: 4\r\nContent-Length: 5\r\n\r\nnull"),
		[]byte("garbage\r\n\r\n"),
		[]byte("Content-Length: -1\r\n\r\n"),
		[]byte("Content-Length: 4\r\n\r\nnull"),
		[]byte(""),
		[]byte("\r\n"),
		[]byte("Content-Length: 12\r\n\r\n{\"jsonrpc\":\"2.0\",\"method\":\"textDocument/didOpen\",\"params\":{}}"),
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > fuzzMaxInput {
			data = data[:fuzzMaxInput]
		}
		reader := bufio.NewReader(bytes.NewReader(data))
		message, err := readMessage(reader)
		if err != nil {
			return // any rejection is a valid, safe outcome
		}
		var encoded bytes.Buffer
		if err := writeMessage(&encoded, message); err != nil {
			// writeMessage only fails on a nil writer or unencodable raw JSON;
			// a decoded wireMessage must round-trip through re-encoding.
			t.Fatalf("decoded message could not be re-encoded: %v", err)
		}
	})
}

// FuzzParseLSPResults runs arbitrary JSON through the hover, location, symbol
// and diagnostics parsers. These consume server-controlled result payloads and
// must degrade to an error for bad shapes instead of panicking.
func FuzzParseLSPResults(f *testing.F) {
	seeds := []string{
		`{"contents":"hover text","range":{"start":{"line":0,"character":1},"end":{"line":0,"character":2}}}`,
		`{"contents":{"kind":"markdown","value":"**bold**"}}`,
		`{"contents":{"kind":"","value":""}}`,
		`{"contents":["plain",{"language":"go","value":"func main() {}"}]}`,
		`{"contents":[],"range":{"start":{"line":1,"character":0},"end":{"line":1,"character":4}}}`,
		`null`,
		`{"uri":"file:///tmp/a.go","range":{"start":{"line":1,"character":0},"end":{"line":1,"character":4}}}`,
		`[{"uri":"file:///a.go","range":{}},{"targetUri":"file:///b.go","targetSelectionRange":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}}}]`,
		`{"targetUri":"file:///c.go","targetRange":{"start":{"line":2,"character":2},"end":{"line":2,"character":3}}}`,
		`[{"name":"main","kind":12,"range":{},"selectionRange":{}},{"name":"x","kind":1,"location":{"range":{}}}]`,
		`[{"name":"nested","kind":2,"range":{},"selectionRange":{},"children":[{"name":"child","kind":3,"range":{},"selectionRange":{}}]}]`,
		`{"uri":"file:///a.go","diagnostics":[{"range":{},"severity":1,"message":"boom","code":"E1","source":"gopls"}]}`,
		`{"uri":"file:///a.go","diagnostics":[]}`,
		`{}`,
		`[]`,
		`"str"`,
		`123`,
		`{"contents":123}`,
	}
	for _, seed := range seeds {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > fuzzMaxInput {
			raw = raw[:fuzzMaxInput]
		}
		message := json.RawMessage(raw)
		_, _ = parseHover(message)
		_, _ = parseLocations(message)
		_, _ = parseSymbols(message)
		_, _ = parseDiagnostics(message)
	})
}
