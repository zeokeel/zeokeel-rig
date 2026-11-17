package mcpserver_test

// 审计：与 tools/mcp 真实 client 的相容性（协议版本、空工具集、异常工具名、
// isError/RPCError 边界，以及"服务端超长行会不会打崩客户端"）。
//
// 子进程技巧：父进程用 -test.run=^TestAuditServeHelper$ 重新执行测试二进制，helper 在
// 子进程里扮 MCP server；服务结束用 os.Exit 避免测试框架往 stdout 写 "PASS" 污染
// JSON-RPC 流（与 e2e_test.go 的 TestMain 同一思路，但不需要改 TestMain）。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/mcpserver"
	"github.com/zeokeel/zeokeel-rig/tools/mcp"
)

const auditHelperEnv = "RIG_MCPSERVER_AUDIT_HELPER"

// TestAuditServeHelper 在子进程里扮演 server；父进程里它只是个被跳过的用例。
func TestAuditServeHelper(t *testing.T) {
	if os.Getenv(auditHelperEnv) != "1" {
		t.Skip("helper process only")
	}
	big := &fakeTool{
		name:   "huge",
		schema: map[string]any{"type": "object"},
		invoke: func(context.Context, rig.ToolCall) (rig.ToolResult, error) {
			return rig.ToolResult{Content: strings.Repeat("x", 100<<20)}, nil
		},
	}
	failing := &fakeTool{
		name:   "failing",
		schema: map[string]any{"type": "object"},
		invoke: func(context.Context, rig.ToolCall) (rig.ToolResult, error) {
			return rig.ToolResult{}, errors.New("tool exploded")
		},
	}
	weird := &fakeTool{
		name:        "中文 名 字",
		description: "带空格与中文",
		schema:      map[string]any{"type": "object"},
		invoke: func(context.Context, rig.ToolCall) (rig.ToolResult, error) {
			return rig.ToolResult{Content: "ok"}, nil
		},
	}
	longName := &fakeTool{
		name:   strings.Repeat("n", 200),
		schema: map[string]any{"type": "object"},
		invoke: func(context.Context, rig.ToolCall) (rig.ToolResult, error) {
			return rig.ToolResult{Content: "long"}, nil
		},
	}
	crash := &fakeTool{
		name:   "crash",
		schema: map[string]any{"type": "object"},
		invoke: func(context.Context, rig.ToolCall) (rig.ToolResult, error) {
			// 模拟服务端在请求中途崩溃：没有任何响应就退出进程。
			os.Exit(3)
			return rig.ToolResult{}, nil
		},
	}
	var tools []rig.Tool
	switch os.Getenv(auditHelperEnv + "_SET") {
	case "empty":
		tools = nil
	case "edge":
		tools = []rig.Tool{weird, longName}
	case "crash":
		tools = []rig.Tool{crash}
	default:
		tools = []rig.Tool{big, failing}
	}
	server, err := mcpserver.New(tools, mcpserver.WithServerInfo("audit-server", "0.0.0"))
	if err != nil {
		fmt.Fprint(os.Stderr, "audit helper: "+err.Error()+"\n")
		os.Exit(1)
	}
	if err := server.Serve(context.Background(), os.Stdin, os.Stdout); err != nil {
		fmt.Fprint(os.Stderr, "audit helper: "+err.Error()+"\n")
		os.Exit(1)
	}
	os.Exit(0)
}

