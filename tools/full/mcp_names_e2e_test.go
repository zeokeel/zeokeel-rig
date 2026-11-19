package full_test

// 端到端夹具：真 HTTP MCP server（httptest + JSON-RPC）→ mcp.StartHTTP → full 聚合
// → agent.New。
//
// 为什么需要它：tools/mcp 的单测只能证明"一次 NewTools 调用内不产生重名"，证明不了
// full 的**跨 client 聚合**接线是对的。这里起两个真实 client（同名 server "srv.a"，
// 都暴露 `read.file`），断言 full.NewRuntime/All 产出的工具名全局唯一，并且把它们
// 交给 agent.New 不会报 duplicate tool name —— 重名会让宿主应用在启动阶段直接失败。

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/agent"
	"github.com/zeokeel/zeokeel-rig/tools/full"
	"github.com/zeokeel/zeokeel-rig/tools/mcp"
)

const e2eMCPProtocol = "2025-06-18"

// newFakeMCPServer 起一个只实现 initialize / notifications/initialized / tools/list
// 的 JSON-RPC MCP 端点，tools/list 返回 toolName 一个工具。
//
// 形态照 tools/mcp/http_test.go 的 handler：POST 走 JSON-RPC、应答 application/json，
// GET（通知流）返回 405 让客户端降级，DELETE 返回 204。
func newFakeMCPServer(t *testing.T, toolName string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
			return
		case http.MethodPost:
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		var request struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Errorf("decode %q: %v", body, err)
			return
		}
		switch request.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "e2e-session")
			writeJSONRPC(t, w, map[string]any{
				"jsonrpc": "2.0",
				"id":      request.ID,
				"result": map[string]any{
					"protocolVersion": e2eMCPProtocol,
					"capabilities": map[string]any{
						"tools":     map[string]any{},
						"resources": map[string]any{},
						"prompts":   map[string]any{},
					},
					"serverInfo": map[string]any{"name": "e2e-fake", "version": "1.0.0"},
				},
			})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			writeJSONRPC(t, w, map[string]any{
				"jsonrpc": "2.0",
				"id":      request.ID,
				"result": map[string]any{
					"tools": []any{map[string]any{
						"name":        toolName,
						"description": "fake tool for the end-to-end fixture",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
					}},
				},
			})
		default:
			writeJSONRPC(t, w, map[string]any{
				"jsonrpc": "2.0",
				"id":      request.ID,
				"error":   map[string]any{"code": -32601, "message": "method not found"},
			})
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// writeJSONRPC 写一条 JSON-RPC 应答。
func writeJSONRPC(t *testing.T, w http.ResponseWriter, payload any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

// startMCPClient 起一个真实 HTTP client 并注册清理。
func startMCPClient(t *testing.T, url, serverName string) *mcp.Client {
	t.Helper()
	client, err := mcp.StartHTTP(context.Background(), mcp.HTTPConfig{
		URL:        url,
		ServerName: serverName,
		Timeout:    5 * time.Second,
	})
	if err != nil {
		t.Fatalf("start MCP client for %s: %v", url, err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// toolNames 返回工具集里的名字（保持顺序）。
func toolNames(tools []rig.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name())
	}
	return names
}

// requireUniqueNames 断言工具名唯一，失败时把重复项与全部名字一起打出来。
func requireUniqueNames(t *testing.T, tools []rig.Tool) []string {
	t.Helper()
	names := toolNames(tools)
	seen := make(map[string]int, len(names))
	for index, name := range names {
		if first, duplicate := seen[name]; duplicate {
			t.Fatalf("duplicate tool name %q at index %d (first at %d); names = %v", name, index, first, names)
		}
		seen[name] = index
	}
	return names
}

// TestMCPToolsAreUniqueAcrossClientsEndToEnd：两个同名 server（都是 "srv.a"）各暴露
// `read.file`，full 聚合后名字必须唯一，且能直接交给 agent.New。
func TestMCPToolsAreUniqueAcrossClientsEndToEnd(t *testing.T) {
	endpoint := newFakeMCPServer(t, "read.file")
	first := startMCPClient(t, endpoint, "srv.a")
	second := startMCPClient(t, endpoint, "srv.a")

	opts := full.AllOptions{
		Root:       t.TempDir(),
		MCPClients: []*mcp.Client{first, second},
	}
	runtime, err := full.NewRuntime(opts)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })

	names := requireUniqueNames(t, runtime.Tools)
	t.Logf("runtime tool count = %d", len(names))

	// 两个 client 的 read.file 都在，且第二个被确定性地改成了 __2。
	joined := "\x00" + strings.Join(names, "\x00") + "\x00"
	for _, want := range []string{"\x00mcp__srv_a__read_file\x00", "\x00mcp__srv_a__read_file__2\x00"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %v", strings.Trim(want, "\x00"), names)
		}
	}

	// All 与 NewRuntime 的产出必须一致（同名字集）。
	all := full.All(opts)
	allNames := requireUniqueNames(t, all)
	if len(allNames) != len(names) || strings.Join(allNames, ",") != strings.Join(names, ",") {
		t.Fatalf("All names differ from NewRuntime names:\n all  = %v\n rt   = %v", allNames, names)
	}

	// 真正的验收：这套工具体能交给 agent.New（重名会让它直接报错）。
	if _, err := agent.New(rig.AgentSpec{
		Model:    "test-model",
		Provider: stubProvider{},
		Tools:    runtime.Tools,
	}); err != nil {
		t.Fatalf("agent.New rejected the full tool set: %v (names: %v)", err, names)
	}
}

// TestMCPResourceToolsAreUniqueAcrossClients：server 绑定工具里还有两个名字**不含
// server 身份**的资源工具（mcp_list_resources / mcp_read_resource），两个 client 会
// 各自产出同名工具 —— 这也是跨 client 重名的来源，端到端夹具必须一起覆盖。
func TestMCPResourceToolsAreUniqueAcrossClients(t *testing.T) {
	endpoint := newFakeMCPServer(t, "read.file")
	first := startMCPClient(t, endpoint, "srv.a")
	second := startMCPClient(t, endpoint, "srv.b")
	runtime, err := full.NewRuntime(full.AllOptions{
		Root:       t.TempDir(),
		MCPClients: []*mcp.Client{first, second},
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })

	names := requireUniqueNames(t, runtime.Tools)
	if _, err := agent.New(rig.AgentSpec{
		Model:    "test-model",
		Provider: stubProvider{},
		Tools:    runtime.Tools,
	}); err != nil {
		t.Fatalf("agent.New rejected the tool set: %v (names: %v)", err, names)
	}
}

// stubProvider 只是让 agent.New 通过 Provider 必填校验；夹具不会真的调用模型。
type stubProvider struct{}

func (stubProvider) Name() string { return "stub" }

func (stubProvider) Stream(context.Context, rig.Request) (rig.Stream, error) {
	return nil, errors.New("stub provider: not used by this fixture")
}
