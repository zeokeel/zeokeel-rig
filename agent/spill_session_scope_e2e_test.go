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

// scopeRecordingStore 记下 Save / GC 时 ctx 上的作用域。
//
// 数据库（session/sqlite）后端的 GC 按作用域裁剪，作用域只能来自 ctx；文件后端
// 靠文件名前缀分区。少注入一层，Store 后端就退回"全局裁剪、跨会话互删句柄"。
type scopeRecordingStore struct{ scopes []string }

func (s *scopeRecordingStore) Save(ctx context.Context, _ rig.ToolCall, content string) (rig.SpillRef, error) {
	s.scopes = append(s.scopes, rig.SpillScope(ctx))
	return rig.SpillRef{ID: "ref-1", Bytes: len(content)}, nil
}

func (s *scopeRecordingStore) Load(context.Context, string) (string, error) { return "", nil }

func (s *scopeRecordingStore) GC(ctx context.Context, _ int) (int, error) {
	s.scopes = append(s.scopes, rig.SpillScope(ctx))
	return 0, nil
}

// TestAgentSpillStoreIsScopedToRunSession 是 Store 后端的 F-3 回归钉：agent 在调用
// spillToolResult 时注入的作用域必须是**本次 Run 的 SessionID**，Save 与随后按
// MaxFiles 触发的 GC 都要带同一个值。没有它，sqlite 的回收会删掉别的会话的行，
// 那些会话的 `ref:` 句柄从此读不回来（数据丢失）。
func TestAgentSpillStoreIsScopedToRunSession(t *testing.T) {
	store := &scopeRecordingStore{}
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
		Spill:    &rig.SpillPolicy{ThresholdBytes: 100, Store: store, MaxFiles: 4},
	})
	if _, err := a.Run(context.Background(), rig.RunRequest{SessionID: "sess-store",
		Input: []rig.Message{rig.NewMessage(rig.RoleUser, "go")}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 一次 Save + 一次 MaxFiles 触发的 GC。
	if len(store.scopes) != 2 {
		t.Fatalf("Store 收到 %d 次调用（作用域 %v），want 2 次（Save + GC）", len(store.scopes), store.scopes)
	}
	for index, scope := range store.scopes {
		if scope != "sess-store" {
			t.Fatalf("第 %d 次调用的作用域 = %q，want sess-store（Store 后端靠它分区回收）",
				index+1, scope)
		}
	}
}
