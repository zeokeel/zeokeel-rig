package mcp

// 审计边界 #4：MCP 工具身份必须是"**连接** + 原始工具名"，而不是
// "(server 显示名, 原始工具名)"。
//
// 现象（修复前）：两个不同 client 声明同一个 ServerName（同一个 server 配两次，或两
// 个 server 恰好同名）时，buildToolSet 按 (server 名, 原始名) 去重，把第二个连接的
// 工具**整个丢掉** —— 宿主少了一半工具，而且完全没有诊断。tools/full 只是在装配边界
// 用名字后缀规避了症状，根因在这里。

import (
	"net/http"
	"testing"
)

// TestAggregateToolsKeepsSameNamedConnections：两个真实连接、相同 ServerName、各暴露
// read.file → 必须得到 2 个工具（`mcp__srv_a__read_file` 与 `mcp__srv_a__read_file__2`），
// 并且恰好报告 1 条冲突。
func TestAggregateToolsKeepsSameNamedConnections(t *testing.T) {
	url := newHTTPTestServer(t, newHTTPMCPHandler(t, httpMCPHooks{
		onRequest: func(msg httpRPCRequest, w http.ResponseWriter, _ *http.Request) {
			if msg.Method != "tools/list" {
				t.Errorf("method = %q, want tools/list", msg.Method)
			}
			writeHTTPRPCResult(t, w, msg.ID, map[string]any{
				"tools": []any{httpToolJSON("read.file", "dotted tool")},
			})
		},
	}))
	first := startHTTPClient(t, url, func(cfg *HTTPConfig) { cfg.ServerName = "srv.a" })
	second := startHTTPClient(t, url, func(cfg *HTTPConfig) { cfg.ServerName = "srv.a" })
	if first == second {
		t.Fatal("fixture must create two distinct client connections")
	}

	// Tools 留 nil：让 AggregateTools 走真实的 ListTools 路径。
	tools, collisions, err := AggregateTools([]ServerTools{
		{Client: first},
		{Client: second},
	}, "")
	if err != nil {
		t.Fatalf("AggregateTools: %v", err)
	}
	got := names(tools)
	t.Logf("names = %v collisions = %v", got, collisions)
	if len(tools) != 2 {
		t.Fatalf("got %d tool(s) %v, want both connections' tools (第二个连接的工具被当成重复丢掉了)", len(tools), got)
	}
	if got[0] != "mcp__srv_a__read_file" || got[1] != "mcp__srv_a__read_file__2" {
		t.Fatalf("names = %v, want [mcp__srv_a__read_file mcp__srv_a__read_file__2]", got)
	}
	if len(collisions) != 1 {
		t.Fatalf("collisions = %v, want exactly 1", collisions)
	}
	collision := collisions[0]
	if collision.ConflictsWithName != "read.file" {
		t.Fatalf("ConflictsWithName = %q, want read.file (the first connection's tool): %+v",
			collision.ConflictsWithName, collision)
	}
	if collision.ConflictsWithServer != "srv.a" || collision.Server != "srv.a" {
		t.Fatalf("collision servers = %q/%q, want srv.a/srv.a: %+v",
			collision.ConflictsWithServer, collision.Server, collision)
	}
	if collision.AssignedName != "mcp__srv_a__read_file__2" {
		t.Fatalf("AssignedName = %q", collision.AssignedName)
	}
}

// TestAggregateToolsSameConnectionDuplicateStaysWrappedOnce：**同一连接**里同一个工具
// 出现两次仍然只包装一次（既有语义不能被连接身份改动破坏）。
func TestAggregateToolsSameConnectionDuplicateStaysWrappedOnce(t *testing.T) {
	url := newHTTPTestServer(t, newHTTPMCPHandler(t, httpMCPHooks{
		onRequest: func(msg httpRPCRequest, w http.ResponseWriter, _ *http.Request) {
			writeHTTPRPCResult(t, w, msg.ID, map[string]any{
				"tools": []any{httpToolJSON("echo", "first"), httpToolJSON("echo", "second")},
			})
		},
	}))
	client := startHTTPClient(t, url, func(cfg *HTTPConfig) { cfg.ServerName = "srv.a" })
	tools, collisions, err := AggregateTools([]ServerTools{{Client: client}}, "")
	if err != nil {
		t.Fatalf("AggregateTools: %v", err)
	}
	got := names(tools)
	t.Logf("names = %v collisions = %v", got, collisions)
	if len(tools) != 1 || got[0] != "mcp__srv_a__echo" {
		t.Fatalf("tools = %v, want exactly one mcp__srv_a__echo", got)
	}
	if len(collisions) != 0 {
		t.Fatalf("collisions = %v, want none for a duplicated listing in one connection", collisions)
	}
}