// startAuditClient 起一个子进程 server 并返回已握手的 client。
func startAuditClient(t *testing.T, set string) *mcp.Client {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	env := append(os.Environ(), auditHelperEnv+"=1", auditHelperEnv+"_SET="+set)
	client, err := mcp.Start(ctx, mcp.Config{
		Command: executable,
		Args:    []string{"-test.run=^TestAuditServeHelper$"},
		Env:     env,
		Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("mcp.Start(set=%s): %v", set, err)
	}
	t.Cleanup(func() {
		// 传输已被 abort 时 Close 也可能触发客户端内部状态问题；这里不让清理失败
		// 掩盖真正的断言。
		defer func() { _ = recover() }()
		_ = client.Close()
	})
	return client
}

// TestAuditCompat_ProtocolVersionNegotiationAgrees 证明服务端支持的版本集合与
// client 的完全一致，并且逐个版本都能协商成功（回显）。
func TestAuditCompat_ProtocolVersionNegotiationAgrees(t *testing.T) {
	server := newServer(t, echoTool("echo"))
	serverSet := make(map[string]bool, len(mcpserver.SupportedProtocolVersions))
	for _, version := range mcpserver.SupportedProtocolVersions {
		serverSet[version] = true
	}
	if len(serverSet) != len(mcp.SupportedProtocolVersions) {
		t.Fatalf("server versions = %v, client versions = %v", mcpserver.SupportedProtocolVersions, mcp.SupportedProtocolVersions)
	}
	for _, version := range mcp.SupportedProtocolVersions {
		if !serverSet[version] {
			t.Fatalf("server does not support client version %q (server: %v)", version, mcpserver.SupportedProtocolVersions)
		}
		line := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":%q}}`, version)
		result := auditResult(t, auditRaw(t, server, line))
		if result["protocolVersion"] != version {
			t.Fatalf("initialize(%q) echoed %v, want the same version", version, result["protocolVersion"])
		}
	}
	// 客户端握手（它总是请求最新版本）也必须成功。
	client := startAuditClient(t, "edge")
	if got := client.ProtocolVersion(); got != mcp.SupportedProtocolVersions[0] {
		t.Fatalf("client negotiated %q, want %q", got, mcp.SupportedProtocolVersions[0])
	}
}

// TestAuditCompat_EmptyToolListAcceptedByRealClient 证明 tools/list 的空数组被真实
// client 接受（不是 null、不报错）。
func TestAuditCompat_EmptyToolListAcceptedByRealClient(t *testing.T) {
	client := startAuditClient(t, "empty")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	list, err := client.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools on an empty server: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("ListTools = %+v, want none", list)
	}
	if err := client.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

// TestAuditCompat_EdgeToolNamesAcceptedByRealClient 证明带空格/中文/200 字符的工具名
// 能被真实 client 列出并调用。
//
// 注意：MCP 对工具名的建议是 `^[a-zA-Z0-9_-]{1,64}$`；本 server 不做校验，因此这类
// 名字能过我们的 client，但对更严格的客户端（编辑器/桌面客户端）可能不可见。见报告。
func TestAuditCompat_EdgeToolNamesAcceptedByRealClient(t *testing.T) {
	client := startAuditClient(t, "edge")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	list, err := client.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListTools = %d tools, want 2", len(list))
	}
	for _, name := range []string{"中文 名 字", strings.Repeat("n", 200)} {
		result, err := client.CallTool(ctx, name, nil)
		if err != nil {
			t.Fatalf("CallTool(%q): %v", name, err)
		}
		if result.IsError {
			t.Fatalf("CallTool(%q) reported IsError: %+v", name, result)
		}
	}
}

// TestAuditCompat_ConcurrentRequestsAfterCrashAllFail 端到端回归：服务端崩溃后，
// 10 个并发请求全部返回错误（没有一个 panic，也没有把客户端内部互斥量锁死）。
//
// 注意 panic 曾经发生在**持锁期间**，所以表现不只是崩溃：后续所有请求会在 Lock()
// 上永久阻塞（同一个 bug 的两种症状）。
func TestAuditCompat_ConcurrentRequestsAfterCrashAllFail(t *testing.T) {
	client := startAuditClient(t, "crash")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := client.CallTool(ctx, "crash", nil); err == nil {
		t.Fatal("CallTool(crash) succeeded; the server process was supposed to die")
	}

	const workers = 10
	var wait sync.WaitGroup
	errs := make([]error, workers)
	panics := make([]any, workers)
	for index := 0; index < workers; index++ {
		wait.Add(1)
		go func(slot int) {
			defer wait.Done()
			defer func() { panics[slot] = recover() }()
			if slot%2 == 0 {
				errs[slot] = client.Ping(ctx)
				return
			}
			_, errs[slot] = client.ListTools(ctx)
		}(index)
	}
	wait.Wait()
	for slot := 0; slot < workers; slot++ {
		if panics[slot] != nil {
			t.Fatalf("worker %d panicked after the server crashed: %v", slot, panics[slot])
		}
		if errs[slot] == nil {
			t.Fatalf("worker %d returned nil error after the server crashed", slot)
		}
	}
}

