package lsp

// The fake language server runs as a subprocess of this same test binary: the
// test binary re-executes itself with RIG_LSP_TEST_MODE set, TestMain dispatch
// runs runFakeServer, and stdin/stdout become the JSON-RPC transport. No gopls,
// no external server binary and no network are needed.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

const (
	fakeModeEnv   = "RIG_LSP_TEST_MODE"
	fakeRecordEnv = "RIG_LSP_TEST_RECORD"
)

func TestMain(m *testing.M) {
	if mode := strings.TrimSpace(os.Getenv(fakeModeEnv)); mode != "" {
		runFakeServer(mode)
		return
	}
	os.Exit(m.Run())
}

// fakeServer speaks just enough LSP for the client tests and records every
// request/notification it receives (method + params) for assertions.
type fakeServer struct {
	out     *bufio.Writer
	record  string
	docURI  string
	docText string
}

func runFakeServer(mode string) {
	if mode == "crash-on-start" {
		fmt.Fprintln(os.Stderr, "fake language server: crashing on startup")
		os.Exit(3)
	}
	server := &fakeServer{
		out:    bufio.NewWriter(os.Stdout),
		record: os.Getenv(fakeRecordEnv),
	}
	defer server.flush()
	in := bufio.NewReader(os.Stdin)
	for {
		message, err := readMessage(in)
		if err != nil {
			return
		}
		server.recordMessage(message)
		server.handle(mode, message)
	}
}

func (s *fakeServer) flush() {
	if s.out != nil {
		_ = s.out.Flush()
	}
}

