package mcpserver

// 本文件是"可选严格协议"与命名/schema 校验的内部测试。内部包是为了直接替换
// 告警输出（warnOut）并读取 strict 状态，不必为了测试把内部字段导出。

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
)

// stubTool 是测试用最小工具。
type stubTool struct {
	name   string
	schema map[string]any
}

func (t stubTool) Name() string           { return t.name }
func (t stubTool) Description() string    { return "stub" }
func (t stubTool) Schema() map[string]any { return t.schema }
func (t stubTool) Invoke(context.Context, rig.ToolCall) (rig.ToolResult, error) {
	return rig.ToolResult{Content: "ok"}, nil
}

// rpcEnvelope 是响应行（够用的子集）。
type rpcEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *wireError      `json:"error"`
}

// callHandleLine 驱动一行输入并解码响应。
func callHandleLine(t *testing.T, s *Server, line string) rpcEnvelope {
	t.Helper()
	payload, handled, err := s.HandleLine(context.Background(), []byte(line))
	if err != nil {
		t.Fatalf("HandleLine(%s): %v", line, err)
	}
	if !handled || len(payload) == 0 {
		t.Fatalf("HandleLine(%s) 没有产生应答（通知？）", line)
	}
	var env rpcEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		t.Fatalf("响应不是合法 JSON: %v (%s)", err, payload)
	}
	return env
}

// 严格模式：jsonrpc 必须是 "2.0"，否则 -32600；宽松模式保持兼容。
func TestStrictProtocolRequiresJSONRPC20(t *testing.T) {
	lines := []string{
		`{"id":1,"method":"ping"}`,                  // 缺 jsonrpc
		`{"jsonrpc":"1.0","id":1,"method":"ping"}`,  // 旧版本
		`{"jsonrpc":"2.0 ","id":1,"method":"ping"}`, // 尾随空格
	}
	for _, line := range lines {
		strict, err := New(nil, WithStrictProtocol(true))
		if err != nil {
			t.Fatalf("New(strict): %v", err)
		}
		env := callHandleLine(t, strict, line)
		if env.Error == nil || env.Error.Code != CodeInvalidRequest {
			t.Fatalf("严格模式下 %s 应当 -32600，实际 %+v", line, env)
		}

		lax, err := New(nil)
		if err != nil {
			t.Fatalf("New(lax): %v", err)
		}
		if env := callHandleLine(t, lax, line); env.Error != nil {
			t.Fatalf("宽松模式下 %s 不应报协议错：%+v", line, env.Error)
		}
	}
}

// 严格模式：id 只能是 string/number/null；对象/数组/布尔 → -32600。
func TestStrictProtocolRejectsCompositeID(t *testing.T) {
	bad := []string{
		`{"jsonrpc":"2.0","id":{"a":1},"method":"ping"}`,
		`{"jsonrpc":"2.0","id":[1,2],"method":"ping"}`,
		`{"jsonrpc":"2.0","id":true,"method":"ping"}`,
	}
	for _, line := range bad {
		strict, _ := New(nil, WithStrictProtocol(true))
		if env := callHandleLine(t, strict, line); env.Error == nil || env.Error.Code != CodeInvalidRequest {
			t.Fatalf("严格模式下 %s 应当 -32600，实际 %+v", line, env)
		}
		lax, _ := New(nil)
		if env := callHandleLine(t, lax, line); env.Error != nil {
			t.Fatalf("宽松模式下 %s 不应报协议错：%+v", line, env.Error)
		}
	}
	// 合法 id 形态在两种模式下都通过。
	for _, ok := range []string{
		`{"jsonrpc":"2.0","id":7,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":"abc","method":"ping"}`,
		`{"jsonrpc":"2.0","id":1.5,"method":"ping"}`,
	} {
		strict, _ := New(nil, WithStrictProtocol(true))
		if env := callHandleLine(t, strict, ok); env.Error != nil {
			t.Fatalf("严格模式下合法 id 被拒：%s → %+v", ok, env.Error)
		}
	}
}

