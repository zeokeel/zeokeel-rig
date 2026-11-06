package mcp

// Test binary re-exec helper: the same test binary acts as a fake MCP server
// when RIG_MCP_HELPER=1. This keeps the MCP client tests standard-library only:
// no external server binary, no network, deterministic scenarios.

import (
	"bufio"
	"encoding/base64"
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
				"capabilities":    helperCapabilities(scenario),
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
		case "resources/list":
			if !initialized {
				replyError(request.ID, -32000, "initialized notification missing", nil)
				continue
			}
			if !scenarioHasResources(scenario) {
				replyError(request.ID, -32601, "method not found: resources/list (capability not declared)", nil)
				continue
			}
			reply(request.ID, helperResourceList(request.Params, scenario))
		case "resources/read":
			if !initialized {
				replyError(request.ID, -32000, "initialized notification missing", nil)
				continue
			}
			if !scenarioHasResources(scenario) {
				replyError(request.ID, -32601, "method not found: resources/read (capability not declared)", nil)
				continue
			}
			helperResourceRead(request.ID, request.Params, reply, replyError)
		case "prompts/list":
			if !initialized {
				replyError(request.ID, -32000, "initialized notification missing", nil)
				continue
			}
			if !scenarioHasPrompts(scenario) {
				replyError(request.ID, -32601, "method not found: prompts/list (capability not declared)", nil)
				continue
			}
			reply(request.ID, helperPromptList(request.Params, scenario))
		case "prompts/get":
			if !initialized {
				replyError(request.ID, -32000, "initialized notification missing", nil)
				continue
			}
			if !scenarioHasPrompts(scenario) {
				replyError(request.ID, -32601, "method not found: prompts/get (capability not declared)", nil)
				continue
			}
			helperGetPrompt(request.ID, request.Params, reply, replyError)
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

// --- resources/ and prompts/ scenarios -------------------------------------

// helperCapabilities returns the capabilities the fake server declares for a
// scenario. The default scenario declares tools only, which is what the
// capability-not-declared tests rely on (the server answers resources/prompts
// methods with a JSON-RPC -32601).
func helperCapabilities(scenario string) map[string]any {
	capabilities := map[string]any{"tools": map[string]any{}}
	if scenarioHasResources(scenario) {
		capabilities["resources"] = map[string]any{}
	}
	if scenarioHasPrompts(scenario) {
		capabilities["prompts"] = map[string]any{}
	}
	return capabilities
}

func scenarioHasResources(scenario string) bool {
	switch scenario {
	case "resources", "resources-pagination", "empty", "full":
		return true
	}
	return false
}

func scenarioHasPrompts(scenario string) bool {
	switch scenario {
	case "prompts", "prompts-pagination", "empty", "full":
		return true
	}
	return false
}

// helperCursor decodes the pagination cursor of a list request.
func helperCursor(params json.RawMessage) string {
	var page struct {
		Cursor string `json:"cursor"`
	}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &page)
	}
	return page.Cursor
}

// helperResourceList returns one resources/list page for a scenario.
func helperResourceList(params json.RawMessage, scenario string) map[string]any {
	resource := func(uri, name, description, mimeType string) map[string]any {
		return map[string]any{"uri": uri, "name": name, "description": description, "mimeType": mimeType}
	}
	switch scenario {
	case "empty":
		return map[string]any{"resources": []any{}}
	case "resources-pagination":
		if helperCursor(params) == "resources-page-2" {
			// The first entry repeats page one's last URI (deduplicated by the
			// client); "mem://doc/c" is new.
			return map[string]any{"resources": []any{
				resource("mem://doc/b", "b", "second resource", "text/plain"),
				resource("mem://doc/c", "c", "third resource", "text/plain"),
			}}
		}
		return map[string]any{
			"resources": []any{
				resource("mem://doc/a", "a", "first resource", "text/plain"),
				resource("mem://doc/b", "b", "second resource", "text/plain"),
			},
			"nextCursor": "resources-page-2",
		}
	case "full", "resources":
		return map[string]any{"resources": []any{
			resource("mem://doc/plain", "plain", "A plain text resource", "text/plain"),
			resource("mem://doc/binary", "binary", "A binary resource", "application/octet-stream"),
			resource("mem://doc/multi", "multi", "Two contents", "text/plain"),
		}}
	default:
		return map[string]any{"resources": []any{}}
	}
}

