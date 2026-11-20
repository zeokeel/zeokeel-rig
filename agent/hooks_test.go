package agent_test

// 循环钩子（rig.TurnHook）是宿主接入 agent 循环内部边界的唯一缝：回合快照、
// 事件双写、成本记账、工具结果后处理（脱敏/追加提示）都发生在循环内部，宿主在
// Run 外面包一层是够不着的。
//
// 本文件钉住四件事：① 四个边界的调用顺序；② AfterTool 能改写结果，且改写对
// 模型可见（进历史 + 下一次请求）；③ BeforeRun 出错会中止本次运行；④ BeforeTool
// 出错会让这次调用失败但整轮继续（executor 未被调用）。

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
)

// recordingHook 记录边界事件，并可按需在边界上做决定。
type recordingHook struct {
	mu sync.Mutex

	entries []string

	beforeRunErr  error
	beforeToolErr error
	mutateTool    func(result *rig.ToolResult)
}

func (h *recordingHook) record(entry string) {
	h.mu.Lock()
	h.entries = append(h.entries, entry)
	h.mu.Unlock()
}

func (h *recordingHook) Entries() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.entries...)
}

func (h *recordingHook) BeforeRun(_ context.Context, _ rig.RunRequest) error {
	h.record("before-run")
	return h.beforeRunErr
}

func (h *recordingHook) AfterRun(_ context.Context, result rig.RunResult) {
	h.record("after-run:" + strings.Join(resultTexts(result), ","))
}

func (h *recordingHook) BeforeTool(_ context.Context, call rig.ToolCall) error {
	h.record("before-tool:" + call.Name)
	return h.beforeToolErr
}

func (h *recordingHook) AfterTool(_ context.Context, call rig.ToolCall, result *rig.ToolResult) {
	h.record("after-tool:" + call.Name)
	if h.mutateTool != nil {
		h.mutateTool(result)
	}
}

// resultTexts 把结果里的消息正文抽出来，方便断言 AfterRun 看到的是最终状态。
func resultTexts(result rig.RunResult) []string {
	texts := make([]string, 0, len(result.Messages))
	for _, message := range result.Messages {
		texts = append(texts, message.Text())
	}
	return texts
}

// TestTurnHookOrderAndResultRewrite 覆盖顺序与"改写对模型可见"。
func TestTurnHookOrderAndResultRewrite(t *testing.T) {
	provider := &scriptProvider{scripts: [][]rig.Chunk{
		toolCallScript("call_1", "echo", `{"text":"hi"}`),
		textScript("done"),
	}}
	echo := &fakeTool{
		name: "echo",
		fn: func(_ context.Context, call rig.ToolCall) (rig.ToolResult, error) {
			return rig.ToolResult{Content: "echoed:" + call.Arguments}, nil
		},
	}
	hook := &recordingHook{mutateTool: func(result *rig.ToolResult) {
		result.Content += "|hooked"
	}}
	a := mustNewAgent(t, rig.AgentSpec{Model: "m", Provider: provider, Tools: []rig.Tool{echo}, Hooks: []rig.TurnHook{hook}})

	result, err := a.Run(context.Background(), rig.RunRequest{Input: []rig.Message{userMessage("go")}})
	requireNoError(t, err)

	want := []string{
		"before-run",
		"before-tool:echo",
		"after-tool:echo",
		"after-run:" + strings.Join(resultTexts(result), ","),
	}
	got := hook.Entries()
	if len(got) != len(want) {
		t.Fatalf("钩子序列 = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("钩子[%d] = %q, want %q（全序列 %v）", index, got[index], want[index], got)
		}
	}

	// AfterTool 的改写必须进历史（模型下一轮看到的是改写后的内容）。
	var toolMessage string
	for _, message := range result.Messages {
		if message.Role == rig.RoleTool {
			toolMessage = message.Text()
		}
	}
	if !strings.Contains(toolMessage, "|hooked") {
		t.Fatalf("改写后的结果没有进历史：%q", toolMessage)
	}
	second := provider.request(1)
	if !strings.Contains(second.Messages[2].Text(), "|hooked") {
		t.Fatalf("改写后的结果没有进下一次请求：%q", second.Messages[2].Text())
	}
}