func (s *fakeServer) recordMessage(message wireMessage) {
	if s.record == "" || message.Method == "" {
		return
	}
	entry := map[string]any{
		"method": message.Method,
		"params": json.RawMessage(message.Params),
		"hasID":  len(message.ID) > 0,
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	file, err := os.OpenFile(s.record, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	_, _ = file.Write(append(data, '\n'))
	_ = file.Close()
}

func (s *fakeServer) result(id json.RawMessage, result any) {
	data := json.RawMessage("null")
	if result != nil {
		encoded, err := json.Marshal(result)
		if err != nil {
			s.fail(err)
			return
		}
		data = encoded
	}
	s.write(wireMessage{JSONRPC: "2.0", ID: id, Result: data})
}

func (s *fakeServer) errorResponse(id json.RawMessage, code int, message string) {
	s.write(wireMessage{JSONRPC: "2.0", ID: id, Error: &wireError{Code: code, Message: message}})
}

func (s *fakeServer) notify(method string, params any) {
	data, err := json.Marshal(params)
	if err != nil {
		s.fail(err)
		return
	}
	s.write(wireMessage{JSONRPC: "2.0", Method: method, Params: data})
}

func (s *fakeServer) fail(err error) {
	fmt.Fprintln(os.Stderr, "fake language server:", err)
	s.flush()
	os.Exit(1)
}

func (s *fakeServer) write(message wireMessage) {
	if err := writeMessage(s.out, message); err != nil {
		s.fail(err)
	}
	s.flush()
}

func (s *fakeServer) handle(mode string, message wireMessage) {
	switch message.Method {
	case "initialize":
		if mode == "hang-initialize" {
			time.Sleep(700 * time.Millisecond)
		}
		capabilities := map[string]any{
			"hoverProvider":          true,
			"definitionProvider":     true,
			"referencesProvider":     true,
			"implementationProvider": true,
			"documentSymbolProvider": true,
			"textDocumentSync":       1,
		}
		if mode == "capabilities-minimal" {
			// 只声明 hover/definition：用于验证 references/implementation 的服务端能力检查。
			delete(capabilities, "referencesProvider")
			delete(capabilities, "implementationProvider")
		}
		s.result(message.ID, map[string]any{
			"capabilities": capabilities,
			"serverInfo":   map[string]any{"name": "fake-lsp", "version": "0.0.1"},
		})

	case "initialized":
		// nothing to do

	case "textDocument/didOpen":
		var params struct {
			TextDocument struct {
				URI  string `json:"uri"`
				Text string `json:"text"`
			} `json:"textDocument"`
		}
		_ = json.Unmarshal(message.Params, &params)
		s.docURI = params.TextDocument.URI
		s.docText = params.TextDocument.Text
		switch mode {
		case "diagnostics":
			s.publishDiagnostics()
		case "diagnostics-late":
			time.Sleep(100 * time.Millisecond)
			s.publishDiagnostics()
		}

	case "textDocument/didChange":
		var params struct {
			ContentChanges []struct {
				Text string `json:"text"`
			} `json:"contentChanges"`
		}
		_ = json.Unmarshal(message.Params, &params)
		if len(params.ContentChanges) > 0 {
			s.docText = params.ContentChanges[0].Text
		}

	case "textDocument/hover":
		switch mode {
		case "error":
			s.errorResponse(message.ID, -32000, "fake hover failure")
		case "crash-on-hover":
			s.flush()
			os.Exit(3)
		case "hang-hover":
			time.Sleep(700 * time.Millisecond)
			s.hoverResult(message.ID)
		case "hover-plain":
			s.result(message.ID, map[string]any{"contents": "plain hover text"})
		default:
			s.hoverResult(message.ID)
		}

	case "textDocument/definition":
		switch mode {
		case "definition-multi":
			s.result(message.ID, []any{
				map[string]any{"uri": s.docURI, "range": fakeRange(2, 4, 2, 9)},
				map[string]any{
					"targetUri":            "file:///tmp/other/target.go",
					"targetRange":          fakeRange(2, 4, 2, 9),
					"targetSelectionRange": fakeRange(9, 1, 9, 5),
				},
			})
		case "definition-null":
			s.result(message.ID, nil)
		default:
			s.result(message.ID, map[string]any{"uri": s.docURI, "range": fakeRange(2, 4, 2, 9)})
		}

	case "textDocument/references":
		switch mode {
		case "references-multi":
			s.result(message.ID, []any{
				map[string]any{"uri": s.docURI, "range": fakeRange(2, 4, 2, 9)},
				map[string]any{
					"targetUri":            "file:///tmp/other/target.go",
					"targetRange":          fakeRange(2, 4, 2, 9),
					"targetSelectionRange": fakeRange(9, 1, 9, 5),
				},
			})
		case "references-null":
			s.result(message.ID, nil)
		default:
			s.result(message.ID, map[string]any{"uri": s.docURI, "range": fakeRange(2, 4, 2, 9)})
		}

	case "textDocument/implementation":
		switch mode {
		case "implementation-multi":
			s.result(message.ID, []any{
				map[string]any{"uri": s.docURI, "range": fakeRange(2, 4, 2, 9)},
				map[string]any{
					"targetUri":            "file:///tmp/other/impl.go",
					"targetSelectionRange": fakeRange(4, 1, 4, 7),
				},
			})
		case "implementation-null":
			s.result(message.ID, nil)
		default:
			s.result(message.ID, map[string]any{
				"targetUri":            "file:///tmp/other/impl.go",
				"targetSelectionRange": fakeRange(4, 1, 4, 7),
			})
		}

	case "textDocument/documentSymbol":
		s.result(message.ID, fakeSymbols())

	case "shutdown":
		s.result(message.ID, nil)

	case "exit":
		s.flush()
		os.Exit(0)

	default:
		if len(message.ID) > 0 {
			s.errorResponse(message.ID, -32601, "fake server does not support "+message.Method)
		}
	}
}

func (s *fakeServer) hoverResult(id json.RawMessage) {
	s.result(id, map[string]any{
		"contents": map[string]any{
			"kind":  "markdown",
			"value": "**fake hover**\n\nmarkdown body",
		},
		"range": fakeRange(0, 0, 0, 6),
	})
}

func (s *fakeServer) publishDiagnostics() {
	if s.docURI == "" {
		return
	}
	s.notify("textDocument/publishDiagnostics", map[string]any{
		"uri": s.docURI,
		"diagnostics": []any{
			map[string]any{
				"range":    fakeRange(1, 2, 1, 5),
				"severity": SeverityError,
				"source":   "fake",
				"message":  "undefined: foo",
			},
			map[string]any{
				"range":    fakeRange(3, 0, 3, 3),
				"severity": SeverityWarning,
				"message":  "unused variable x",
			},
		},
	})
}

func fakeRange(startLine, startCharacter, endLine, endCharacter int) map[string]any {
	return map[string]any{
		"start": map[string]any{"line": startLine, "character": startCharacter},
		"end":   map[string]any{"line": endLine, "character": endCharacter},
	}
}

// fakeSymbols is a two-level document symbol tree plus one flat leaf.
func fakeSymbols() []any {
	return []any{
		map[string]any{
			"name":           "FakeService",
			"kind":           5, // class
			"detail":         "struct",
			"range":          fakeRange(0, 0, 10, 0),
			"selectionRange": fakeRange(0, 6, 0, 17),
			"children": []any{
				map[string]any{
					"name":           "Handler",
					"kind":           12, // function
					"range":          fakeRange(2, 2, 4, 2),
					"selectionRange": fakeRange(2, 2, 2, 12),
				},
				map[string]any{
					"name":           "Ctx",
					"kind":           8, // field
					"range":          fakeRange(5, 1, 5, 8),
					"selectionRange": fakeRange(5, 1, 5, 8),
				},
			},
		},
		map[string]any{
			"name":           "helper",
			"kind":           12, // function
			"range":          fakeRange(7, 0, 9, 0),
			"selectionRange": fakeRange(7, 0, 7, 6),
		},
	}
}
