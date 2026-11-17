package mcpserver_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/mcpserver"
)

// Server 必须是可嵌入的普通类型（不依赖任何外部状态）。
var _ = func() { _, _ = mcpserver.New(nil) }

// fakeTool 是最小的 rig.Tool 实现，用来精确控制每个用例的行为。
type fakeTool struct {
	name        string
	description string
	schema      map[string]any
	invoke      func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error)
}

func (f *fakeTool) Name() string           { return f.name }
func (f *fakeTool) Description() string    { return f.description }
func (f *fakeTool) Schema() map[string]any { return f.schema }
func (f *fakeTool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	if f.invoke != nil {
		return f.invoke(ctx, call)
	}
	return rig.ToolResult{Content: "ok"}, nil
}

// echoTool 回显它收到的 arguments 原文，便于断言"参数是怎么被传下去的"。
func echoTool(name string) *fakeTool {
	return &fakeTool{
		name:        name,
		description: "echo arguments",
		schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"text": map[string]any{"type": "string"},
			},
		},
		invoke: func(_ context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			return rig.ToolResult{Content: call.Name + "|" + call.Arguments}, nil
		},
	}
}

// newServer 用给定工具构造 server。
func newServer(t *testing.T, tools ...rig.Tool) *mcpserver.Server {
	t.Helper()
	server, err := mcpserver.New(tools)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return server
}

// call 通过 HandleLine 发一条请求并解析响应。
func call(t *testing.T, server *mcpserver.Server, request string) map[string]any {
	t.Helper()
	response, handled, err := server.HandleLine(context.Background(), []byte(request))
	if err != nil {
		t.Fatalf("HandleLine(%s): %v", request, err)
	}
	if !handled {
		t.Fatalf("HandleLine(%s) reported handled=false", request)
	}
	var decoded map[string]any
	if err := json.Unmarshal(response, &decoded); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, response)
	}
	return decoded
}

// result 取出响应的 result 对象。
func result(t *testing.T, response map[string]any) map[string]any {
	t.Helper()
	if errValue, ok := response["error"]; ok {
		t.Fatalf("unexpected JSON-RPC error: %v", errValue)
	}
	raw, ok := response["result"].(map[string]any)
	if !ok {
		t.Fatalf("response has no result object: %v", response)
	}
	return raw
}

// rpcErrorCode 取出 JSON-RPC 错误码（无错误则失败）。
func rpcErrorCode(t *testing.T, response map[string]any) int {
	t.Helper()
	errObject, ok := response["error"].(map[string]any)
	if !ok {
		t.Fatalf("response has no error object: %v", response)
	}
	code, ok := errObject["code"].(float64)
	if !ok {
		t.Fatalf("error.code is not a number: %v", errObject["code"])
	}
	return int(code)
}