// 严格模式：未 initialize 直接 tools/call → -32600；initialize 之后放行。
func TestStrictProtocolRequiresInitializeBeforeToolsCall(t *testing.T) {
	call := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"t","arguments":{}}}`

	strict, err := New([]rig.Tool{stubTool{name: "t"}}, WithStrictProtocol(true))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if env := callHandleLine(t, strict, call); env.Error == nil || env.Error.Code != CodeInvalidRequest {
		t.Fatalf("initialize 之前 tools/call 应当 -32600，实际 %+v", env)
	}
	if env := callHandleLine(t, strict, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`); env.Error != nil {
		t.Fatalf("initialize 失败: %+v", env.Error)
	}
	if env := callHandleLine(t, strict, call); env.Error != nil {
		t.Fatalf("initialize 之后 tools/call 仍被拒: %+v", env.Error)
	}

	// 宽松模式：initialize 之前调用 tools/call 保持原行为（可用）。
	lax, _ := New([]rig.Tool{stubTool{name: "t"}})
	if env := callHandleLine(t, lax, call); env.Error != nil {
		t.Fatalf("宽松模式下 initialize 之前 tools/call 应保持可用: %+v", env.Error)
	}
}

// 严格模式：重复 initialize → -32600。
func TestStrictProtocolRejectsDuplicateInitialize(t *testing.T) {
	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
	strict, _ := New(nil, WithStrictProtocol(true))
	if env := callHandleLine(t, strict, init); env.Error != nil {
		t.Fatalf("第一次 initialize 失败: %+v", env.Error)
	}
	if env := callHandleLine(t, strict, init); env.Error == nil || env.Error.Code != CodeInvalidRequest {
		t.Fatalf("重复 initialize 应当 -32600，实际 %+v", env)
	}

	lax, _ := New(nil)
	if env := callHandleLine(t, lax, init); env.Error != nil {
		t.Fatalf("宽松模式第一次 initialize: %+v", env.Error)
	}
	if env := callHandleLine(t, lax, init); env.Error != nil {
		t.Fatalf("宽松模式重复 initialize 应保持兼容: %+v", env.Error)
	}
}

// 工具名校验：默认只告警（不破坏已有宿主），严格模式报错。
func TestToolNameValidationWarnsByDefaultErrorsInStrict(t *testing.T) {
	var warn bytes.Buffer
	server, err := New(
		[]rig.Tool{stubTool{name: "bad name!"}},
		func(s *Server) { s.warnOut = &warn },
	)
	if err != nil {
		t.Fatalf("默认模式不应因为名字不合规报错: %v", err)
	}
	if server == nil {
		t.Fatal("New 返回 nil server")
	}
	if !strings.Contains(warn.String(), "bad name!") {
		t.Fatalf("默认模式应当告警，实际告警输出 = %q", warn.String())
	}

	if _, err := New([]rig.Tool{stubTool{name: "bad name!"}}, WithStrictProtocol(true)); err == nil {
		t.Fatal("严格模式下不合规工具名应当报错")
	}
	// 合规名字：严格模式不报错、也不告警。
	var okWarn bytes.Buffer
	if _, err := New(
		[]rig.Tool{stubTool{name: "read_file-2"}},
		WithStrictProtocol(true),
		func(s *Server) { s.warnOut = &okWarn },
	); err != nil {
		t.Fatalf("严格模式下合规名字被拒: %v", err)
	}
	if okWarn.Len() != 0 {
		t.Fatalf("合规名字不应告警: %q", okWarn.String())
	}
	// 超过 64 字符的名字在严格模式下报错。
	if _, err := New([]rig.Tool{stubTool{name: strings.Repeat("a", 65)}}, WithStrictProtocol(true)); err == nil {
		t.Fatal("严格模式下超长工具名应当报错")
	}
}

// toolSchema：只有既无 type 也无 $ref 时才补 type。
func TestToolSchemaKeepsRef(t *testing.T) {
	ref := map[string]any{"$ref": "#/$defs/Arguments"}
	got := toolSchema(stubTool{name: "t", schema: ref})
	if _, hasType := got["type"]; hasType {
		t.Fatalf("$ref schema 被强行加了 type: %+v", got)
	}
	if got["$ref"] != "#/$defs/Arguments" {
		t.Fatalf("$ref 被改写: %+v", got)
	}

	withType := map[string]any{"type": "array"}
	if got := toolSchema(stubTool{name: "t", schema: withType}); got["type"] != "array" {
		t.Fatalf("已有 type 被覆盖: %+v", got)
	}

	bare := map[string]any{"properties": map[string]any{}}
	patched := toolSchema(stubTool{name: "t", schema: bare})
	if patched["type"] != "object" {
		t.Fatalf("缺 type 的 schema 应补 object: %+v", patched)
	}
	if _, mutated := bare["type"]; mutated {
		t.Fatal("toolSchema 改写了工具自己的 schema map（应复制）")
	}
	if got := toolSchema(stubTool{name: "t"}); got["type"] != "object" {
		t.Fatalf("nil schema 应返回 {\"type\":\"object\"}: %+v", got)
	}
}
