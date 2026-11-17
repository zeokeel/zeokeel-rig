package mcpserver_test

// 审计：协议层对抗（JSON-RPC 2.0 结构与 MCP 握手顺序）。
//
// 这些用例区分两类结论：
//
//   - 通过 = 当前实现对畸形输入的处理被观察到并且是安全的（记为"宽松但安全"的
//     特征化断言，日后收紧时会在这里变红，提醒更新文档）；
//   - 失败 = 发现的缺陷（以 TestAuditFinding_ 前缀标记，报告里有级别与期望改法）。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/mcpserver"
)

// auditRaw 发送一行原始文本并返回解析后的响应（handled=false 时返回 nil）。
func auditRaw(t *testing.T, server *mcpserver.Server, line string) map[string]any {
	t.Helper()
	response, handled, err := server.HandleLine(context.Background(), []byte(line))
	if err != nil {
		t.Fatalf("HandleLine(%q) 返回 Go 错误: %v", line, err)
	}
	if !handled {
		return nil
	}
	var decoded map[string]any
	if err := json.Unmarshal(response, &decoded); err != nil {
		t.Fatalf("响应不是合法 JSON: %v (%s)", err, response)
	}
	return decoded
}

// TestAuditProtocol_RequestsAcceptedBeforeInitialize 特征化：server 不维护会话状态，
// 未 initialize 的 tools/call 也会执行。
//
// 结论：宽松但安全（无状态服务器没有可"提前使用"的初始化状态）。MCP 要求客户端先
// initialize；若要与严格客户端对齐，可以加一个"未握手只允许 initialize/ping"的门，
// 但会让 tools/mcp 的 httptransport 重连路径（它总会握手）之外的调试更麻烦。
func TestAuditProtocol_RequestsAcceptedBeforeInitialize(t *testing.T) {
	server := newServer(t, echoTool("echo"))
	response := auditRaw(t, server, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{}}}`)
	if response == nil {
		t.Fatal("tools/call before initialize produced no response")
	}
	if code := auditErrorCode(response); code != 0 {
		t.Fatalf("tools/call before initialize returned error %d: %v", code, response)
	}
	result := auditResult(t, response)
	if content, ok := result["content"].([]any); !ok || len(content) == 0 {
		t.Fatalf("result = %v, want a content block", result)
	}
}

// TestAuditProtocol_InitializeIsIdempotent 特征化：重复 initialize、以及 initialized
// 通知之后再 initialize 都被接受（无状态，不回 -32600）。
func TestAuditProtocol_InitializeIsIdempotent(t *testing.T) {
	server := newServer(t, echoTool("echo"))
	first := auditRaw(t, server, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	second := auditRaw(t, server, `{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	if auditErrorCode(first) != 0 || auditErrorCode(second) != 0 {
		t.Fatalf("repeated initialize rejected: first=%v second=%v", first, second)
	}
	if got := auditResult(t, second)["protocolVersion"]; got != "2025-06-18" {
		t.Fatalf("second initialize protocolVersion = %v", got)
	}
	// notifications/initialized 是通知：不应产生输出。
	if response := auditRaw(t, server, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); response != nil {
		t.Fatalf("initialized notification produced a response: %v", response)
	}
	if response := auditRaw(t, server, `{"jsonrpc":"2.0","id":3,"method":"initialize","params":{}}`); auditErrorCode(response) != 0 {
		t.Fatalf("initialize after initialized rejected: %v", response)
	}
}

// TestAuditProtocol_JsonrpcFieldIsNotValidated 特征化：`jsonrpc` 缺失或值错误时仍执行。
//
// 结论：偏离 JSON-RPC 2.0（服务器应对非法请求回 -32600）。实际风险低（MCP 客户端
// 都发 "2.0"），但如果将来要对接严格代理，这里是"静默接受畸形请求"的入口。
// 期望改法：parseRequest 校验 request.JSONRPC == "2.0"，否则 -32600。
func TestAuditProtocol_JsonrpcFieldIsNotValidated(t *testing.T) {
	server := newServer(t, echoTool("echo"))
	for _, line := range []string{
		`{"id":1,"method":"ping"}`,
		`{"jsonrpc":"1.0","id":2,"method":"ping"}`,
		`{"jsonrpc":"2.0 ","id":3,"method":"ping"}`,
	} {
		response := auditRaw(t, server, line)
		if response == nil {
			t.Fatalf("%s: no response", line)
		}
		// 现状：缺失/错误的 jsonrpc 被当作合法请求执行（code 0）。
		if code := auditErrorCode(response); code != 0 {
			t.Logf("%s -> error %d %v", line, code, response)
		}
	}
	// 数字类型的 jsonrpc 会在解码期失败，被归类成 parse error（-32700）而不是
	// invalid request（-32600）：错误码分类不精确，但仍然是"有应答"。
	response := auditRaw(t, server, `{"jsonrpc":2,"id":4,"method":"ping"}`)
	if code := auditErrorCode(response); code != mcpserver.CodeParseError {
		t.Fatalf("numeric jsonrpc -> code %d, want %d (current behavior)", code, mcpserver.CodeParseError)
	}
}

