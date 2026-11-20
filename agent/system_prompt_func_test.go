package agent_test

// 系统提示词可以按运行计算（rig.AgentSpec.SystemPromptFunc）：环境块里的日期、
// git 分支、当前工作区这类内容会在长驻进程里过期，而固定字符串是在构造时冻结的。
//
// 本用例钉住：① 每个 Run 调用一次；② 每次运行看到的是本次算出来的值（不是构造
// 时那一次）；③ 返回空串时等同没有系统提示词。

import (
	"context"
	"strconv"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
)

func TestSystemPromptFuncIsEvaluatedPerRun(t *testing.T) {
	provider := &scriptProvider{scripts: [][]rig.Chunk{
		textScript("first"),
		textScript("second"),
	}}
	calls := 0
	a := mustNewAgent(t, rig.AgentSpec{
		Model:    "m",
		Provider: provider,
		SystemPromptFunc: func(_ context.Context, _ rig.RunRequest) string {
			calls++
			return "env: run-" + strconv.Itoa(calls)
		},
	})

	if _, err := a.Run(context.Background(), rig.RunRequest{Input: []rig.Message{userMessage("one")}}); err != nil {
		t.Fatalf("第一次 Run: %v", err)
	}
	if _, err := a.Run(context.Background(), rig.RunRequest{Input: []rig.Message{userMessage("two")}}); err != nil {
		t.Fatalf("第二次 Run: %v", err)
	}

	if calls != 2 {
		t.Fatalf("SystemPromptFunc 调用次数 = %d, want 2（每次 Run 一次）", calls)
	}
	if got := provider.request(0).Messages[0]; got.Role != rig.RoleSystem || got.Text() != "env: run-1" {
		t.Fatalf("第一次请求的系统提示词 = %q/%q", got.Role, got.Text())
	}
	if got := provider.request(1).Messages[0]; got.Role != rig.RoleSystem || got.Text() != "env: run-2" {
		t.Fatalf("第二次请求的系统提示词 = %q/%q（应当是本次运行算出来的）", got.Role, got.Text())
	}
}

// TestSystemPromptFuncEmptyFallsBackToNothing 钉住"返回空串就是没有系统提示词"。
func TestSystemPromptFuncEmptyFallsBackToNothing(t *testing.T) {
	provider := &scriptProvider{scripts: [][]rig.Chunk{textScript("done")}}
	a := mustNewAgent(t, rig.AgentSpec{
		Model:            "m",
		Provider:         provider,
		SystemPrompt:     "static prompt",
		SystemPromptFunc: func(context.Context, rig.RunRequest) string { return "" },
	})
	if _, err := a.Run(context.Background(), rig.RunRequest{Input: []rig.Message{userMessage("hi")}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	first := provider.request(0).Messages[0]
	if first.Role == rig.RoleSystem {
		t.Fatalf("SystemPromptFunc 返回空串时不该再有系统消息，实际 %q", first.Text())
	}
}
