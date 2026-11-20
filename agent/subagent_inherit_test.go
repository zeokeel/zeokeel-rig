package agent_test

// 子 agent 的提示词与钩子继承语义。
//
// 子 spec 是父 spec 的浅拷贝，所以父上的"按运行计算的系统提示词"与"循环钩子"会
// 被一起带过去。两件事都不能默默发生：
//   - SystemPromptFunc 优先级最高，会把 SubagentSpec.SystemPrompt / SystemPromptAppend /
//     structured-output 指令全部盖掉（子 agent 拿不到自己该有的提示词）；
//   - 父的 TurnHook 被带进子 agent 后，父会话的"回合快照/成本记账"会为子 agent
//     的 Run 额外触发一次。
//
// 语义定为：**两者都不继承**——子 agent 的提示词由子 spec 自己决定，钩子由宿主
// 在需要时显式传（目前没有对应 RunnerOptions，因此默认不触发）。

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/agent"
)

// TestSubagentDoesNotInheritParentSystemPromptFunc 钉住"子 agent 用自己的提示词"。
func TestSubagentDoesNotInheritParentSystemPromptFunc(t *testing.T) {
	provider := &scriptProvider{scripts: [][]rig.Chunk{textScript("child-done")}}
	runner, err := agent.NewRunner(rig.AgentSpec{
		Model:            "m",
		Provider:         provider,
		SystemPromptFunc: func(context.Context, rig.RunRequest) string { return "FUNC-PARENT" },
	}, agent.DefaultRunnerOptions())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	ref, err := runner.Start(context.Background(), rig.SubagentSpec{
		Prompt:       "do the thing",
		SystemPrompt: "CHILD-OVERRIDE",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := ref.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	// 子 agent 的请求：系统消息必须是子自己的提示词。
	request := provider.request(0)
	if len(request.Messages) == 0 || request.Messages[0].Role != rig.RoleSystem {
		t.Fatalf("子 agent 的请求没有系统消息: %+v", request.Messages)
	}
	if got := request.Messages[0].Text(); got != "CHILD-OVERRIDE" {
		t.Fatalf("子 agent 的系统提示词 = %q, want CHILD-OVERRIDE（父的 SystemPromptFunc 不该盖掉它）", got)
	}
}

// recordingHookForSubagent 记录被触发过的会话。
type recordingHookForSubagent struct {
	mu       sync.Mutex
	sessions []string
}

func (h *recordingHookForSubagent) BeforeRun(_ context.Context, req rig.RunRequest) error {
	h.mu.Lock()
	h.sessions = append(h.sessions, req.SessionID)
	h.mu.Unlock()
	return nil
}
func (h *recordingHookForSubagent) AfterRun(context.Context, rig.RunResult)        {}
func (h *recordingHookForSubagent) BeforeTool(context.Context, rig.ToolCall) error { return nil }
func (h *recordingHookForSubagent) AfterTool(context.Context, rig.ToolCall, *rig.ToolResult) {
}

// TestSubagentDoesNotInheritParentHooks 钉住"钩子不跟着浅拷贝跑进子 agent"。
func TestSubagentDoesNotInheritParentHooks(t *testing.T) {
	provider := &scriptProvider{scripts: [][]rig.Chunk{textScript("child-done")}}
	parent := &recordingHookForSubagent{}
	runner, err := agent.NewRunner(rig.AgentSpec{
		Model:    "m",
		Provider: provider,
		Hooks:    []rig.TurnHook{parent},
	}, agent.DefaultRunnerOptions())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	ref, err := runner.Start(context.Background(), rig.SubagentSpec{Prompt: "do it"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := ref.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	for _, session := range parent.sessions {
		if strings.HasPrefix(session, "subagent") {
			t.Fatalf("父的钩子被带进了子 agent（session=%q）：宿主会为同一件事记两次账", session)
		}
	}
	if len(parent.sessions) != 0 {
		t.Fatalf("父钩子不该被子 agent 触发，实际收到 %v", parent.sessions)
	}
}
