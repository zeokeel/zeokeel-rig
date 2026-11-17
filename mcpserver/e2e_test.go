package mcpserver_test

// 端到端互通测试：**真的**用 tools/mcp 的 client（子进程 stdio 传输）连本包的
// server。测试二进制通过环境变量把自己重新执行为 server，因此不需要外部二进制、
// 不需要网络。

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/mcpserver"
	"github.com/zeokeel/zeokeel-rig/tools/mcp"
)

const (
	e2eHelperEnv     = "RIG_MCPSERVER_E2E_HELPER"
	e2eServerName    = "rig-e2e"
	e2eServerVersion = "0.0.1-e2e"
)

// TestMain 在被子进程重新执行时扮演 MCP server，否则跑本包的测试。
func TestMain(m *testing.M) {
	if os.Getenv(e2eHelperEnv) == "1" {
		if err := runE2EServer(); err != nil {
			os.Stderr.WriteString("e2e helper: " + err.Error() + "\n")
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runE2EServer 是我们自己的 server 在 stdin/stdout 上的真实服务。
func runE2EServer() error {
	tools := []rig.Tool{
		&fakeTool{
			name:        "echo",
			description: "回显参数（E2E）",
			schema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"text": map[string]any{"type": "string"}},
			},
			invoke: func(_ context.Context, call rig.ToolCall) (rig.ToolResult, error) {
				return rig.ToolResult{Content: "echo:" + call.Arguments}, nil
			},
		},
		&fakeTool{
			name:        "failing",
			description: "总是失败（E2E）",
			schema:      map[string]any{"type": "object"},
			invoke: func(context.Context, rig.ToolCall) (rig.ToolResult, error) {
				return rig.ToolResult{Content: "工具级失败：磁盘只读", IsError: true}, nil
			},
		},
		&fakeTool{
			name:        "big",
			description: "返回刚好超过默认上限的结果（E2E）",
			schema:      map[string]any{"type": "object"},
			invoke: func(context.Context, rig.ToolCall) (rig.ToolResult, error) {
				return rig.ToolResult{Content: strings.Repeat("x", mcpserver.DefaultMaxResultBytes+1024)}, nil
			},
		},
	}
	server, err := mcpserver.New(tools,
		mcpserver.WithServerInfo(e2eServerName, e2eServerVersion),
		mcpserver.WithInstructions("E2E 说明"),
	)
	if err != nil {
		return err
	}
	return server.Serve(context.Background(), os.Stdin, os.Stdout)
}

// TestClientServerEndToEnd 用真实 client 走完整握手 + list + call + ping。
func TestClientServerEndToEnd(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := mcp.Start(ctx, mcp.Config{
		Command: executable,
		Args:    []string{"-test.run=^$"}, // 子进程由 TestMain 短路，不跑任何测试
		Env:     append(os.Environ(), e2eHelperEnv+"=1"),
		Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("mcp.Start: %v", err)
	}
	defer func() { _ = client.Close() }()

	// 1) 握手：serverInfo 与协议版本。
	if got := client.ServerName(); got != e2eServerName {
		t.Fatalf("ServerName = %q, want %q", got, e2eServerName)
	}
	if got := client.ServerVersion(); got != e2eServerVersion {
		t.Fatalf("ServerVersion = %q, want %q", got, e2eServerVersion)
	}
	if got := client.ProtocolVersion(); got != mcp.SupportedProtocolVersions[0] {
		t.Fatalf("ProtocolVersion = %q, want %q", got, mcp.SupportedProtocolVersions[0])
	}

	// 2) tools/list：名字、描述、schema 都要能过 client 的校验。
	list, err := client.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("ListTools returned %d tools, want 3: %+v", len(list), list)
	}
	byName := map[string]mcp.ToolInfo{}
	for _, info := range list {
		byName[info.Name] = info
	}
	echo, ok := byName["echo"]
	if !ok {
		t.Fatalf("echo tool missing: %+v", list)
	}
	if echo.Description != "回显参数（E2E）" {
		t.Fatalf("echo description = %q", echo.Description)
	}
	var schema map[string]any
	if err := json.Unmarshal(echo.InputSchema, &schema); err != nil {
		t.Fatalf("echo schema is not an object: %v (%s)", err, echo.InputSchema)
	}
	if schema["type"] != "object" {
		t.Fatalf("echo schema = %v, want type=object", schema)
	}

	// 3) tools/call 成功路径（含非 ASCII 往返）。
	callResult, err := client.CallTool(ctx, "echo", json.RawMessage(`{"text":"你好 🚀"}`))
	if err != nil {
		t.Fatalf("CallTool(echo): %v", err)
	}
	if callResult.IsError {
		t.Fatalf("CallTool(echo) reported IsError: %+v", callResult)
	}
	if len(callResult.Content) != 1 || callResult.Content[0].Type != "text" {
		t.Fatalf("content = %+v", callResult.Content)
	}
	if !strings.Contains(callResult.Content[0].Text, "你好 🚀") {
		t.Fatalf("content text = %q, want the echoed arguments", callResult.Content[0].Text)
	}

	// 4) 工具级失败 → IsError 而不是协议错误。
	failure, err := client.CallTool(ctx, "failing", nil)
	if err != nil {
		t.Fatalf("CallTool(failing) returned a protocol error: %v", err)
	}
	if !failure.IsError {
		t.Fatalf("CallTool(failing) IsError = false, want true: %+v", failure)
	}
	if len(failure.Content) == 0 || !strings.Contains(failure.Content[0].Text, "磁盘只读") {
		t.Fatalf("failure content = %+v", failure.Content)
	}

	// 5) 未知工具 → JSON-RPC 错误 -32602（client 侧是 *mcp.RPCError）。
	_, err = client.CallTool(ctx, "does-not-exist", nil)
	if err == nil {
		t.Fatal("CallTool(unknown) succeeded, want a protocol error")
	}
	var rpcError *mcp.RPCError
	if !errors.As(err, &rpcError) {
		t.Fatalf("error = %v (%T), want *mcp.RPCError", err, err)
	}
	if rpcError.Code != mcpserver.CodeInvalidParams {
		t.Fatalf("error code = %d, want %d", rpcError.Code, mcpserver.CodeInvalidParams)
	}

	// 6) ping。
	if err := client.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

// TestClientRejectsWrongProtocolVersion 确保协商失败时 client 会拒绝——反向证明
// 我们的 version 协商是 client 真正在意的字段。
func TestClientRejectsWrongProtocolVersion(t *testing.T) {
	// 直接对 HandleLine 断言：我们回给一个 client 不支持的版本时，client 的
	// SupportedProtocolVersions 里没有它，因此会握手失败（这里只验证我们确实
	// 会回显陌生版本这一点不会悄悄发生）。
	server, err := mcpserver.New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	response, _, err := server.HandleLine(context.Background(), []byte(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2038-01-01"}}`))
	if err != nil {
		t.Fatalf("HandleLine: %v", err)
	}
	var decoded struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, supported := range mcp.SupportedProtocolVersions {
		if decoded.Result.ProtocolVersion == supported {
			return // 回落到双方都支持的版本：正确行为
		}
	}
	t.Fatalf("server answered unsupported version %q, which the client would reject",
		decoded.Result.ProtocolVersion)
}

// TestClientReceivesTruncatedBigResult 是输出上限的端到端回归：工具返回
// DefaultMaxResultBytes+1024 的内容时，server 必须截断后**正常应答**，
// 真实 client 既不能报 "bufio.Scanner: token too long"，也不能把它当协议错误。
func TestClientReceivesTruncatedBigResult(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client, err := mcp.Start(ctx, mcp.Config{
		Command: executable,
		Args:    []string{"-test.run=^$"},
		Env:     append(os.Environ(), e2eHelperEnv+"=1"),
		Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("mcp.Start: %v", err)
	}
	defer func() { _ = client.Close() }()

	result, err := client.CallTool(ctx, "big", nil)
	if err != nil {
		t.Fatalf("CallTool(big) failed at the transport level: %v", err)
	}
	if result.IsError {
		t.Fatalf("truncation was reported as a tool error: %+v", result)
	}
	if len(result.Content) != 1 {
		t.Fatalf("content blocks = %d, want 1", len(result.Content))
	}
	text := result.Content[0].Text
	if !strings.Contains(text, "[truncated: ") {
		t.Fatalf("big result was not truncated: %d bytes", len(text))
	}
	if len(text) > mcpserver.DefaultMaxResultBytes {
		t.Fatalf("truncated text is %d bytes, want <= %d", len(text), mcpserver.DefaultMaxResultBytes)
	}
	// 连接必须还能用（客户端没有被一行超长消息搞坏）。
	if err := client.Ping(ctx); err != nil {
		t.Fatalf("Ping after a truncated big result: %v", err)
	}
}