func TestInitializeEchoesSupportedProtocolVersion(t *testing.T) {
	server, err := mcpserver.New(nil,
		mcpserver.WithServerInfo("rig-test", "9.9.9"),
		mcpserver.WithInstructions("先读文件再动手"),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	response := call(t, server, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`)
	if got, want := response["jsonrpc"], "2.0"; got != want {
		t.Fatalf("jsonrpc = %v, want %v", got, want)
	}
	if got, want := response["id"], float64(1); got != want {
		t.Fatalf("id = %v, want %v", got, want)
	}
	body := result(t, response)
	if got, want := body["protocolVersion"], "2025-03-26"; got != want {
		t.Fatalf("protocolVersion = %v, want the client's version (echo)", got)
	}
	if got, want := body["instructions"], "先读文件再动手"; got != want {
		t.Fatalf("instructions = %v, want %v", got, want)
	}
	info, ok := body["serverInfo"].(map[string]any)
	if !ok {
		t.Fatalf("serverInfo = %v", body["serverInfo"])
	}
	if info["name"] != "rig-test" || info["version"] != "9.9.9" {
		t.Fatalf("serverInfo = %v", info)
	}
	capabilities, ok := body["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("capabilities = %v", body["capabilities"])
	}
	tools, ok := capabilities["tools"].(map[string]any)
	if !ok {
		t.Fatalf("capabilities.tools = %v", capabilities["tools"])
	}
	if tools["listChanged"] != false {
		t.Fatalf("capabilities.tools.listChanged = %v, want false", tools["listChanged"])
	}
}

func TestInitializeUnsupportedVersionFallsBack(t *testing.T) {
	server := newServer(t)
	body := result(t, call(t, server, `{"jsonrpc":"2.0","id":"a","method":"initialize","params":{"protocolVersion":"1999-01-01"}}`))
	if got, want := body["protocolVersion"], mcpserver.ProtocolVersion; got != want {
		t.Fatalf("protocolVersion = %v, want the default %v", got, want)
	}
	if _, ok := body["instructions"]; ok {
		t.Fatal("instructions present without WithInstructions")
	}
}

func TestInitializeWithoutParams(t *testing.T) {
	server := newServer(t)
	body := result(t, call(t, server, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	if got, want := body["protocolVersion"], mcpserver.ProtocolVersion; got != want {
		t.Fatalf("protocolVersion = %v, want %v", got, want)
	}
	info, ok := body["serverInfo"].(map[string]any)
	if !ok || info["name"] == "" || info["version"] == "" {
		t.Fatalf("serverInfo = %v, want default name/version", body["serverInfo"])
	}
}

func TestInitializedNotificationHasNoResponse(t *testing.T) {
	server := newServer(t)
	response, handled, err := server.HandleLine(context.Background(), []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	if err != nil {
		t.Fatalf("HandleLine: %v", err)
	}
	if handled {
		t.Fatalf("handled = true, want false for a notification")
	}
	if len(response) != 0 {
		t.Fatalf("response = %s, want none", response)
	}
}

func TestToolsListPassesSchemaThrough(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "文件路径"},
		},
		"required":             []any{"path"},
		"additionalProperties": false,
	}
	tool := &fakeTool{name: "read_file", description: "读文件", schema: schema}
	server := newServer(t, tool, echoTool("echo"))

	body := result(t, call(t, server, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	list, ok := body["tools"].([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("tools = %v, want 2 entries", body["tools"])
	}
	first := list[0].(map[string]any)
	if first["name"] != "read_file" || first["description"] != "读文件" {
		t.Fatalf("tool[0] = %v", first)
	}
	encodedSchema, err := json.Marshal(first["inputSchema"])
	if err != nil {
		t.Fatalf("marshal inputSchema: %v", err)
	}
	var got, want map[string]any
	if err := json.Unmarshal(encodedSchema, &got); err != nil {
		t.Fatalf("inputSchema is not an object: %v", err)
	}
	raw, _ := json.Marshal(schema)
	_ = json.Unmarshal(raw, &want)
	gotText, _ := json.Marshal(got)
	wantText, _ := json.Marshal(want)
	if !bytes.Equal(gotText, wantText) {
		t.Fatalf("inputSchema = %s, want %s", gotText, wantText)
	}
	if list[1].(map[string]any)["name"] != "echo" {
		t.Fatalf("tool[1] = %v", list[1])
	}
}

func TestToolsListEmpty(t *testing.T) {
	server := newServer(t)
	response := call(t, server, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	// 必须是合法的空数组，而不是 null 或缺字段。
	if !bytes.Contains(mustJSON(t, response), []byte(`"tools":[]`)) {
		t.Fatalf("tools/list = %s, want an empty array", mustJSON(t, response))
	}
}

func TestToolsListNormalisesMissingSchema(t *testing.T) {
	// 没有 schema 的工具也必须公布一个 object schema（MCP 要求）。
	tool := &fakeTool{name: "noargs", description: "无参数"}
	server := newServer(t, tool)
	body := result(t, call(t, server, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	list := body["tools"].([]any)
	schema := list[0].(map[string]any)["inputSchema"].(map[string]any)
	if schema["type"] != "object" {
		t.Fatalf("inputSchema = %v, want type=object", schema)
	}
}

func TestToolsCallSuccess(t *testing.T) {
	tool := &fakeTool{
		name: "greet",
		invoke: func(_ context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			if call.Name != "greet" {
				t.Fatalf("tool saw name %q, want greet", call.Name)
			}
			if !strings.Contains(call.Arguments, `"who":"世界"`) {
				t.Fatalf("tool saw arguments %q", call.Arguments)
			}
			return rig.ToolResult{Content: "你好，世界"}, nil
		},
	}
	server := newServer(t, tool)
	response := call(t, server, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"greet","arguments":{"who":"世界"}}}`)
	body := result(t, response)
	if _, ok := body["isError"]; ok {
		t.Fatalf("isError present on success: %v", body)
	}
	content, ok := body["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("content = %v", body["content"])
	}
	first := content[0].(map[string]any)
	if first["type"] != "text" || first["text"] != "你好，世界" {
		t.Fatalf("content[0] = %v", first)
	}
}

func TestToolsCallIsErrorResult(t *testing.T) {
	tool := &fakeTool{
		name: "fail",
		invoke: func(context.Context, rig.ToolCall) (rig.ToolResult, error) {
			return rig.ToolResult{Content: "拒绝执行：路径越界", IsError: true}, nil
		},
	}
	server := newServer(t, tool)
	body := result(t, call(t, server, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fail"}}`))
	if body["isError"] != true {
		t.Fatalf("isError = %v, want true", body["isError"])
	}
	content := body["content"].([]any)
	if content[0].(map[string]any)["text"] != "拒绝执行：路径越界" {
		t.Fatalf("content = %v", content)
	}
}

func TestToolsCallGoErrorBecomesIsErrorContent(t *testing.T) {
	// 工具执行失败是"工具级失败"，不是协议错误：必须回 isError content，不能是 -32603。
	tool := &fakeTool{
		name: "boom",
		invoke: func(context.Context, rig.ToolCall) (rig.ToolResult, error) {
			return rig.ToolResult{}, errors.New("磁盘只读")
		},
	}
	server := newServer(t, tool)
	response := call(t, server, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"boom"}}`)
	body := result(t, response)
	if body["isError"] != true {
		t.Fatalf("isError = %v, want true", body["isError"])
	}
	content := body["content"].([]any)
	if !strings.Contains(content[0].(map[string]any)["text"].(string), "磁盘只读") {
		t.Fatalf("content = %v, want the error text", content)
	}
}

func TestToolsCallPanicBecomesIsErrorContent(t *testing.T) {
	tool := &fakeTool{
		name: "panics",
		invoke: func(context.Context, rig.ToolCall) (rig.ToolResult, error) {
			panic("工具内部炸了")
		},
	}
	server := newServer(t, tool)
	body := result(t, call(t, server, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"panics"}}`))
	if body["isError"] != true {
		t.Fatalf("isError = %v, want true", body["isError"])
	}
	text := body["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "工具内部炸了") {
		t.Fatalf("content text = %q, want the panic value", text)
	}
}

