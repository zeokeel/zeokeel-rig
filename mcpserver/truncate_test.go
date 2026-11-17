package mcpserver_test

// tools/call 结果上限（WithMaxResultBytes）的线级测试：截断必须是"正常结果 +
// 截断标记"，绝不能被报成 JSON-RPC error，也不能静默丢内容。

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/mcpserver"
)

// bigTool 返回固定长度的内容，便于精确卡在上限边界上。
func bigTool(name string, payload string) *fakeTool {
	return &fakeTool{
		name:        name,
		description: "返回固定长度内容",
		schema:      map[string]any{"type": "object"},
		invoke: func(_ context.Context, _ rig.ToolCall) (rig.ToolResult, error) {
			return rig.ToolResult{Content: payload}, nil
		},
	}
}

// callContent 发一条 tools/call 并把 content 块解码成 []map[string]any。
func callContent(t *testing.T, server *mcpserver.Server, toolName string) []map[string]any {
	t.Helper()
	request := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q}}`, toolName)
	body := result(t, call(t, server, request))
	raw, ok := body["content"].([]any)
	if !ok {
		t.Fatalf("content = %v", body["content"])
	}
	blocks := make([]map[string]any, 0, len(raw))
	for _, entry := range raw {
		block, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("content block = %v", entry)
		}
		blocks = append(blocks, block)
	}
	return blocks
}

// omittedCount 从截断注释里解析出"省略了多少字节"。
var omittedPattern = regexp.MustCompile(`\[truncated: (\d+) bytes omitted\]`)

func omittedCount(t *testing.T, text string) int {
	t.Helper()
	match := omittedPattern.FindStringSubmatch(text)
	if match == nil {
		t.Fatalf("content has no truncation note: %q", tail(text, 80))
	}
	count, err := strconv.Atoi(match[1])
	if err != nil {
		t.Fatalf("truncated count %q is not a number: %v", match[1], err)
	}
	return count
}

func tail(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[len(text)-limit:]
}

func TestMaxResultBytesNoTruncationWhenExactlyAtLimit(t *testing.T) {
	const limit = 64
	server, err := mcpserver.New([]rig.Tool{bigTool("exact", strings.Repeat("a", limit))},
		mcpserver.WithMaxResultBytes(limit))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	blocks := callContent(t, server, "exact")
	if len(blocks) != 1 {
		t.Fatalf("blocks = %v", blocks)
	}
	if blocks[0]["text"] != strings.Repeat("a", limit) {
		t.Fatalf("content was modified at the exact limit: %q", blocks[0]["text"])
	}
	if _, ok := blocks[0]["_meta"]; ok {
		t.Fatalf("_meta present without truncation: %v", blocks[0])
	}
}

func TestMaxResultBytesTruncatesOneByteOver(t *testing.T) {
	const limit = 64
	payload := strings.Repeat("a", limit+1)
	server, err := mcpserver.New([]rig.Tool{bigTool("over", payload)},
		mcpserver.WithMaxResultBytes(limit))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	blocks := callContent(t, server, "over")
	if len(blocks) != 1 {
		t.Fatalf("blocks = %v", blocks)
	}
	text := blocks[0]["text"].(string)
	if !strings.HasPrefix(text, "a") {
		t.Fatalf("text was not truncated at the front: %q", text)
	}
	if !strings.Contains(text, "[truncated: ") {
		t.Fatalf("text has no truncation note: %q", text)
	}
	omitted := omittedCount(t, text)
	kept := strings.Count(text[:strings.Index(text, "\n…[truncated:")], "a")
	if omitted != len(payload)-kept {
		t.Fatalf("note says %d bytes omitted, but %d were actually dropped", omitted, len(payload)-kept)
	}
	meta, ok := blocks[0]["_meta"].(map[string]any)
	if !ok {
		t.Fatalf("_meta = %v, want {\"truncated\":true}", blocks[0]["_meta"])
	}
	if meta["truncated"] != true {
		t.Fatalf("_meta = %v, want truncated=true", meta)
	}
	if body := result(t, call(t, server, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"over"}}`)); body["isError"] != nil && body["isError"] != false {
		t.Fatalf("truncation must not be isError: %v", body)
	}
}