// TestAuditCompat_IsErrorVersusRPCError 用真实 client 钉住失败分类：
// 工具返回 Go error → CallResult.IsError=true（不是 RPCError）；
// 未知工具 → *mcp.RPCError（-32602）。
func TestAuditCompat_IsErrorVersusRPCError(t *testing.T) {
	client := startAuditClient(t, "big")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	result, err := client.CallTool(ctx, "failing", nil)
	if err != nil {
		t.Fatalf("CallTool(failing) returned a protocol error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("CallTool(failing) IsError = false: %+v", result)
	}
	if len(result.Content) == 0 || !strings.Contains(result.Content[0].Text, "tool exploded") {
		t.Fatalf("CallTool(failing) content = %+v, want the Go error text", result.Content)
	}

	_, err = client.CallTool(ctx, "does-not-exist", nil)
	if err == nil {
		t.Fatal("CallTool(unknown tool) succeeded, want an RPC error")
	}
	var rpcError *mcp.RPCError
	if !errors.As(err, &rpcError) {
		t.Fatalf("error = %v (%T), want *mcp.RPCError", err, err)
	}
	if rpcError.Code != mcpserver.CodeInvalidParams {
		t.Fatalf("RPC error code = %d, want %d", rpcError.Code, mcpserver.CodeInvalidParams)
	}
}

// TestAuditCompat_HugeResultIsTruncatedAndSessionSurvives 回归：100 MiB 工具结果经
// 服务端截断后，真实 client 能完整读回一行、内容带截断注释、连接仍可用。
//
// 历史：默认预算等于客户端读取上限时，整行会超出 8 MiB，客户端 `token too long` →
// 传输中止。现在预算减半并加了编码后硬上限，本用例守住"服务端输出永远可被客户端读回"。
func TestAuditCompat_HugeResultIsTruncatedAndSessionSurvives(t *testing.T) {
	client := startAuditClient(t, "big")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	result, err := client.CallTool(ctx, "huge", nil)
	if err != nil {
		t.Fatalf("CallTool(huge) on the real client: %v", err)
	}
	if len(result.Content) == 0 {
		t.Fatal("CallTool(huge) returned no content")
	}
	if !strings.Contains(result.Content[0].Text, "bytes omitted]") {
		t.Fatalf("content is not marked as truncated: %q", tailForLog(result.Content[0].Text, 120))
	}
	if err := client.Ping(ctx); err != nil {
		t.Fatalf("session did not survive a truncated huge result: %v", err)
	}
}

// TestAuditCompat_RequestAfterTransportAbortReturnsError 端到端回归（原审计发现
// F2，已修）：传输被 abort 之后（这里用"服务端在请求中途退出进程"复现，
// 超长行路径已被服务端修掉），`tools/mcp` 的下一步请求不是返回 ErrClosed，而是
// **panic：assignment to entry in nil map**。
//
// 历史位置：tools/mcp/stdio_transport.go:96-104。abort() 把 t.pending 置为 nil 但
// 只设置 abortErr、不设置 t.closed；Request() 在锁内只检查 t.closed，随后
// `t.pending[msg.ID] = response` 直接写 nil map。更糟的是 panic 发生在**持锁期间**，
// 会把 t.mu 永久留在锁定状态，后续所有请求在 Lock() 上死锁（同一 bug 的两种症状）。
//
// 修复：Request 在锁内检查 `t.pending == nil`，返回 abortErr/ErrClosed。
//
// 注意缺陷归属：这是 tools/mcp（客户端）的实现问题，**不是 mcpserver 的缺陷**——
// mcpserver 侧现在保证任何响应行都不会超过客户端的读取上限（见
// TestAuditResource_DefaultResultBudgetLeavesEnvelopeHeadroom 与
// TestAuditCompat_HugeResultIsTruncatedAndSessionSurvives），所以本用例改用"服务端
// 中途崩溃"这一与截断无关的路径来复现。
//
// 现状（已修）：tools/mcp 的 Request 在 pending == nil 时返回 abortErr（服务端崩溃
// 时是 "server process exited"），不再 panic；本用例是这条修复的端到端回归。
func TestAuditCompat_RequestAfterTransportAbortReturnsError(t *testing.T) {
	client := startAuditClient(t, "crash")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 服务端在请求处理中直接退出：客户端应把在途请求置为传输错误（而不是 panic）。
	if _, err := client.CallTool(ctx, "crash", nil); err == nil {
		t.Fatal("CallTool(crash) succeeded; the server process was supposed to die")
	}

	var recovered any
	var pingErr error
	func() {
		defer func() { recovered = recover() }()
		pingErr = client.Ping(ctx)
	}()
	if recovered != nil {
		t.Fatalf("client panicked after the transport aborted: %v "+
			"(Request must return an identifiable error, not write a nil pending map)", recovered)
	}
	if pingErr == nil {
		t.Fatal("Ping succeeded on an aborted transport")
	}
	if !errors.Is(pingErr, mcp.ErrClosed) && !strings.Contains(pingErr.Error(), "server process exited") {
		t.Fatalf("Ping error = %v, want ErrClosed or a process-exit error", pingErr)
	}
	t.Logf("Ping on an aborted transport returned an identifiable error: %v", pingErr)
}