func TestToolsCallUnknownToolIsInvalidParams(t *testing.T) {
	server := newServer(t, echoTool("known"))
	response := call(t, server, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope","arguments":{}}}`)
	if code := rpcErrorCode(t, response); code != mcpserver.CodeInvalidParams {
		t.Fatalf("error code = %d, want %d", code, mcpserver.CodeInvalidParams)
	}
}

func TestToolsCallNonObjectArgumentsIsInvalidParams(t *testing.T) {
	server := newServer(t, echoTool("echo"))
	for _, arguments := range []string{`[1,2,3]`, `"字符串"`, `42`, `true`} {
		request := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":%s}}`, arguments)
		response := call(t, server, request)
		if code := rpcErrorCode(t, response); code != mcpserver.CodeInvalidParams {
			t.Fatalf("arguments=%s: error code = %d, want %d", arguments, code, mcpserver.CodeInvalidParams)
		}
	}
}

func TestToolsCallMissingArgumentsBecomesEmptyObject(t *testing.T) {
	server := newServer(t, echoTool("echo"))
	body := result(t, call(t, server, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo"}}`))
	text := body["content"].([]any)[0].(map[string]any)["text"].(string)
	if text != "echo|{}" {
		t.Fatalf("tool received %q, want %q", text, "echo|{}")
	}
}

func TestToolsCallMissingNameIsInvalidParams(t *testing.T) {
	server := newServer(t, echoTool("echo"))
	response := call(t, server, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"arguments":{}}}`)
	if code := rpcErrorCode(t, response); code != mcpserver.CodeInvalidParams {
		t.Fatalf("error code = %d, want %d", code, mcpserver.CodeInvalidParams)
	}
}

func TestToolsCallPartsBecomeExtraContent(t *testing.T) {
	tool := &fakeTool{
		name: "withparts",
		invoke: func(context.Context, rig.ToolCall) (rig.ToolResult, error) {
			return rig.ToolResult{
				Content: "已读取",
				Parts:   []rig.ContentPart{rig.TextPart("附加文本")},
			}, nil
		},
	}
	server := newServer(t, tool)
	body := result(t, call(t, server, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"withparts"}}`))
	content := body["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content = %v, want the result text plus the part", content)
	}
	if content[1].(map[string]any)["text"] != "附加文本" {
		t.Fatalf("content[1] = %v", content[1])
	}
}