// TestAuditProtocol_NonScalarRequestIDsAreEchoed 特征化：object/array/bool 类型的 id
// 会被原样回显。
//
// 结论：偏离 JSON-RPC 2.0（id 只允许 string/number/null）。这样的响应会让严格客户端
// 无法把响应关联到请求（我们的 tools/mcp 用 uint64 解析，会直接把这类行丢掉）。
// 期望改法：parseRequest 里校验 id 是 string/number/null，否则回 -32600 且 id=null。
func TestAuditProtocol_NonScalarRequestIDsAreEchoed(t *testing.T) {
	server := newServer(t, echoTool("echo"))
	cases := []string{`{"a":1}`, `[1,2]`, `true`}
	for _, rawID := range cases {
		line := `{"jsonrpc":"2.0","id":` + rawID + `,"method":"ping"}`
		response := auditRaw(t, server, line)
		if response == nil {
			t.Fatalf("id=%s: no response", rawID)
		}
		echoed, err := json.Marshal(response["id"])
		if err != nil {
			t.Fatalf("encode id: %v", err)
		}
		var want any
		if err := json.Unmarshal([]byte(rawID), &want); err != nil {
			t.Fatalf("decode raw id: %v", err)
		}
		wantEncoded, _ := json.Marshal(want)
		if string(echoed) != string(wantEncoded) {
			t.Fatalf("id=%s echoed as %s, want %s", rawID, echoed, wantEncoded)
		}
	}
}

// TestAuditProtocol_NullIDRequestGetsNoResponse 特征化：显式 id:null 被当作通知，
// 不产生输出（JSON-RPC 建议这样处理"无法应答的请求"）。
func TestAuditProtocol_NullIDRequestGetsNoResponse(t *testing.T) {
	server := newServer(t, echoTool("echo"))
	if response := auditRaw(t, server, `{"jsonrpc":"2.0","id":null,"method":"ping"}`); response != nil {
		t.Fatalf("null id produced a response: %v", response)
	}
}

// TestAuditProtocol_MethodEdgeCases 覆盖空方法、NUL 方法与超长方法。
func TestAuditProtocol_MethodEdgeCases(t *testing.T) {
	server := newServer(t, echoTool("echo"))

	t.Run("empty and null method are invalid requests", func(t *testing.T) {
		for _, line := range []string{
			`{"jsonrpc":"2.0","id":1,"method":""}`,
			`{"jsonrpc":"2.0","id":2,"method":null}`,
		} {
			if code := auditErrorCode(auditRaw(t, server, line)); code != mcpserver.CodeInvalidRequest {
				t.Fatalf("%s -> code %d, want %d", line, code, mcpserver.CodeInvalidRequest)
			}
		}
	})

	t.Run("method with NUL byte is a clean method-not-found", func(t *testing.T) {
		response := auditRaw(t, server, `{"jsonrpc":"2.0","id":3,"method":"bad\u0000name"}`)
		if code := auditErrorCode(response); code != mcpserver.CodeMethodNotFound {
			t.Fatalf("code = %d, want %d", code, mcpserver.CodeMethodNotFound)
		}
		message := response["error"].(map[string]any)["message"].(string)
		if !strings.Contains(message, "bad\x00name") {
			t.Fatalf("error message = %q, want the method echoed verbatim", message)
		}
	})

	t.Run("100KB method is echoed once, response stays bounded", func(t *testing.T) {
		method := strings.Repeat("m", 100<<10)
		line := `{"jsonrpc":"2.0","id":4,"method":"` + method + `"}`
		response := auditRaw(t, server, line)
		if code := auditErrorCode(response); code != mcpserver.CodeMethodNotFound {
			t.Fatalf("code = %d, want %d", code, mcpserver.CodeMethodNotFound)
		}
		// 响应里回显方法名：amplification 约 1x（不超过入站行 + 错误对象开销）。
		encoded, err := json.Marshal(response)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if len(encoded) > len(line)+1024 {
			t.Fatalf("response %d bytes for a %d-byte request", len(encoded), len(line))
		}
	})
}

