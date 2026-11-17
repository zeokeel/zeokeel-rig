package mcpserver_test

// 审计：内容正确性（inputSchema 透传、isError 与 RPCError 的边界、非文本 Part 的
// 映射与顺序）。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
)

// TestAuditContent_SchemaPassthroughKeepsRefsAndOneOf 证明深层 schema（$ref /
// definitions / 嵌套 oneOf）在 tools/list 里原样透传，不会被展平或改写。
func TestAuditContent_SchemaPassthroughKeepsRefsAndOneOf(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"args": map[string]any{"$ref": "#/$defs/Args"},
		},
		"$defs": map[string]any{
			"Args": map[string]any{
				"oneOf": []any{
					map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"$ref": "#/$defs/Echo"}}},
					map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				},
			},
			"Echo": map[string]any{"type": "string"},
		},
	}
	server := newServer(t, &fakeTool{name: "rich", schema: schema})
	list := auditResult(t, auditRaw(t, server, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	tools, _ := list["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v, want 1", list["tools"])
	}
	got, err := json.Marshal(tools[0].(map[string]any)["inputSchema"])
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	want, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("inputSchema changed in transit:\ngot  %s\nwant %s", got, want)
	}
}

// TestAuditContent_RefSchemaKeepsNoInjectedType 回归（已修）：只有**既无 type 也
// 无 $ref** 的 schema 才补 `"type":"object"`；$ref schema 原样透传。
//
// 历史：本用例原来是特征化记录——`$ref` schema 会被补 `"type":"object"`。风险是
// JSON Schema 2020-12 里 `$ref` 与 `type` 同层叠加，若 $ref 指向非对象类型（数组），
// 补 type:object 会让 schema 恒不可满足。现在按"期望改法"修掉了，并保留这条用例
// 防止回退。
func TestAuditContent_RefSchemaKeepsNoInjectedType(t *testing.T) {
	server := newServer(t, &fakeTool{
		name:   "ref-only",
		schema: map[string]any{"$ref": "#/$defs/Args", "$defs": map[string]any{"Args": map[string]any{"type": "array"}}},
	})
	list := auditResult(t, auditRaw(t, server, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	schema := list["tools"].([]any)[0].(map[string]any)["inputSchema"].(map[string]any)
	if _, injected := schema["type"]; injected {
		t.Fatalf("$ref schema 不应被注入 type: %v", schema)
	}
	if _, ok := schema["$ref"]; !ok {
		t.Fatalf("schema lost its $ref: %v", schema)
	}

	// 对照：既无 type 也无 $ref 时仍然补 object（保持既有可用性行为）。
	plain := newServer(t, &fakeTool{name: "plain", schema: map[string]any{"properties": map[string]any{}}})
	list = auditResult(t, auditRaw(t, plain, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	schema = list["tools"].([]any)[0].(map[string]any)["inputSchema"].(map[string]any)
	if schema["type"] != "object" {
		t.Fatalf("缺 type 的普通 schema 应补 object，实际 %v", schema["type"])
	}
}

// TestAuditContent_PartsBecomeOrderedTextBlocks 证明 ToolResult.Parts 的顺序被保留，
// 且非文本 Part 的映射方式（image_url → "[image: URL]" 文本块）。
func TestAuditContent_PartsBecomeOrderedTextBlocks(t *testing.T) {
	tool := &fakeTool{
		name:   "parts",
		schema: map[string]any{"type": "object"},
		invoke: func(_ context.Context, _ rig.ToolCall) (rig.ToolResult, error) {
			return rig.ToolResult{
				Content: "main",
				Parts: []rig.ContentPart{
					rig.TextPart("second"),
					rig.ImagePart("https://example.com/a.png", "high"),
					rig.TextPart("third"),
				},
			}, nil
		},
	}
	server := newServer(t, tool)
	result := auditResult(t, auditRaw(t, server, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"parts"}}`))
	blocks, ok := result["content"].([]any)
	if !ok || len(blocks) != 4 {
		t.Fatalf("content blocks = %v, want 4", result["content"])
	}
	wantTexts := []string{"main", "second", "[image: https://example.com/a.png]", "third"}
	for index, want := range wantTexts {
		block := blocks[index].(map[string]any)
		if block["type"] != "text" {
			t.Fatalf("block %d type = %v, want text", index, block["type"])
		}
		if block["text"] != want {
			t.Fatalf("block %d text = %v, want %q", index, block["text"], want)
		}
	}
	// Detail（"high"）在 MCP text 块里没有对应字段：被丢弃。记录该信息损失。
	encoded, _ := json.Marshal(result["content"])
	if strings.Contains(string(encoded), "high") {
		t.Fatalf("image detail leaked into the content blocks: %s", encoded)
	}
}

// TestAuditContent_EmptyContentStillHasOneTextBlock 证明空内容也会产出一个 text 块
// （客户端不会看到空 content 数组）。
func TestAuditContent_EmptyContentStillHasOneTextBlock(t *testing.T) {
	server := newServer(t, &fakeTool{
		name:   "empty",
		schema: map[string]any{"type": "object"},
		invoke: func(_ context.Context, _ rig.ToolCall) (rig.ToolResult, error) {
			return rig.ToolResult{}, nil
		},
	})
	result := auditResult(t, auditRaw(t, server, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"empty"}}`))
	blocks, ok := result["content"].([]any)
	if !ok || len(blocks) != 1 {
		t.Fatalf("content = %v, want exactly one empty text block", result["content"])
	}
	if blocks[0].(map[string]any)["text"] != "" {
		t.Fatalf("block = %v, want empty text", blocks[0])
	}
}

// TestAuditContent_ToolErrorDiscardsParts 记录：工具返回 Go error 时，Parts 与
// IsError 都由 invokeTool 重建（原始 Parts 丢弃）。
//
// 影响：如果工具在"部分成功"时同时给出 error 与 Parts，客户端只能看到 error 文本。
// 期望：这是刻意的最小映射（错误路径不携带附件），但值得在 doc 里写明。
func TestAuditContent_ToolErrorDiscardsParts(t *testing.T) {
	server := newServer(t, &fakeTool{
		name:   "error-with-parts",
		schema: map[string]any{"type": "object"},
		invoke: func(_ context.Context, _ rig.ToolCall) (rig.ToolResult, error) {
			return rig.ToolResult{Content: "partial", Parts: []rig.ContentPart{rig.TextPart("attachment")}}, errFakeToolError
		},
	})
	result := auditResult(t, auditRaw(t, server, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"error-with-parts"}}`))
	if result["isError"] != true {
		t.Fatalf("isError = %v, want true", result["isError"])
	}
	blocks := result["content"].([]any)
	if len(blocks) != 1 || blocks[0].(map[string]any)["text"] != "fake tool failure" {
		t.Fatalf("content = %v, want a single block carrying the Go error text", blocks)
	}
}

// TestAuditContent_NULBytesRoundTrip 证明参数里的 `\u0000` 与工具结果里的 NUL 字节
// 都能往返：响应仍是合法 JSON（NUL 被编码成 \u0000），工具看到的参数原文不变。
func TestAuditContent_NULBytesRoundTrip(t *testing.T) {
	var seenArguments string
	tool := &fakeTool{
		name:   "nul",
		schema: map[string]any{"type": "object"},
		invoke: func(_ context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			seenArguments = call.Arguments
			return rig.ToolResult{Content: "before\x00after"}, nil
		},
	}
	server := newServer(t, tool)
	response, handled, err := server.HandleLine(context.Background(),
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nul","arguments":{"t":"a\u0000b"}}}`))
	if err != nil || !handled {
		t.Fatalf("HandleLine: handled=%v err=%v", handled, err)
	}
	if !json.Valid(response) {
		t.Fatalf("response with NUL bytes is not valid JSON: %q", response)
	}
	var decoded struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := decoded.Result.Content[0].Text; got != "before\x00after" {
		t.Fatalf("content = %q, want the NUL preserved", got)
	}
	// 工具收到的参数是原始 JSON 字节：`\u0000` 保持转义形式（不会被提前解码成 NUL）。
	if !strings.Contains(seenArguments, `\u0000`) {
		t.Fatalf("tool saw arguments %q, want the escaped form preserved", seenArguments)
	}
}

// errFakeToolError 是测试用工具错误。
var errFakeToolError = &fakeToolError{}

type fakeToolError struct{}

func (*fakeToolError) Error() string { return "fake tool failure" }