func TestMaxResultBytesTruncatesTheLongestBlockOnly(t *testing.T) {
	// 两个内容块：长的那个被截断，短的原样保留。
	limit := mcpserver.DefaultMaxResultBytes
	tool := &fakeTool{
		name:   "twoblocks",
		schema: map[string]any{"type": "object"},
		invoke: func(_ context.Context, _ rig.ToolCall) (rig.ToolResult, error) {
			return rig.ToolResult{
				Content: strings.Repeat("L", limit),
				Parts:   []rig.ContentPart{rig.TextPart("短块")},
			}, nil
		},
	}
	server, err := mcpserver.New([]rig.Tool{tool}, mcpserver.WithMaxResultBytes(limit))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	blocks := callContent(t, server, "twoblocks")
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d, want 2", len(blocks))
	}
	if blocks[1]["text"] != "短块" {
		t.Fatalf("short block was modified: %v", blocks[1])
	}
	if _, ok := blocks[1]["_meta"]; ok {
		t.Fatalf("short block carries _meta: %v", blocks[1])
	}
	if !strings.Contains(blocks[0]["text"].(string), "[truncated: ") {
		t.Fatalf("long block was not truncated")
	}
	if _, ok := blocks[0]["_meta"]; !ok {
		t.Fatalf("long block has no _meta: %v", blocks[0])
	}
}

func TestMaxResultBytesUnlimitedWithMinusOne(t *testing.T) {
	// -1 显式表示"不限"：即使远超默认上限也原样透传。
	payload := strings.Repeat("z", mcpserver.DefaultMaxResultBytes+1024)
	server, err := mcpserver.New([]rig.Tool{bigTool("huge", payload)},
		mcpserver.WithMaxResultBytes(-1))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	blocks := callContent(t, server, "huge")
	if blocks[0]["text"] != payload {
		t.Fatalf("payload was truncated with -1 (len %d, want %d)", len(blocks[0]["text"].(string)), len(payload))
	}
	if _, ok := blocks[0]["_meta"]; ok {
		t.Fatalf("_meta present with -1: %v", blocks[0])
	}
}