// TestAuditProtocol_ParamsShapeHandling 覆盖 params 为数组/标量时的处理。
func TestAuditProtocol_ParamsShapeHandling(t *testing.T) {
	server := newServer(t, echoTool("echo"))

	// tools/call 的 params 必须是对象：数组 → -32602。
	response := auditRaw(t, server, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":[1,2]}`)
	if code := auditErrorCode(response); code != mcpserver.CodeInvalidParams {
		t.Fatalf("array params for tools/call -> code %d, want %d", code, mcpserver.CodeInvalidParams)
	}
	// 错误文案里带 Go 类型名（mcpserver.callParams）：对模型可读性无影响，但泄露内部
	// 类型名；期望改法：改成 "params must be an object"。
	message := response["error"].(map[string]any)["message"].(string)
	if !strings.Contains(message, "mcpserver.callParams") {
		t.Logf("params error message no longer leaks the Go type name: %q", message)
	}

	// ping/tools/list 的 params 被忽略（数组也接受）。
	for _, line := range []string{
		`{"jsonrpc":"2.0","id":2,"method":"ping","params":[1,2]}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/list","params":[1,2]}`,
	} {
		if code := auditErrorCode(auditRaw(t, server, line)); code != 0 {
			t.Fatalf("%s -> code %d, want 0", line, code)
		}
	}
}

// TestAuditProtocol_DuplicateJSONKeysLastWins 特征化：重复的 JSON 键按 Go 的
// "后出现的覆盖先出现的"处理。恶意客户端无法借此"先声明 A 再执行 B"——server 只看
// 解码后的值，因此行为确定；但对严格实现来说这是未定义输入，值得记录。
func TestAuditProtocol_DuplicateJSONKeysLastWins(t *testing.T) {
	server := newServer(t, echoTool("echo"))
	response := auditRaw(t, server, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope","name":"echo","arguments":{}}}`)
	if code := auditErrorCode(response); code != 0 {
		t.Fatalf("duplicate name keys -> code %d, want 0 (last wins)", code)
	}
	text := auditResult(t, response)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.HasPrefix(text, "echo|") {
		t.Fatalf("executed tool produced %q, want echo|…", text)
	}
}

// TestAuditProtocol_CancelledRequestStillGetsResult 记录一次违反 MCP 建议的行为：
// 被取消的调用仍然写出完整结果（isError=true）。
//
// MCP 2025-06-18 的 cancellation 约定是"接收方不应为被取消的请求发送响应"。我们的
// tools/mcp 客户端会按 id 丢弃迟到响应（不影响它），但严格客户端可能把未知 id 的
// 响应当成协议错误。期望改法：request 上下文被取消时跳过写出（或在 inflight 里标记
// "已取消"并在 dispatch 写完前检查）。
func TestAuditProtocol_CancelledRequestStillGetsResult(t *testing.T) {
	server := newServer(t, echoTool("echo"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// 直接取消再调用：模拟"请求已被取消"（Serve 路径下由 inflight 取消触发）。
	response, handled, err := server.HandleLine(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo"}}`))
	if err != nil {
		t.Fatalf("HandleLine: %v", err)
	}
	if !handled || response == nil {
		t.Fatal("cancelled call produced no response")
	}
	t.Logf("cancelled call response: %s", strings.TrimSpace(string(response)))
}

// TestAuditProtocol_DuplicateIDConcurrentCallsBothAnswered 特征化：同一个 id 的两个
// 并发请求都会被执行并写出响应（两条响应的 id 相同）。
//
// 影响：JSON-RPC 要求调用方保证 id 唯一；服务端无法替它去重（两个请求都已经在跑）。
// 现实风险是"响应串线"——严格客户端会把第二条响应当成未知 id 丢弃。若要在服务端
// 兜底，可以在 registerInflight 发现同 id 在途时回 -32600（明确拒绝），而不是让两个
// 调用都跑完。
func TestAuditProtocol_DuplicateIDConcurrentCallsBothAnswered(t *testing.T) {
	bothStarted := make(chan struct{}, 2)
	release := make(chan struct{})
	tool := &fakeTool{
		name:   "wait",
		schema: map[string]any{"type": "object"},
		invoke: func(context.Context, rig.ToolCall) (rig.ToolResult, error) {
			bothStarted <- struct{}{}
			<-release
			return rig.ToolResult{Content: "done"}, nil
		},
	}
	server := newServer(t, tool)
	input := strings.NewReader(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"wait"}}` + "\n" +
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"wait"}}` + "\n")
	var out lockedBuilder
	done := make(chan error, 1)
	go func() { done <- server.Serve(context.Background(), input, &out) }()

	for started := 0; started < 2; started++ {
		select {
		case <-bothStarted:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of 2 duplicate-id calls started", started)
		}
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return")
	}
	lines := nonEmptyLines(out.String())
	if len(lines) != 2 {
		t.Fatalf("responses = %d, want 2 (both duplicate-id calls answered)", len(lines))
	}
	for _, line := range lines {
		if !strings.Contains(line, `"id":7`) {
			t.Fatalf("response lost the shared id: %s", truncateForLog(line))
		}
	}
}

// auditErrorCode 返回 JSON-RPC 错误码（无错误返回 0）。
func auditErrorCode(response map[string]any) int {
	if response == nil {
		return 0
	}
	errObject, ok := response["error"].(map[string]any)
	if !ok {
		return 0
	}
	code, ok := errObject["code"].(float64)
	if !ok {
		return 0
	}
	return int(code)
}

// auditResult 返回响应的 result 对象（无 result 时失败）。
func auditResult(t *testing.T, response map[string]any) map[string]any {
	t.Helper()
	raw, ok := response["result"].(map[string]any)
	if !ok {
		t.Fatalf("response has no result: %v", response)
	}
	return raw
}