// TestTurnHookBeforeRunErrorAbortsRun 覆盖"BeforeRun 出错即中止"。
func TestTurnHookBeforeRunErrorAbortsRun(t *testing.T) {
	sentinel := errors.New("hook refused")
	provider := &scriptProvider{scripts: [][]rig.Chunk{textScript("should not run")}}
	hook := &recordingHook{beforeRunErr: sentinel}
	a := mustNewAgent(t, rig.AgentSpec{Model: "m", Provider: provider, Hooks: []rig.TurnHook{hook}})

	_, err := a.Run(context.Background(), rig.RunRequest{Input: []rig.Message{userMessage("go")}})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Run 返回 %v, want %v", err, sentinel)
	}
	if provider.callCount() != 0 {
		t.Fatalf("BeforeRun 失败后仍调用了 provider：%d 次", provider.callCount())
	}
}

// TestTurnHookBeforeToolErrorFailsTheCallOnly 覆盖"BeforeTool 出错只失败这一次调用"。
func TestTurnHookBeforeToolErrorFailsTheCallOnly(t *testing.T) {
	provider := &scriptProvider{scripts: [][]rig.Chunk{
		toolCallScript("call_1", "echo", `{}`),
		textScript("done"),
	}}
	echo := &fakeTool{name: "echo"}
	hook := &recordingHook{beforeToolErr: errors.New("denied by hook")}
	a := mustNewAgent(t, rig.AgentSpec{Model: "m", Provider: provider, Tools: []rig.Tool{echo}, Hooks: []rig.TurnHook{hook}})

	result, err := a.Run(context.Background(), rig.RunRequest{Input: []rig.Message{userMessage("go")}})
	requireNoError(t, err)
	if echo.callCount() != 0 {
		t.Fatalf("BeforeTool 失败后工具仍被执行：%d 次", echo.callCount())
	}
	var toolMessage string
	for _, message := range result.Messages {
		if message.Role == rig.RoleTool {
			toolMessage = message.Text()
		}
	}
	if !strings.Contains(toolMessage, "denied by hook") {
		t.Fatalf("工具消息应带上钩子的拒绝理由，实际 %q", toolMessage)
	}
	if result.Messages[len(result.Messages)-1].Text() != "done" {
		t.Fatal("轮次应当继续到模型给出最终答复")
	}
}

// panicAfterHook 在 AfterRun 里 panic，用于验证"钩子炸了不该吞掉已算好的结果"。
type panicAfterHook struct{ recordingHook }

func (h *panicAfterHook) AfterRun(context.Context, rig.RunResult) { panic("hook exploded") }

// TestTurnHookAfterRunRunsOnBeforeRunFailure 钉住"回合收尾一定发生"：
// 宿主用 BeforeRun 开回合（快照/成本区间）、用 AfterRun 收尾时，被 BeforeRun 拒绝
// 的那次运行也必须收到一次 AfterRun，否则回合永远开着。
func TestTurnHookAfterRunRunsOnBeforeRunFailure(t *testing.T) {
	sentinel := errors.New("nope")
	hook := &recordingHook{beforeRunErr: sentinel}
	provider := &scriptProvider{scripts: [][]rig.Chunk{textScript("x")}}
	a := mustNewAgent(t, rig.AgentSpec{Model: "m", Provider: provider, Hooks: []rig.TurnHook{hook}})

	if _, err := a.Run(context.Background(), rig.RunRequest{Input: []rig.Message{userMessage("go")}}); !errors.Is(err, sentinel) {
		t.Fatalf("Run 返回 %v, want %v", err, sentinel)
	}
	var afterRuns int
	for _, entry := range hook.Entries() {
		if strings.HasPrefix(entry, "after-run:") {
			afterRuns++
		}
	}
	if afterRuns != 1 {
		t.Fatalf("BeforeRun 失败后 AfterRun 调用 %d 次, want 1（回合必须收尾）：%v", afterRuns, hook.Entries())
	}
}

// TestTurnHookPanicDoesNotLoseResult 钉住"收尾钩子 panic 不改变调用方看到的结果"。
func TestTurnHookPanicDoesNotLoseResult(t *testing.T) {
	provider := &scriptProvider{scripts: [][]rig.Chunk{textScript("done")}}
	a := mustNewAgent(t, rig.AgentSpec{Model: "m", Provider: provider, Hooks: []rig.TurnHook{&panicAfterHook{}}})

	result, err := a.Run(context.Background(), rig.RunRequest{Input: []rig.Message{userMessage("go")}})
	requireNoError(t, err)
	if len(result.Messages) == 0 || result.Messages[len(result.Messages)-1].Text() != "done" {
		t.Fatalf("结果被钩子的 panic 弄丢了: %+v", result.Messages)
	}
}
