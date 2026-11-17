package mcpserver_test

// 审计：资源与 DoS 边界（行上限、巨量结果、并发、写失败、EOF 语义）。
//
// TestAuditFinding_ 前缀的用例记录已确认缺陷（会失败，报告里有级别与改法）；
// 其余是安全的特征化断言。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/mcpserver"
	"github.com/zeokeel/zeokeel-rig/tools/mcp"
)

// lockedBuilder 是并发安全的输出缓冲（Serve 会在多个 goroutine 里写）。
type lockedBuilder struct {
	mu     sync.Mutex
	buffer strings.Builder
}

func (b *lockedBuilder) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *lockedBuilder) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

// TestAuditResource_LineLimitBoundary 钉住 1 MiB 行上限的精确边界：
// 内容正好 1 MiB 被接受；多 1 字节回 -32700；两种情况下流都继续可用。
func TestAuditResource_LineLimitBoundary(t *testing.T) {
	server := newServer(t, echoTool("echo"))

	// template 里 arguments 的字符串被填充到目标内容长度（不含行尾换行）。
	build := func(contentBytes int) string {
		template := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"t":""}}}`
		padding := contentBytes - len(template)
		if padding < 0 {
			t.Fatalf("template is longer than the target %d", contentBytes)
		}
		return strings.Replace(template, `"t":""`, `"t":"`+strings.Repeat("x", padding)+`"`, 1)
	}

	// 正好 1 MiB：接受。
	accepted := build(mcpserver.MaxLineBytes)
	var out lockedBuilder
	input := strings.NewReader(accepted + "\n" + `{"jsonrpc":"2.0","id":2,"method":"ping"}` + "\n")
	if err := server.Serve(context.Background(), input, &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	lines := nonEmptyLines(out.String())
	if len(lines) != 2 {
		t.Fatalf("responses = %d, want 2 (1MiB request + ping)", len(lines))
	}
	if !strings.Contains(lines[0], `"result"`) {
		t.Fatalf("1MiB request was not processed: %s", truncateForLog(lines[0]))
	}

	// 1 MiB + 1：-32700，且后续请求仍被服务。
	rejected := build(mcpserver.MaxLineBytes + 1)
	out = lockedBuilder{}
	input = strings.NewReader(rejected + "\n" + `{"jsonrpc":"2.0","id":3,"method":"ping"}` + "\n")
	if err := server.Serve(context.Background(), input, &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	lines = nonEmptyLines(out.String())
	if len(lines) != 2 {
		t.Fatalf("responses = %d, want 2 (parse error + ping)", len(lines))
	}
	if !strings.Contains(lines[0], `-32700`) {
		t.Fatalf("oversized line response = %s, want a -32700 parse error", truncateForLog(lines[0]))
	}
	if !strings.Contains(lines[1], `"result"`) {
		t.Fatalf("stream did not survive the oversized line: %s", truncateForLog(lines[1]))
	}
}

// TestAuditResource_HundredConcurrentCalls 证明 100 个并发调用各自得到响应。
func TestAuditResource_HundredConcurrentCalls(t *testing.T) {
	server := newServer(t, echoTool("echo"))
	var requests strings.Builder
	for index := 1; index <= 100; index++ {
		fmt.Fprintf(&requests, `{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"echo","arguments":{}}}`+"\n", index)
	}
	var out lockedBuilder
	if err := server.Serve(context.Background(), strings.NewReader(requests.String()), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	lines := nonEmptyLines(out.String())
	if len(lines) != 100 {
		t.Fatalf("responses = %d, want 100", len(lines))
	}
	seen := make(map[float64]bool, 100)
	for _, line := range lines {
		var response struct {
			ID *float64 `json:"id"`
		}
		if err := json.Unmarshal([]byte(line), &response); err != nil || response.ID == nil {
			t.Fatalf("bad response line: %v (%s)", err, truncateForLog(line))
		}
		seen[*response.ID] = true
	}
	for index := 1; index <= 100; index++ {
		if !seen[float64(index)] {
			t.Fatalf("missing response for id %d", index)
		}
	}
}

// TestAuditResource_DefaultResultBudgetLeavesEnvelopeHeadroom 是"别把默认预算抬回
// 客户端上限"的守门用例。
//
// 历史：默认预算曾等于 tools/mcp 的 MaxMessageBytes（8 MiB），而整行还要加 JSON
// 信封/转义开销，于是最大响应会超出客户端读取上限。现在的实现把默认预算减半并加了
// 编码后硬上限；本用例在"有人把默认值抬回去"时立刻变红。
func TestAuditResource_DefaultResultBudgetLeavesEnvelopeHeadroom(t *testing.T) {
	if mcpserver.DefaultMaxResultBytes >= mcp.MaxMessageBytes {
		t.Fatalf("DefaultMaxResultBytes=%d must stay below the client read limit %d: "+
			"a full-budget result plus JSON envelope/escaping would exceed it",
			mcpserver.DefaultMaxResultBytes, mcp.MaxMessageBytes)
	}

	// 最坏情况：内容全是需要 JSON 转义的字符（每个 `"` 编码成 `\"`，体积翻倍）。
	tool := &fakeTool{
		name:   "escaped",
		schema: map[string]any{"type": "object"},
		invoke: func(context.Context, rig.ToolCall) (rig.ToolResult, error) {
			return rig.ToolResult{Content: strings.Repeat(`"`, mcpserver.DefaultMaxResultBytes)}, nil
		},
	}
	server := newServer(t, tool)
	response, handled, err := server.HandleLine(context.Background(),
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"escaped"}}`))
	if err != nil || !handled {
		t.Fatalf("HandleLine: handled=%v err=%v", handled, err)
	}
	lineBytes := len(strings.TrimRight(string(response), "\n"))
	if lineBytes > mcp.MaxMessageBytes {
		t.Fatalf("worst-case escaping produced a %d-byte line, beyond the client read limit %d",
			lineBytes, mcp.MaxMessageBytes)
	}
	if !json.Valid(response) {
		t.Fatal("worst-case response is not valid JSON")
	}
}

// TestAuditResource_HugeResultIsBoundedAndTruncated 证明 100 MiB 工具结果被默认上限
// 截断、响应仍是合法 JSON、且带 `_meta.truncated`。
func TestAuditResource_HugeResultIsBoundedAndTruncated(t *testing.T) {
	tool := &fakeTool{
		name:   "huge",
		schema: map[string]any{"type": "object"},
		invoke: func(context.Context, rig.ToolCall) (rig.ToolResult, error) {
			return rig.ToolResult{Content: strings.Repeat("x", 100<<20)}, nil
		},
	}
	server := newServer(t, tool)
	response, handled, err := server.HandleLine(context.Background(),
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"huge"}}`))
	if err != nil || !handled {
		t.Fatalf("HandleLine: handled=%v err=%v", handled, err)
	}
	if len(response) > 9<<20 {
		t.Fatalf("response = %d bytes, want it bounded near DefaultMaxResultBytes", len(response))
	}
	var decoded struct {
		Result struct {
			Content []struct {
				Text string         `json:"text"`
				Meta map[string]any `json:"_meta"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response, &decoded); err != nil {
		t.Fatalf("truncated response is not valid JSON: %v", err)
	}
	if len(decoded.Result.Content) != 1 {
		t.Fatalf("content blocks = %d, want 1", len(decoded.Result.Content))
	}
	block := decoded.Result.Content[0]
	if block.Meta["truncated"] != true {
		t.Fatalf("_meta.truncated = %v, want true", block.Meta)
	}
	if !strings.HasSuffix(block.Text, "bytes omitted]") {
		t.Fatalf("truncated text does not end with the omission note: %q", tailForLog(block.Text, 80))
	}
}

// TestAuditResource_TruncatedResultFitsClientMessageLimit 是"截断后的整行必须能被
// 客户端读回"的回归用例。
//
// 历史：审计发现默认预算（当时是 8 MiB text）会产出 8,388,710 字节的响应行，比
// tools/mcp 的 MaxMessageBytes=8,388,608 多 102 字节，客户端 `bufio.Scanner:
// token too long` → 传输中止。现在 server 侧把默认内容预算降到客户端上限的一半
// （DefaultMaxResultBytes = ClientReadLimit/2）并加了编码后硬上限，本用例守住这条线。
func TestAuditResource_TruncatedResultFitsClientMessageLimit(t *testing.T) {
	tool := &fakeTool{
		name:   "big",
		schema: map[string]any{"type": "object"},
		invoke: func(context.Context, rig.ToolCall) (rig.ToolResult, error) {
			return rig.ToolResult{Content: strings.Repeat("x", 9<<20)}, nil
		},
	}
	server := newServer(t, tool)
	response, handled, err := server.HandleLine(context.Background(),
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"big"}}`))
	if err != nil || !handled {
		t.Fatalf("HandleLine: handled=%v err=%v", handled, err)
	}
	lineBytes := len(strings.TrimRight(string(response), "\n"))
	if lineBytes > mcp.MaxMessageBytes {
		t.Fatalf("truncated response line is %d bytes, larger than the client's MaxMessageBytes=%d "+
			"(over by %d bytes): the server's text budget must reserve JSON envelope overhead",
			lineBytes, mcp.MaxMessageBytes, lineBytes-mcp.MaxMessageBytes)
	}
}

// TestAuditResource_UnlimitedContentStillFitsClientReadLimit 证明
// WithMaxResultBytes(MaxResultBytesUnlimited) 只关闭"内容预算"，编码后的行仍然被
// 传输层硬上限兜住（否则 -1 会变成"打崩客户端"的开关）。
func TestAuditResource_UnlimitedContentStillFitsClientReadLimit(t *testing.T) {
	tool := &fakeTool{
		name:   "huge",
		schema: map[string]any{"type": "object"},
		invoke: func(context.Context, rig.ToolCall) (rig.ToolResult, error) {
			// 全是要转义的字符：JSON 编码后体积会翻倍，最能考验硬上限。
			return rig.ToolResult{Content: strings.Repeat(`"\`, 50<<20)}, nil
		},
	}
	server, err := mcpserver.New([]rig.Tool{tool}, mcpserver.WithMaxResultBytes(mcpserver.MaxResultBytesUnlimited))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	response, handled, err := server.HandleLine(context.Background(),
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"huge"}}`))
	if err != nil || !handled {
		t.Fatalf("HandleLine: handled=%v err=%v", handled, err)
	}
	lineBytes := len(strings.TrimRight(string(response), "\n"))
	if lineBytes > mcp.MaxMessageBytes {
		t.Fatalf("unlimited content produced a %d-byte line, beyond the client's %d-byte read limit",
			lineBytes, mcp.MaxMessageBytes)
	}
	var decoded struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response, &decoded); err != nil {
		t.Fatalf("capped response is not valid JSON: %v", err)
	}
	if len(decoded.Result.Content) == 0 || decoded.Result.Content[0].Text == "" {
		t.Fatalf("capped response lost its content: %+v", decoded.Result)
	}
}

// TestAuditResource_WriteFailureSurfacesAsServeError 证明输出写失败会让 Serve 返回
// 错误（而不是静默吞掉）。
func TestAuditResource_WriteFailureSurfacesAsServeError(t *testing.T) {
	server := newServer(t, echoTool("echo"))
	var requests strings.Builder
	for index := 1; index <= 3; index++ {
		fmt.Fprintf(&requests, `{"jsonrpc":"2.0","id":%d,"method":"ping"}`+"\n", index)
	}
	writer := &alwaysFailingWriter{}
	err := server.Serve(context.Background(), strings.NewReader(requests.String()), writer)
	if err == nil {
		t.Fatal("Serve returned nil after every write failed")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error = %v, want it to wrap the writer failure", err)
	}
}

// alwaysFailingWriter 每次写都失败。
type alwaysFailingWriter struct {
	mu    sync.Mutex
	calls int
}

func (w *alwaysFailingWriter) Write([]byte) (int, error) {
	w.mu.Lock()
	w.calls++
	w.mu.Unlock()
	return 0, errors.New("boom: broken pipe")
}

// TestAuditLifecycle_EOFDrainsInflightCallsWithoutHanging 是 EOF 收尾的回归用例。
//
// 历史：审计发现 EOF 分支先 `wg.Wait()` 再取消，因此一个无视 ctx 的工具会让 Serve
// 永久挂死（客户端此时已关闭 stdin，连 notifications/cancelled 都发不出去）。
// 现状：Serve 先给 eofDrainGrace 宽限，再 cancelAllInflight，再等一个宽限；仍不结束
// 就返回错误而不是继续阻塞。本用例同时钉住"工具确实收到 ctx 取消"与"有界返回"。
func TestAuditLifecycle_EOFDrainsInflightCallsWithoutHanging(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	tool := &fakeTool{
		name:   "stuck",
		schema: map[string]any{"type": "object"},
		invoke: func(ctx context.Context, _ rig.ToolCall) (rig.ToolResult, error) {
			close(started)
			<-ctx.Done() // 宽限期后被取消
			close(cancelled)
			return rig.ToolResult{Content: "ctx cancelled", IsError: true}, nil
		},
	}
	server := newServer(t, tool)
	input := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"stuck"}}` + "\n")
	var out lockedBuilder
	done := make(chan error, 1)
	go func() { done <- server.Serve(context.Background(), input, &out) }()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("tool never started")
	}
	// 输入已经 EOF（strings.Reader 耗尽）：Serve 应当在有界时间内收尾并取消在途调用。
	select {
	case err := <-done:
		t.Logf("Serve returned after EOF: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatalf("Serve is still blocked 3s after stdin EOF with an in-flight tool: " +
			"the EOF path must cancel inflight calls instead of waiting forever")
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("the in-flight tool never observed context cancellation on the EOF path")
	}
}

// nonEmptyLines 返回非空行。
func nonEmptyLines(text string) []string {
	lines := make([]string, 0, strings.Count(text, "\n"))
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// truncateForLog 截断一行便于日志阅读。
func truncateForLog(line string) string {
	const limit = 200
	if len(line) <= limit {
		return line
	}
	return line[:limit] + "…"
}

// tailForLog 返回末尾 n 字节。
func tailForLog(text string, n int) string {
	if len(text) <= n {
		return text
	}
	return text[len(text)-n:]
}