func TestPing(t *testing.T) {
	server := newServer(t)
	response := call(t, server, `{"jsonrpc":"2.0","id":3,"method":"ping"}`)
	body := result(t, response)
	if len(body) != 0 {
		t.Fatalf("ping result = %v, want an empty object", body)
	}
}

func TestUnknownMethodIsMethodNotFound(t *testing.T) {
	server := newServer(t)
	response := call(t, server, `{"jsonrpc":"2.0","id":1,"method":"resources/list"}`)
	if code := rpcErrorCode(t, response); code != mcpserver.CodeMethodNotFound {
		t.Fatalf("error code = %d, want %d", code, mcpserver.CodeMethodNotFound)
	}
}

func TestUnknownNotificationIsIgnored(t *testing.T) {
	server := newServer(t)
	response, handled, err := server.HandleLine(context.Background(), []byte(`{"jsonrpc":"2.0","method":"notifications/whatever","params":{"x":1}}`))
	if err != nil {
		t.Fatalf("HandleLine: %v", err)
	}
	if handled || len(response) != 0 {
		t.Fatalf("handled=%t response=%s, want an ignored notification", handled, response)
	}
}

func TestMalformedJSONIsParseError(t *testing.T) {
	server := newServer(t)
	response := call(t, server, `{"jsonrpc":"2.0","id":1,"method":`)
	if code := rpcErrorCode(t, response); code != mcpserver.CodeParseError {
		t.Fatalf("error code = %d, want %d", code, mcpserver.CodeParseError)
	}
}

func TestBatchArrayIsInvalidRequest(t *testing.T) {
	server := newServer(t)
	response := call(t, server, `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`)
	if code := rpcErrorCode(t, response); code != mcpserver.CodeInvalidRequest {
		t.Fatalf("error code = %d, want %d", code, mcpserver.CodeInvalidRequest)
	}
}

func TestIDTypeIsPreserved(t *testing.T) {
	server := newServer(t)
	numeric := call(t, server, `{"jsonrpc":"2.0","id":42,"method":"ping"}`)
	if _, ok := numeric["id"].(float64); !ok {
		t.Fatalf("numeric id came back as %T, want a JSON number", numeric["id"])
	}
	textual := call(t, server, `{"jsonrpc":"2.0","id":"req-7","method":"ping"}`)
	if textual["id"] != "req-7" {
		t.Fatalf("string id came back as %v, want \"req-7\"", textual["id"])
	}
	if !bytes.Contains(mustJSON(t, textual), []byte(`"id":"req-7"`)) {
		t.Fatalf("string id was not quoted in the response: %s", mustJSON(t, textual))
	}
}

