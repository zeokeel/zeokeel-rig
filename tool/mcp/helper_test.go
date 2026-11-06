package mcp

// Test binary re-exec helper: the same test binary acts as a fake MCP server
// when RIG_MCP_HELPER=1. This keeps the MCP client tests standard-library only:
// no external server binary, no network, deterministic scenarios.

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"testing"
	"time"
)

const (
	helperEnv     = "RIG_MCP_HELPER"
	scenarioEnv   = "RIG_MCP_SCENARIO"
	helperServer  = "fakeserver"
	helperVersion = "1.2.3"
)

// TestMain dispatches into the fake server when this binary is re-executed as a
// child process; otherwise it runs the package tests.
func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		os.Exit(runHelperServer(os.Stdin, os.Stdout, os.Getenv(scenarioEnv)))
	}
	os.Exit(m.Run())
}

// helperRequest is one JSON-RPC message read by the fake server.
type helperRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// runHelperServer serves the newline-delimited JSON-RPC protocol until stdin
// closes. The scenario selects the behavior each test needs.
func runHelperServer(stdin io.Reader, stdout io.Writer, scenario string) int {
	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), MaxMessageBytes)
	out := bufio.NewWriter(stdout)
	initialized := false

	write := func(message any) {
		data, err := json.Marshal(message)
		if err != nil {
			return
		}
		if _, err := out.Write(append(data, '\n')); err != nil {
			return
		}
		_ = out.Flush()
	}
	reply := func(id json.RawMessage, result any) {
		if id == nil {
			return
		}
		write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	replyError := func(id json.RawMessage, code int, message string, data any) {
		if id == nil {
			return
		}
		body := map[string]any{"code": code, "message": message}
		if data != nil {
			body["data"] = data
		}
		write(map[string]any{"jsonrpc": "2.0", "id": id, "error": body})
	}

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var request helperRequest
		if err := json.Unmarshal(line, &request); err != nil {
			continue
		}
		switch request.Method {
		case "initialize":
			version := "2025-06-18"
			if scenario == "unsupported" {
				version = "2099-01-01"
			}
			result := map[string]any{
				"protocolVersion": version,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": helperServer, "version": helperVersion},
			}
			if scenario == "badinit" {
				delete(result, "serverInfo")
			}
			reply(request.ID, result)
		case "notifications/initialized":
			initialized = true
		case "ping":
			if scenario == "emptyresult" {
				write(map[string]any{"jsonrpc": "2.0", "id": request.ID})
				continue
			}
			reply(request.ID, map[string]any{})
		case "tools/list":
			if !initialized {
				replyError(request.ID, -32000, "initialized notification missing", nil)
				continue
			}
			reply(request.ID, helperToolList(request.Params, scenario))
		case "tools/call":
			if !initialized {
				replyError(request.ID, -32000, "initialized notification missing", nil)
				continue
			}
			if code := helperToolCall(request.ID, request.Params, reply, replyError); code != 0 {
				return code
			}
		default:
			replyError(request.ID, -32601, "method not found: "+request.Method, nil)
		}
	}
	return 0
}

// helperToolList returns one tools/list page. The pagination scenario serves two
// pages and repeats "echo" on the second one to exercise deduplication.
func helperToolList(params json.RawMessage, scenario string) map[string]any {
	var cursor string
	if len(params) > 0 {
		var page struct {
			Cursor string `json:"cursor"`
		}
		_ = json.Unmarshal(params, &page)
		cursor = page.Cursor
	}
	if scenario == "pagination" {
		if cursor == "page-2" {
			return map[string]any{
				"tools": []any{
					helperTool("echo", "Echo text back", helperSchema("text")),
					helperTool("picture", "Return an image", helperSchema()),
				},
			}
		}
		return map[string]any{
			"tools": []any{
				helperTool("echo", "Echo text back", helperSchema("text")),
				helperTool("fail", "Always fail", helperSchema()),
			},
			"nextCursor": "page-2",
		}
	}
	return map[string]any{"tools": []any{
		helperTool("echo", "Echo text back", helperSchema("text")),
		helperTool("fail", "Always fail", helperSchema()),
		helperTool("picture", "Return an image", helperSchema()),
		helperTool("structured", "Return structured content", helperSchema()),
		helperTool("crash", "Exit the server", helperSchema()),
		helperTool("rpc_error", "Return a JSON-RPC error", helperSchema()),
		helperTool("slow", "Reply after a delay", helperSchema()),
	}}
}

// helperTool builds one tools/list entry.
func helperTool(name, description string, schema map[string]any) map[string]any {
	return map[string]any{"name": name, "description": description, "inputSchema": schema}
}

// helperSchema builds a minimal object schema with the named string properties.
func helperSchema(properties ...string) map[string]any {
	fields := make(map[string]any, len(properties))
	for _, property := range properties {
		fields[property] = map[string]any{"type": "string"}
	}
	schema := map[string]any{"type": "object", "properties": fields}
	if len(properties) > 0 {
		schema["required"] = properties
	}
	return schema
}

// helperToolCall serves one tools/call request. A non-zero return code makes
// the fake server exit (used by the crash scenario); normal calls return 0.
func helperToolCall(
	id json.RawMessage,
	params json.RawMessage,
	reply func(json.RawMessage, any),
	replyError func(json.RawMessage, int, string, any),
) int {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		replyError(id, -32602, "invalid params", nil)
		return 0
	}
	switch call.Name {
	case "echo":
		var arguments struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(call.Arguments, &arguments)
		reply(id, map[string]any{"content": []any{
			map[string]any{"type": "text", "text": "echo: " + arguments.Text},
		}})
	case "fail":
		reply(id, map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "tool failed"}},
			"isError": true,
		})
	case "picture":
		reply(id, map[string]any{"content": []any{
			map[string]any{"type": "text", "text": "here"},
			map[string]any{"type": "image", "data": "aGVsbG8=", "mimeType": "image/png"},
		}})
	case "structured":
		reply(id, map[string]any{
			"content":           []any{},
			"structuredContent": map[string]any{"answer": 42},
		})
	case "rpc_error":
		replyError(id, -32000, "tool exploded", map[string]any{"reason": "test"})
	case "slow":
		time.Sleep(500 * time.Millisecond)
		reply(id, map[string]any{"content": []any{
			map[string]any{"type": "text", "text": "slow done"},
		}})
	case "crash":
		return 3
	default:
		replyError(id, -32602, "unknown tool: "+call.Name, nil)
	}
	return 0
}