func TestMaxResultBytesZeroFallsBackToDefault(t *testing.T) {
	// 0 不是"不限"：必须回落到默认上限（否则一个参数拼错就把保护关掉了）。
	payload := strings.Repeat("y", mcpserver.DefaultMaxResultBytes+4096)
	server, err := mcpserver.New([]rig.Tool{bigTool("big", payload)},
		mcpserver.WithMaxResultBytes(0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	blocks := callContent(t, server, "big")
	text := blocks[0]["text"].(string)
	if !strings.Contains(text, "[truncated: ") {
		t.Fatalf("WithMaxResultBytes(0) disabled the default limit")
	}
	if len(text) > mcpserver.DefaultMaxResultBytes {
		t.Fatalf("truncated text is %d bytes, want <= %d", len(text), mcpserver.DefaultMaxResultBytes)
	}
}

func TestMaxResultBytesDefaultLimitApplies(t *testing.T) {
	// 不传 Option 也必须有 8 MiB 的保护。
	payload := strings.Repeat("d", mcpserver.DefaultMaxResultBytes+1)
	server, err := mcpserver.New([]rig.Tool{bigTool("dflt", payload)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	blocks := callContent(t, server, "dflt")
	if !strings.Contains(blocks[0]["text"].(string), "[truncated: ") {
		t.Fatal("default limit did not apply")
	}
}

func TestMaxResultBytesTruncatesErrorResultsToo(t *testing.T) {
	// isError 结果同样受限（否则一个失败的巨型输出就能撑爆会话）。
	payload := strings.Repeat("e", 4096)
	tool := &fakeTool{
		name:   "bignfail",
		schema: map[string]any{"type": "object"},
		invoke: func(_ context.Context, _ rig.ToolCall) (rig.ToolResult, error) {
			return rig.ToolResult{Content: payload, IsError: true}, nil
		},
	}
	server, err := mcpserver.New([]rig.Tool{tool}, mcpserver.WithMaxResultBytes(128))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	body := result(t, call(t, server, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"bignfail"}}`))
	if body["isError"] != true {
		t.Fatalf("isError = %v, want true", body["isError"])
	}
	blocks := body["content"].([]any)
	text := blocks[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "[truncated: ") {
		t.Fatal("error result was not truncated")
	}
}

func TestMaxResultBytesSmallLimitStillProducesValidJSON(t *testing.T) {
	// 上限小到只够放注释时也不能panic、不能产出坏 JSON。
	server, err := mcpserver.New([]rig.Tool{bigTool("tiny", strings.Repeat("中", 500))},
		mcpserver.WithMaxResultBytes(1))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	blocks := callContent(t, server, "tiny")
	text := blocks[0]["text"].(string)
	if text == "" {
		t.Fatal("content became empty")
	}
	if !strings.Contains(text, "[truncated: ") {
		t.Fatalf("text = %q", text)
	}
	// 不允许出现半个 rune（JSON 会把它替换成 U+FFFD）。
	if strings.ContainsRune(text, '\uFFFD') {
		t.Fatalf("text contains a replacement rune: %q", text)
	}
}

// 默认上限必须严格小于客户端读取上限（tools/mcp 的 bufio.Scanner 上界 8 MiB）：
// 服务端写出比它更大的行 = 真实客户端直接报 "token too long"、连接不可用。
func TestDefaultMaxResultBytesStaysBelowClientReadLimit(t *testing.T) {
	if mcpserver.DefaultMaxResultBytes >= mcpserver.ClientReadLimit {
		t.Fatalf("DefaultMaxResultBytes (%d) must stay below ClientReadLimit (%d)",
			mcpserver.DefaultMaxResultBytes, mcpserver.ClientReadLimit)
	}
	if mcpserver.DefaultMaxResultBytes > mcpserver.ClientReadLimit/2 {
		t.Fatalf("DefaultMaxResultBytes (%d) leaves no headroom for the JSON envelope/escaping (limit %d)",
			mcpserver.DefaultMaxResultBytes, mcpserver.ClientReadLimit)
	}
}

// 转义会把内容撑大（`"` 翻倍、控制符 6 倍）：即使内容上限被关掉（-1），编码后的
// 响应行也必须小于客户端读取上限。
func TestEscapedContentStaysUnderClientReadLimit(t *testing.T) {
	payload := strings.Repeat(`"`, 5<<20) // 5 MiB 的引号 → 转义后 ≈ 10 MiB
	server, err := mcpserver.New([]rig.Tool{bigTool("quotes", payload)},
		mcpserver.WithMaxResultBytes(mcpserver.MaxResultBytesUnlimited))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	response, handled, err := server.HandleLine(context.Background(),
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"quotes"}}`))
	if err != nil {
		t.Fatalf("HandleLine: %v", err)
	}
	if !handled {
		t.Fatal("request was not handled")
	}
	if len(response) >= mcpserver.ClientReadLimit {
		t.Fatalf("response line is %d bytes, must stay under the client read limit %d",
			len(response), mcpserver.ClientReadLimit)
	}
	var decoded map[string]any
	if err := json.Unmarshal(response, &decoded); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if _, ok := decoded["error"]; ok {
		t.Fatalf("truncation turned into a JSON-RPC error: %v", decoded["error"])
	}
	body := result(t, decoded)
	block := body["content"].([]any)[0].(map[string]any)
	if !strings.Contains(block["text"].(string), "[truncated: ") {
		t.Fatalf("escaped content was not truncated: %v", block["text"].(string)[:80])
	}
	if meta, ok := block["_meta"].(map[string]any); !ok || meta["truncated"] != true {
		t.Fatalf("_meta = %v, want truncated=true", block["_meta"])
	}
}

// 控制符是最坏情况（每个字符 6 字节）：同样不能穿透读取上限。
func TestControlCharContentStaysUnderClientReadLimit(t *testing.T) {
	payload := strings.Repeat("\x01", 3<<20) // 3 MiB 控制符 → 转义后 18 MiB
	server, err := mcpserver.New([]rig.Tool{bigTool("ctrl", payload)},
		mcpserver.WithMaxResultBytes(mcpserver.MaxResultBytesUnlimited))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	response, _, err := server.HandleLine(context.Background(),
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"ctrl"}}`))
	if err != nil {
		t.Fatalf("HandleLine: %v", err)
	}
	if len(response) >= mcpserver.ClientReadLimit {
		t.Fatalf("response line is %d bytes, must stay under %d", len(response), mcpserver.ClientReadLimit)
	}
	var decoded map[string]any
	if err := json.Unmarshal(response, &decoded); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
}