func TestNonASCIIAndEmoji(t *testing.T) {
	server := newServer(t, echoTool("echo"))
	request := `{"jsonrpc":"2.0","id":"中文","method":"tools/call","params":{"name":"echo","arguments":{"text":"emoji 🚀 与中文"}}}`
	response := call(t, server, request)
	body := result(t, response)
	text := body["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "emoji 🚀 与中文") {
		t.Fatalf("tool received %q, want the emoji payload intact", text)
	}
	// 不转义成 \u 序列（JSON 合法但人类可读性差，客户端也会当成等价的——这里
	// 只要求能往返，不强制编码风格）。
	if !strings.Contains(string(mustJSON(t, response)), "🚀") {
		t.Fatalf("response lost the emoji: %s", mustJSON(t, response))
	}
}

func TestHandleLineNotificationSemantics(t *testing.T) {
	server := newServer(t)
	cases := []struct {
		name    string
		line    string
		handled bool
	}{
		{name: "request", line: `{"jsonrpc":"2.0","id":1,"method":"ping"}`, handled: true},
		{name: "notification", line: `{"jsonrpc":"2.0","method":"notifications/initialized"}`, handled: false},
		{name: "unknown notification", line: `{"jsonrpc":"2.0","method":"foo/bar"}`, handled: false},
		{name: "bad json", line: `{`, handled: true},
		{name: "oversized", line: `{"jsonrpc":"2.0","id":1,"method":"ping","params":"` + strings.Repeat("x", mcpserver.MaxLineBytes) + `"}`, handled: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			response, handled, err := server.HandleLine(context.Background(), []byte(testCase.line))
			if err != nil {
				t.Fatalf("HandleLine: %v", err)
			}
			if handled != testCase.handled {
				t.Fatalf("handled = %t, want %t", handled, testCase.handled)
			}
			if testCase.handled && len(response) == 0 {
				t.Fatal("handled=true but no response bytes")
			}
			if !testCase.handled && len(response) != 0 {
				t.Fatalf("handled=false but response=%s", response)
			}
		})
	}
	// 超长行必须是 parse error，而不是 invalid params / 静默丢弃。
	response, _, err := server.HandleLine(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"ping","params":"`+strings.Repeat("x", mcpserver.MaxLineBytes)+`"}`))
	if err != nil {
		t.Fatalf("HandleLine: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(response, &decoded); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if code := rpcErrorCode(t, decoded); code != mcpserver.CodeParseError {
		t.Fatalf("oversized line error code = %d, want %d", code, mcpserver.CodeParseError)
	}
}

func TestOversizedLineKeepsStreamAlive(t *testing.T) {
	server := newServer(t)
	input := &bytes.Buffer{}
	input.WriteString(`{"jsonrpc":"2.0","id":1,"method":"ping","params":"` + strings.Repeat("x", mcpserver.MaxLineBytes+512) + `"}` + "\n")
	input.WriteString(`{"jsonrpc":"2.0","id":2,"method":"ping"}` + "\n")
	var output bytes.Buffer
	if err := server.Serve(context.Background(), input, &output); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	responses := decodeLines(t, output.Bytes())
	if len(responses) != 2 {
		t.Fatalf("got %d responses, want 2: %s", len(responses), output.Bytes())
	}
	if code := rpcErrorCode(t, responses[0]); code != mcpserver.CodeParseError {
		t.Fatalf("first response code = %d, want %d", code, mcpserver.CodeParseError)
	}
	result(t, responses[1]) // 第二条必须是正常 ping 响应
	if responses[1]["id"] != float64(2) {
		t.Fatalf("second response id = %v, want 2", responses[1]["id"])
	}
}

func TestServeHandlesBlankLinesAndEOF(t *testing.T) {
	server := newServer(t)
	input := strings.NewReader("\n\n" + `{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n\n")
	var output bytes.Buffer
	if err := server.Serve(context.Background(), input, &output); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	responses := decodeLines(t, output.Bytes())
	if len(responses) != 1 {
		t.Fatalf("got %d responses, want 1 (blank lines must be skipped): %s", len(responses), output.Bytes())
	}
}

