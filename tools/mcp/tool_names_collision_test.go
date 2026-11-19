package mcp

// 审计 P1 回归：MCP 工具名在字符清洗后可能相撞。
//
// 现象（修复前）：NewTools 只对 server/工具名做 sanitize，不查重 —— 一个 server
// 同时公布 `read.file` 与 `read_file` 时两者都变成 `mcp__srv__read_file`（跨 server
// 的 serverPart 同名时同理）。上层把这些工具塞进 agent.AgentSpec.Tools，而
// agent.New 对重名是硬报错：接上这样一个 MCP server，应用直接起不来，错误里只有
// 清洗后的名字，看不出是哪个 server 的哪个工具撞的。

import (
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
)

func names(tools []rig.Tool) []string {
	out := make([]string, 0, len(tools))
	for _, tool := range tools {
		out = append(out, tool.Name())
	}
	return out
}

// TestNewToolsSanitizedCollisionGetsUniqueNames：同一 server 内 `read.file` 与
// `read_file` 清洗后同名时必须得到两个**不同**的模型可见名，且各自仍指向正确的
// 远端工具。
func TestNewToolsSanitizedCollisionGetsUniqueNames(t *testing.T) {
	client := &Client{}
	infos := []ToolInfo{
		{Name: "read.file", Description: "dotted"},
		{Name: "read_file", Description: "underscored"},
	}
	tools := NewTools(client, "srv", infos, "")
	got := names(tools)
	t.Logf("names = %v", got)
	seen := map[string]bool{}
	for _, name := range got {
		if seen[name] {
			t.Fatalf("duplicate model-visible tool name %q in %v (agent.New would refuse to start)", name, got)
		}
		seen[name] = true
	}
	if len(tools) != 2 {
		t.Fatalf("tools = %v, want both tools wrapped", got)
	}
	// 每个包装工具必须仍然映射到它自己的远端名字。
	remote := map[string]string{}
	for _, wrapped := range tools {
		internal, ok := wrapped.(*tool)
		if !ok {
			t.Fatalf("unexpected tool type %T", wrapped)
		}
		remote[internal.name] = internal.remoteName
	}
	if remote["mcp__srv__read_file"] != "read.file" {
		t.Fatalf("first name maps to %q, want read.file", remote["mcp__srv__read_file"])
	}
	if got := remote["mcp__srv__read_file__2"]; got != "read_file" {
		t.Fatalf("disambiguated name maps to %q, want read_file (mapping: %v)", got, remote)
	}
}

// TestNewToolsDuplicateRemoteNameIsWrappedOnce：同一 (server, 原始名) 重复出现是
// 同一个工具，不应产生第二个（更不该变成 read_file__2）。
func TestNewToolsDuplicateRemoteNameIsWrappedOnce(t *testing.T) {
	client := &Client{}
	infos := []ToolInfo{{Name: "echo"}, {Name: "echo"}, {Name: "other"}}
	tools := NewTools(client, "srv", infos, "")
	if len(tools) != 2 {
		t.Fatalf("tools = %v, want the duplicated remote tool wrapped once", names(tools))
	}
}

// TestAggregateToolsAcrossServersAreUnique：两个 server 的可见 serverPart 清洗后
// 相同（"srv.a" 与 "srv_a"）时，跨 server 也不能产生重名。
func TestAggregateToolsAcrossServersAreUnique(t *testing.T) {
	servers := []ServerTools{
		{Client: &Client{}, Server: "srv.a", Tools: []ToolInfo{{Name: "read_file"}}},
		{Client: &Client{}, Server: "srv_a", Tools: []ToolInfo{{Name: "read_file"}}},
	}
	tools, collisions, err := AggregateTools(servers, "")
	if err != nil {
		t.Fatalf("AggregateTools: %v", err)
	}
	got := names(tools)
	t.Logf("names = %v collisions = %v", got, collisions)
	seen := map[string]bool{}
	for _, name := range got {
		if seen[name] {
			t.Fatalf("duplicate name %q across servers: %v", name, got)
		}
		seen[name] = true
	}
	if len(collisions) == 0 {
		t.Fatalf("expected a reported collision for srv.a/srv_a, got none (names: %v)", got)
	}
	collision := collisions[0]
	if collision.Server == "" || collision.RemoteName == "" || collision.AssignedName == "" {
		t.Fatalf("collision must carry server/original/assigned info: %+v", collision)
	}
}

// TestNewToolsCheckedReportsDiagnosableCollision：严格模式必须给出可诊断错误
// （原始 name/server/清洗后 name），而不是让上层 agent.New 抛一句 duplicate tool。
func TestNewToolsCheckedReportsDiagnosableCollision(t *testing.T) {
	client := &Client{}
	infos := []ToolInfo{{Name: "read.file"}, {Name: "read_file"}}
	tools, err := NewToolsChecked(client, "my-server", infos, "")
	if err == nil {
		t.Fatalf("NewToolsChecked = %v, want a collision error", names(tools))
	}
	text := err.Error()
	t.Logf("error = %v", text)
	for _, want := range []string{"my-server", "read.file", "read_file", "mcp__my-server__read_file"} {
		if !strings.Contains(text, want) {
			t.Fatalf("collision error %q does not mention %q", text, want)
		}
	}
	if strings.Contains(text, "duplicate tool") {
		t.Fatalf("collision error must be diagnosable, got %q", text)
	}
}

// TestNewToolsCheckedAcceptsCleanServers：没有冲突时不报错，名字与 NewTools 一致。
func TestNewToolsCheckedAcceptsCleanServers(t *testing.T) {
	client := &Client{}
	infos := []ToolInfo{{Name: "read_file"}, {Name: "write_file"}}
	checked, err := NewToolsChecked(client, "srv", infos, "")
	if err != nil {
		t.Fatalf("NewToolsChecked: %v", err)
	}
	if got, want := names(checked), names(NewTools(client, "srv", infos, "")); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("checked names = %v, want %v", got, want)
	}
}
