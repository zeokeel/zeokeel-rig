package mcp

// 审计 A：MCP 资源/提示工具的模型可见名必须带 server 身份。
//
// 现象（修复前）：五个名字是固定的 `mcp_list_resources` / `mcp_read_resource` /
// `mcp_list_resource_templates` / `mcp_list_prompts` / `mcp_get_prompt`，完全不含 server
// —— 配两个 client 时逐字重复，宿主只能靠 tools/full 的兜底改名掩盖；同一个 server 配
// 两次时更是连"哪个连接"都看不出来。

import (
	"net/http"
	"strings"
	"testing"
)

// resourceToolNamesOf 收集 ResourceTools 产出的名字。
func resourceToolNamesOf(client *Client, prefix string) []string {
	tools := ResourceTools(client, prefix)
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name())
	}
	return names
}

// TestResourceToolNamesCarryServerIdentity：两个不同 server 的连接 → 资源工具名各带
// 自己的 server 身份，且两组名字互不重叠。
func TestResourceToolNamesCarryServerIdentity(t *testing.T) {
	url := newHTTPTestServer(t, newHTTPMCPHandler(t, httpMCPHooks{
		onRequest: func(msg httpRPCRequest, w http.ResponseWriter, _ *http.Request) {
			// 资源/提示工具是懒加载的，构造阶段不会请求；这里只兜住意外的 tools/list。
			writeHTTPRPCResult(t, w, msg.ID, map[string]any{"tools": []any{}})
		},
	}))
	first := startHTTPClient(t, url, func(cfg *HTTPConfig) { cfg.ServerName = "srv.a" })
	second := startHTTPClient(t, url, func(cfg *HTTPConfig) { cfg.ServerName = "srv.b" })

	firstNames := resourceToolNamesOf(first, "mcp__")
	secondNames := resourceToolNamesOf(second, "mcp__")
	t.Logf("first = %v", firstNames)
	t.Logf("second = %v", secondNames)
	if len(firstNames) != 5 || len(secondNames) != 5 {
		t.Fatalf("expected 5 resource/prompt tools per connection, got %d and %d", len(firstNames), len(secondNames))
	}
	for _, name := range firstNames {
		if !strings.HasPrefix(name, "mcp__srv_a__") {
			t.Fatalf("resource tool name %q does not carry the server identity (want mcp__srv_a__...)", name)
		}
	}
	for _, name := range secondNames {
		if !strings.HasPrefix(name, "mcp__srv_b__") {
			t.Fatalf("resource tool name %q does not carry the server identity (want mcp__srv_b__...)", name)
		}
	}
	if strings.Join(firstNames, ",") != strings.Join(resourceToolNamesOf(first, "mcp__"), ",") {
		t.Fatalf("ResourceTools is not stable for the same connection: %v", firstNames)
	}
}

// TestAggregateToolsResourcesUniqueAcrossSameNamedConnections：两条**同名**连接经
// AggregateTools 聚合资源工具时，名字必须仍然唯一（第二条连接获得 __2 后缀），因为
// tools/full 就是靠这一次聚合拿到可注册的工具集。
func TestAggregateToolsResourcesUniqueAcrossSameNamedConnections(t *testing.T) {
	url := newHTTPTestServer(t, newHTTPMCPHandler(t, httpMCPHooks{
		onRequest: func(msg httpRPCRequest, w http.ResponseWriter, _ *http.Request) {
			writeHTTPRPCResult(t, w, msg.ID, map[string]any{"tools": []any{httpToolJSON("read.file", "dotted")}})
		},
	}))
	first := startHTTPClient(t, url, func(cfg *HTTPConfig) { cfg.ServerName = "srv.a" })
	second := startHTTPClient(t, url, func(cfg *HTTPConfig) { cfg.ServerName = "srv.a" })

	tools, collisions, err := AggregateTools([]ServerTools{
		{Client: first, Resources: true},
		{Client: second, Resources: true},
	}, "")
	if err != nil {
		t.Fatalf("AggregateTools: %v", err)
	}
	names := names(tools)
	t.Logf("names = %v", names)
	// 2 个 read.file + 2×5 个资源/提示工具。
	if len(tools) != 12 {
		t.Fatalf("got %d tools, want 12 (2 tools + 10 resource/prompt tools): %v", len(tools), names)
	}
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			t.Fatalf("duplicate name %q in %v", name, names)
		}
		seen[name] = true
	}
	for _, want := range []string{"mcp__srv_a__list_resources", "mcp__srv_a__list_resources__2"} {
		if !seen[want] {
			t.Fatalf("missing %q in %v", want, names)
		}
	}
	// read.file 1 条 + 5 个资源/提示工具 5 条 = 6 条冲突。
	if len(collisions) != 6 {
		t.Fatalf("collisions = %d, want 6 (read.file + 5 resource tools): %+v", len(collisions), collisions)
	}
}

// TestResourceToolNamesUseSanitizedServer：server 名按与 Tools 相同的规则清洗。
func TestResourceToolNamesUseSanitizedServer(t *testing.T) {
	client := &Client{}
	client.serverName = "srv.a"
	names := resourceToolNamesOf(client, "mcp__")
	t.Logf("names = %v", names)
	want := []string{
		"mcp__srv_a__list_resources",
		"mcp__srv_a__read_resource",
		"mcp__srv_a__list_resource_templates",
		"mcp__srv_a__list_prompts",
		"mcp__srv_a__get_prompt",
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("names = %v, want %v", names, want)
	}
}