func TestServeReturnsOnContextCancel(t *testing.T) {
	server := newServer(t)
	reader, writer := io.Pipe()
	defer func() { _ = writer.Close() }()
	var output lockedBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, reader, &output) }()

	if _, err := fmt.Fprintln(writer, `{"jsonrpc":"2.0","id":1,"method":"ping"}`); err != nil {
		t.Fatalf("write ping: %v", err)
	}
	waitForResponse(t, &output, 1)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Serve error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after ctx cancel")
	}
}

func TestServeWaitsForInflightCallOnEOF(t *testing.T) {
	// Serve 返回时所有在途调用必须已经结束（响应也可能已经写出）。
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	tool := &fakeTool{
		name: "slow",
		invoke: func(context.Context, rig.ToolCall) (rig.ToolResult, error) {
			started <- struct{}{}
			<-release
			return rig.ToolResult{Content: "完成"}, nil
		},
	}
	server := newServer(t, tool)
	reader, writer := io.Pipe()
	var output lockedBuffer
	done := make(chan error, 1)
	go func() { done <- server.Serve(context.Background(), reader, &output) }()

	if _, err := fmt.Fprintln(writer, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"slow"}}`); err != nil {
		t.Fatalf("write call: %v", err)
	}
	<-started
	_ = writer.Close() // EOF：输入结束
	select {
	case err := <-done:
		t.Fatalf("Serve returned (%v) while a tool call was still in flight", err)
	case <-time.After(100 * time.Millisecond):
		// 期望：Serve 还在等在途调用
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the in-flight call finished")
	}
	responses := decodeLines(t, output.Bytes())
	if len(responses) != 1 {
		t.Fatalf("got %d responses, want 1: %s", len(responses), output.Bytes())
	}
	if result(t, responses[0])["content"].([]any)[0].(map[string]any)["text"] != "完成" {
		t.Fatalf("response = %v", responses[0])
	}
}

func TestConcurrentCallsDoNotBlockPing(t *testing.T) {
	// 5 个并发 call 同时阻塞；ping 必须在这 5 个结束之前拿到响应。
	const callers = 5
	started := make(chan struct{}, callers)
	release := make(chan struct{})
	tool := &fakeTool{
		name: "blocking",
		invoke: func(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			started <- struct{}{}
			select {
			case <-release:
				return rig.ToolResult{Content: call.Arguments}, nil
			case <-ctx.Done():
				return rig.ToolResult{Content: "cancelled", IsError: true}, nil
			}
		},
	}
	server := newServer(t, tool)
	reader, writer := io.Pipe()
	var output lockedBuffer
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(context.Background(), reader, &output) }()

	for index := 1; index <= callers; index++ {
		line := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"blocking","arguments":{"i":%d}}}`, index, index)
		if _, err := fmt.Fprintln(writer, line); err != nil {
			t.Fatalf("write call %d: %v", index, err)
		}
	}
	for index := 0; index < callers; index++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d/%d calls started: they are being serialised", index, callers)
		}
	}
	// 此时 5 个 call 全部在跑；ping 必须立刻回。
	if _, err := fmt.Fprintln(writer, `{"jsonrpc":"2.0","id":99,"method":"ping"}`); err != nil {
		t.Fatalf("write ping: %v", err)
	}
	waitForResponse(t, &output, 1)
	first := decodeLines(t, output.Bytes())[0]
	if first["id"] != float64(99) {
		t.Fatalf("first response = %v, want the ping response (id 99) — calls are blocking each other", first)
	}
	close(release)
	waitForResponse(t, &output, callers+1)
	_ = writer.Close()
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
	}
}