// helperResourceRead serves resources/read by URI.
func helperResourceRead(
	id json.RawMessage,
	params json.RawMessage,
	reply func(json.RawMessage, any),
	replyError func(json.RawMessage, int, string, any),
) {
	var request struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(params, &request); err != nil {
		replyError(id, -32602, "invalid params", nil)
		return
	}
	switch request.URI {
	case "mem://doc/plain":
		reply(id, map[string]any{"contents": []any{
			map[string]any{"uri": request.URI, "mimeType": "text/plain", "text": "plain resource text"},
		}})
	case "mem://doc/binary":
		reply(id, map[string]any{"contents": []any{
			map[string]any{
				"uri":      request.URI,
				"mimeType": "application/octet-stream",
				"blob":     base64.StdEncoding.EncodeToString([]byte("binary-data")),
			},
		}})
	case "mem://doc/multi":
		reply(id, map[string]any{"contents": []any{
			map[string]any{"uri": request.URI + "#one", "mimeType": "text/plain", "text": "first content"},
			map[string]any{"uri": request.URI + "#two", "mimeType": "text/plain", "text": "second content"},
		}})
	case "mem://doc/empty":
		reply(id, map[string]any{"contents": []any{}})
	case "mem://doc/error":
		replyError(id, -32000, "resource exploded", map[string]any{"uri": request.URI})
	default:
		replyError(id, -32002, "resource not found: "+request.URI, nil)
	}
}

// helperPromptList returns one prompts/list page for a scenario.
func helperPromptList(params json.RawMessage, scenario string) map[string]any {
	prompt := func(name, description string, arguments ...map[string]any) map[string]any {
		entry := map[string]any{"name": name, "description": description}
		if len(arguments) > 0 {
			entry["arguments"] = arguments
		}
		return entry
	}
	argument := func(name, description string, required bool) map[string]any {
		return map[string]any{"name": name, "description": description, "required": required}
	}
	switch scenario {
	case "empty":
		return map[string]any{"prompts": []any{}}
	case "prompts-pagination":
		if helperCursor(params) == "prompts-page-2" {
			// "greet" repeats page one (deduplicated); "review" is new.
			return map[string]any{"prompts": []any{
				prompt("greet", "Greet someone"),
				prompt("review", "Review a change"),
			}}
		}
		return map[string]any{
			"prompts": []any{
				prompt("greet", "Greet someone"),
				prompt("cleanup", "Clean up a workspace"),
			},
			"nextCursor": "prompts-page-2",
		}
	case "full", "prompts":
		return map[string]any{"prompts": []any{
			prompt("greet", "Greet someone", argument("name", "Person to greet", true)),
			prompt("plain", "A prompt without arguments"),
			prompt("resource", "A prompt with an embedded resource"),
			prompt("error", "Always fails"),
		}}
	default:
		return map[string]any{"prompts": []any{}}
	}
}

// helperGetPrompt serves prompts/get by name.
func helperGetPrompt(
	id json.RawMessage,
	params json.RawMessage,
	reply func(json.RawMessage, any),
	replyError func(json.RawMessage, int, string, any),
) {
	var request struct {
		Name      string            `json:"name"`
		Arguments map[string]string `json:"arguments"`
	}
	if err := json.Unmarshal(params, &request); err != nil {
		replyError(id, -32602, "invalid params", nil)
		return
	}
	switch request.Name {
	case "greet":
		name := request.Arguments["name"]
		if name == "" {
			name = "there"
		}
		reply(id, map[string]any{"messages": []any{
			map[string]any{"role": "user", "content": map[string]any{"type": "text", "text": "Hello " + name}},
			map[string]any{"role": "assistant", "content": map[string]any{"type": "text", "text": "Hi! How can I help?"}},
		}})
	case "plain":
		reply(id, map[string]any{"messages": []any{
			map[string]any{"role": "user", "content": map[string]any{"type": "text", "text": "plain prompt"}},
		}})
	case "resource":
		reply(id, map[string]any{"messages": []any{
			map[string]any{"role": "user", "content": map[string]any{
				"type":     "resource",
				"resource": map[string]any{"uri": "mem://doc/plain", "text": "embedded resource text"},
			}},
		}})
	case "error":
		replyError(id, -32000, "prompt exploded", nil)
	default:
		replyError(id, -32602, "unknown prompt: "+request.Name, nil)
	}
}
