package agent_test

// F-3 回归钉：spill 必须用「本次 Run 的 SessionID」作为作用域，而不是空 ctx 的
// default。此前 agent.go 把 r.ctx 传给 spillToolResult，会话 ID 只在工具调用 ctx 上
// 注入 → 所有会话都落在 default 作用域，MaxFiles 仍会跨会话互删。

import (
	"context"
	"os"
	"strings"
	"testing"

	rig "github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/agent"
)

// TestAgentSpillUsesRunSessionScope：端到端——Run 产出的 spill 文件名必须带**本次
// SessionID** 的 scope 前缀，而不是空会话的 "default"。这是 F-3 的回归钉：会话 ID 只在
// 工具调用 ctx 上注入，spill 若用 r.ctx 就会永远落在 default 作用域、跨会话互删。
func TestAgentSpillUsesRunSessionScope(t *testing.T) {
	dir := t.TempDir()
	provider := &scriptProvider{scripts: [][]rig.Chunk{
		toolCallScript("call_1", "big", `{}`),
		textScript("done"),
	}}
	big := &fakeTool{name: "big", fn: func(context.Context, rig.ToolCall) (rig.ToolResult, error) {
		return rig.ToolResult{Content: strings.Repeat("payload ", 200)}, nil
	}}
	a := newAgentForSpillE2E(t, rig.AgentSpec{
		Model:    "m",
		Provider: provider,
		Tools:    []rig.Tool{big},
		Spill:    &rig.SpillPolicy{Dir: dir, ThresholdBytes: 100},
	})
	_, err := a.Run(context.Background(), rig.RunRequest{SessionID: "sess-wire",
		Input: []rig.Message{rig.NewMessage(rig.RoleUser, "go")}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("spill 文件数 = %d, want 1", len(entries))
	}
	name := entries[0].Name()
	if !strings.Contains(name, "sess-wire") {
		t.Fatalf("spill 文件名 = %q，不含会话 ID 作用域（说明用了 r.ctx 而非工具 ctx）", name)
	}
	if strings.Contains(name, "default") {
		t.Fatalf("spill 落在 default 作用域：%q", name)
	}
}

func newAgentForSpillE2E(t *testing.T, spec rig.AgentSpec) rig.Agent {
	t.Helper()
	a, err := agent.New(spec)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	return a
}