func TestCancellationNotificationCancelsRunningCall(t *testing.T) {
	cancelled := make(chan struct{}, 1)
	tool := &fakeTool{
		name: "hang",
		invoke: func(ctx context.Context, _ rig.ToolCall) (rig.ToolResult, error) {
			select {
			case <-ctx.Done():
				cancelled <- struct{}{}
				return rig.ToolResult{Content: "已被取消", IsError: true}, nil
			case <-time.After(10 * time.Second):
				return rig.ToolResult{Content: "超时"}, nil
			}
		},
	}
	server := newServer(t, tool)
	reader, writer := io.Pipe()
	var output lockedBuffer
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(context.Background(), reader, &output) }()

	if _, err := fmt.Fprintln(writer, `{"jsonrpc":"2.0","id":"call-1","method":"tools/call","params":{"name":"hang"}}`); err != nil {
		t.Fatalf("write call: %v", err)
	}
	// 让调用先真正开始（给 Serve 一点时间解析并启动 goroutine）。
	time.Sleep(50 * time.Millisecond)
	if _, err := fmt.Fprintln(writer, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"call-1","reason":"用户停止"}}`); err != nil {
		t.Fatalf("write cancellation: %v", err)
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("tool context was not cancelled by notifications/cancelled")
	}
	waitForResponse(t, &output, 1)
	response := decodeLines(t, output.Bytes())[0]
	if response["id"] != "call-1" {
		t.Fatalf("response id = %v, want \"call-1\"", response["id"])
	}
	body := result(t, response)
	if body["isError"] != true {
		t.Fatalf("cancelled call result = %v, want isError", body)
	}
	_ = writer.Close()
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
	}
}

func TestNotifyLogWritesNotification(t *testing.T) {
	server := newServer(t)
	reader, writer := io.Pipe()
	var output lockedBuffer
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(context.Background(), reader, &output) }()

	// 等 Serve 接上 writer 之后再发通知。
	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := server.NotifyLog("warning", "工具变慢了"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("NotifyLog never became available")
		}
		time.Sleep(5 * time.Millisecond)
	}
	waitForResponse(t, &output, 1)
	lines := decodeLines(t, output.Bytes())
	message := lines[0]
	if message["method"] != "notifications/message" {
		t.Fatalf("method = %v, want notifications/message", message["method"])
	}
	params := message["params"].(map[string]any)
	if params["level"] != "warning" {
		t.Fatalf("level = %v, want warning", params["level"])
	}
	if params["data"] != "工具变慢了" {
		t.Fatalf("data = %v, want the message", params["data"])
	}
	if _, hasID := message["id"]; hasID {
		t.Fatalf("notification carries an id: %v", message)
	}
	_ = writer.Close()
	<-serveDone
}

func TestNotifyLogWithoutServeFails(t *testing.T) {
	server := newServer(t)
	if err := server.NotifyLog("info", "x"); err == nil {
		t.Fatal("NotifyLog before Serve succeeded, want an error")
	}
}

func TestNotifyLogEmptyLevelDefaultsToInfo(t *testing.T) {
	server := newServer(t)
	reader, writer := io.Pipe()
	var output lockedBuffer
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(context.Background(), reader, &output) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := server.NotifyLog("", "x"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("NotifyLog never became available")
		}
		time.Sleep(5 * time.Millisecond)
	}
	waitForResponse(t, &output, 1)
	params := decodeLines(t, output.Bytes())[0]["params"].(map[string]any)
	if params["level"] != "info" {
		t.Fatalf("level = %v, want the default info", params["level"])
	}
	_ = writer.Close()
	<-serveDone
}

func TestNewRejectsDuplicateToolNames(t *testing.T) {
	if _, err := mcpserver.New([]rig.Tool{echoTool("dup"), echoTool("dup")}); err == nil {
		t.Fatal("New accepted duplicate tool names")
	}
}

func TestNewRejectsEmptyToolName(t *testing.T) {
	if _, err := mcpserver.New([]rig.Tool{echoTool("")}); err == nil {
		t.Fatal("New accepted an empty tool name")
	}
}

// --- 测试辅助 -------------------------------------------------------------

// mustJSON 把任意值编码成 JSON（失败即测试失败）。
func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return encoded
}

// decodeLines 把输出按行解码成 JSON 对象（跳过空行）。
func decodeLines(t *testing.T, output []byte) []map[string]any {
	t.Helper()
	var decoded []map[string]any
	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(make([]byte, 0, 64*1024), 16<<20)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("output line is not JSON: %v (%s)", err, line)
		}
		decoded = append(decoded, entry)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan output: %v", err)
	}
	return decoded
}

// lockedBuffer 是并发安全的 bytes.Buffer（Serve 的写发生在多个 goroutine 上）。
type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *lockedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buffer.Bytes()...)
}

// waitForResponse 等到输出里至少有 want 行完整 JSON。
func waitForResponse(t *testing.T, buffer *lockedBuffer, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		data := buffer.Bytes()
		count := bytes.Count(data, []byte("\n"))
		if count >= want && bytes.HasSuffix(data, []byte("\n")) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d responses, got %d: %s", want, count, data)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestToolsListPreservesRegistrationOrder(t *testing.T) {
	// 宿主用注册顺序表达优先级（重要工具在前），server 不得重排。
	first := &fakeTool{name: "zeta", description: "先注册"}
	second := &fakeTool{name: "alpha", description: "后注册"}
	server := newServer(t, first, second)
	body := result(t, call(t, server, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	list := body["tools"].([]any)
	if list[0].(map[string]any)["name"] != "zeta" || list[1].(map[string]any)["name"] != "alpha" {
		t.Fatalf("tools/list = %v, want registration order [zeta alpha]", list)
	}
}

// 并行执行下**响应顺序不保证**（这正是"慢工具不阻塞 ping"的代价），但每个请求
// 必须恰好得到一条响应、id 与内容一一对应。审计脚本若假设 FIFO 会误报。
func TestResponsesAreCompleteButNotOrdered(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	slow := &fakeTool{
		name:   "slow",
		schema: map[string]any{"type": "object"},
		invoke: func(ctx context.Context, _ rig.ToolCall) (rig.ToolResult, error) {
			started <- struct{}{}
			<-release
			return rig.ToolResult{Content: "slow-done"}, nil
		},
	}
	server := newServer(t, slow, echoTool("echo"))
	reader, writer := io.Pipe()
	var output lockedBuffer
	done := make(chan error, 1)
	go func() { done <- server.Serve(context.Background(), reader, &output) }()

	// 1) 慢调用先发，2) ping 后发，3) echo 最后发。
	for _, line := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"slow"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"a":1}}}`,
	} {
		if _, err := fmt.Fprintln(writer, line); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	<-started
	close(release)
	waitForResponse(t, &output, 3)
	_ = writer.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
	}
	responses := decodeLines(t, output.Bytes())
	if len(responses) != 3 {
		t.Fatalf("responses = %d, want 3", len(responses))
	}
	byID := map[float64]map[string]any{}
	for _, response := range responses {
		id, ok := response["id"].(float64)
		if !ok {
			t.Fatalf("response has no numeric id: %v", response)
		}
		if _, duplicate := byID[id]; duplicate {
			t.Fatalf("duplicate response for id %v", id)
		}
		byID[id] = response
	}
	body1 := result(t, byID[1])
	if _, ok := body1["content"]; !ok {
		t.Fatalf("id 1 has no content: %v", byID[1])
	}
	result(t, byID[2]) // ping 必须是正常结果（有错误会直接失败）
	body3 := result(t, byID[3])
	text := body3["content"].([]any)[0].(map[string]any)["text"].(string)
	if text != `echo|{"a":1}` {
		t.Fatalf("id 3 content = %q", text)
	}
}
