package agent_test

// 本文件是 agent 层脱敏收口（AgentSpec 之外的 Redactor 注入，见 agent.NewWithRedactor）
// 的用例：工具结果、助手文本/推理、事件、错误四条出站通道，加上"入参不脱敏"、
// "nil 不改变任何字节"、"幂等"、"并发 Run"四个不变量。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/agent"
	"github.com/zeokeel/zeokeel-rig/secrets"
)

// redactSecret 命中内置的 sk- 规则（长度足够、前缀固定）。
const redactSecret = "sk-abcdefghijklmnopqrstuvwxyz0123456789"

// newRedactingAgent 用给定 spec 与脱敏器构造 agent。
func newRedactingAgent(tb testing.TB, spec rig.AgentSpec, redactor *secrets.Redactor) rig.Agent {
	tb.Helper()
	a, err := agent.NewWithRedactor(spec, redactor)
	if err != nil {
		tb.Fatalf("agent.NewWithRedactor: %v", err)
	}
	return a
}

// assertNoSecret 断言文本里不含秘密。
func assertNoSecret(tb testing.TB, where, text string) {
	tb.Helper()
	if strings.Contains(text, redactSecret) {
		tb.Fatalf("%s 泄露了明文凭据: %q", where, text)
	}
}

// toolMessageOf 返回一次 Run 里 role=tool 的消息。
func toolMessageOf(tb testing.TB, messages []rig.Message) rig.Message {
	tb.Helper()
	for _, msg := range messages {
		if msg.Role == rig.RoleTool {
			return msg
		}
	}
	tb.Fatal("没有找到 role=tool 的消息")
	return rig.Message{}
}

// ---------------------------------------------------------------------------
// 通道 1：工具结果回填
// ---------------------------------------------------------------------------

func TestRedactToolResultContent(t *testing.T) {
	provider := &scriptProvider{scripts: [][]rig.Chunk{
		toolCallScript("call_1", "leak", `{"q":"x"}`),
		textScript("done"),
	}}
	tool := &fakeTool{name: "leak", fn: func(context.Context, rig.ToolCall) (rig.ToolResult, error) {
		return rig.ToolResult{Content: "API_KEY=" + redactSecret}, nil
	}}
	history := newCaptureHistory(nil)
	rec := &recorder{}
	a := newRedactingAgent(t, rig.AgentSpec{
		Model: "m", Provider: provider, Tools: []rig.Tool{tool}, History: history,
	}, secrets.New())

	result, err := a.Run(context.Background(), rig.RunRequest{SessionID: "s1", Input: []rig.Message{userMessage("go")}, Emit: rec.emit})
	requireNoError(t, err)

	toolMessage := toolMessageOf(t, result.Messages)
	assertNoSecret(t, "RunResult 的 tool 消息", toolMessage.Text())
	if !strings.Contains(toolMessage.Text(), secrets.Marker) {
		t.Fatalf("tool 消息没有占位符: %q", toolMessage.Text())
	}
	stored := history.stored("s1")
	var storedTool *rig.Message
	for i := range stored {
		if stored[i].Role == rig.RoleTool {
			storedTool = &stored[i]
		}
	}
	if storedTool == nil {
		t.Fatal("历史里没有 tool 消息")
	}
	assertNoSecret(t, "History.Append 落盘的 tool 消息", storedTool.Text())
	// 事件通道同样脱敏。
	for _, event := range rec.ofType(rig.EventToolResult) {
		if event.Result == nil {
			t.Fatal("EventToolResult 没有 Result")
		}
		assertNoSecret(t, "EventToolResult.Content", event.Result.Content)
		if event.Result.IsError {
			t.Fatal("IsError 被改成了 true")
		}
	}
}

func TestRedactToolResultErrorText(t *testing.T) {
	provider := &scriptProvider{scripts: [][]rig.Chunk{
		toolCallScript("call_1", "boom", `{}`),
		textScript("done"),
	}}
	tool := &fakeTool{name: "boom", fn: func(context.Context, rig.ToolCall) (rig.ToolResult, error) {
		return rig.ToolResult{}, errors.New("upstream rejected key " + redactSecret)
	}}
	rec := &recorder{}
	a := newRedactingAgent(t, rig.AgentSpec{Model: "m", Provider: provider, Tools: []rig.Tool{tool}}, secrets.New())

	result, err := a.Run(context.Background(), rig.RunRequest{Input: []rig.Message{userMessage("go")}, Emit: rec.emit})
	requireNoError(t, err)

	toolMessage := toolMessageOf(t, result.Messages)
	assertNoSecret(t, "工具错误文本", toolMessage.Text())
	if !strings.Contains(toolMessage.Text(), "IsError") && !strings.Contains(toolMessage.Text(), "rig: tool") {
		t.Fatalf("错误文本被改得不可读: %q", toolMessage.Text())
	}
	events := rec.ofType(rig.EventToolResult)
	if len(events) != 1 || events[0].Result == nil {
		t.Fatalf("EventToolResult = %+v", events)
	}
	assertNoSecret(t, "EventToolResult", events[0].Result.Content)
	if !events[0].Result.IsError {
		t.Fatal("IsError 丢了")
	}
}

func TestRedactToolResultTextParts(t *testing.T) {
	imageURL := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUg"
	provider := &scriptProvider{scripts: [][]rig.Chunk{
		toolCallScript("call_1", "attach", `{}`),
		textScript("done"),
	}}
	tool := &fakeTool{name: "attach", fn: func(context.Context, rig.ToolCall) (rig.ToolResult, error) {
		return rig.ToolResult{
			Content: "ok",
			Parts: []rig.ContentPart{
				rig.TextPart("log line with " + redactSecret),
				rig.ImagePart(imageURL, "high"),
			},
		}, nil
	}}
	a := newRedactingAgent(t, rig.AgentSpec{Model: "m", Provider: provider, Tools: []rig.Tool{tool}}, secrets.New())

	result, err := a.Run(context.Background(), rig.RunRequest{Input: []rig.Message{userMessage("go")}})
	requireNoError(t, err)

	var attachment *rig.Message
	for i := range result.Messages {
		if result.Messages[i].Role == rig.RoleUser && len(result.Messages[i].Content) > 1 {
			attachment = &result.Messages[i]
		}
	}
	if attachment == nil {
		t.Fatalf("没有找到附件消息: %+v", result.Messages)
	}
	for _, part := range attachment.Content {
		assertNoSecret(t, "附件文本分片", part.Text)
	}
	// 图片分片必须原样保留（结构与内容都不动）。
	last := attachment.Content[len(attachment.Content)-1]
	if last.Type != rig.PartImageURL || last.ImageURL != imageURL || last.Detail != "high" {
		t.Fatalf("图片分片被改坏: %+v", last)
	}
}

// ---------------------------------------------------------------------------
// 通道 2：助手文本与推理
// ---------------------------------------------------------------------------

func TestRedactAssistantText(t *testing.T) {
	provider := &scriptProvider{scripts: [][]rig.Chunk{{
		{Delta: "here it is: " + redactSecret[:12]},
		{Delta: redactSecret[12:]},
		{FinishReason: "stop"},
	}}}
	history := newCaptureHistory(nil)
	rec := &recorder{}
	a := newRedactingAgent(t, rig.AgentSpec{Model: "m", Provider: provider, History: history}, secrets.New())

	result, err := a.Run(context.Background(), rig.RunRequest{SessionID: "s", Input: []rig.Message{userMessage("go")}, Emit: rec.emit})
	requireNoError(t, err)

	assertNoSecret(t, "assistant 消息", result.Messages[1].Text())
	assertNoSecret(t, "历史", history.stored("s")[1].Text())
	events := rec.ofType(rig.EventMessage)
	if len(events) != 1 || events[0].Message == nil {
		t.Fatalf("EventMessage = %+v", events)
	}
	assertNoSecret(t, "EventMessage", events[0].Message.Text())
}

func TestRedactAssistantReasoning(t *testing.T) {
	provider := &scriptProvider{scripts: [][]rig.Chunk{{
		{Reasoning: "the key is " + redactSecret},
		{Delta: "ok"},
		{FinishReason: "stop"},
	}}}
	rec := &recorder{}
	a := newRedactingAgent(t, rig.AgentSpec{Model: "m", Provider: provider}, secrets.New())

	result, err := a.Run(context.Background(), rig.RunRequest{Input: []rig.Message{userMessage("go")}, Emit: rec.emit})
	requireNoError(t, err)

	assertNoSecret(t, "RunResult.Reasoning", result.Messages[1].Reasoning)
	for _, event := range rec.ofType(rig.EventReasoningDelta) {
		assertNoSecret(t, "EventReasoningDelta", event.Reasoning)
	}
}

// ---------------------------------------------------------------------------
// 通道 3：增量事件
// ---------------------------------------------------------------------------

func TestRedactTextDeltaEvents(t *testing.T) {
	provider := &scriptProvider{scripts: [][]rig.Chunk{{
		{Delta: redactSecret},
		{Delta: " tail"},
		{FinishReason: "stop"},
	}}}
	rec := &recorder{}
	a := newRedactingAgent(t, rig.AgentSpec{Model: "m", Provider: provider}, secrets.New())

	_, err := a.Run(context.Background(), rig.RunRequest{Input: []rig.Message{userMessage("go")}, Emit: rec.emit})
	requireNoError(t, err)

	deltas := rec.ofType(rig.EventTextDelta)
	if len(deltas) != 2 {
		t.Fatalf("EventTextDelta 数量 = %d, want 2", len(deltas))
	}
	for _, event := range deltas {
		assertNoSecret(t, "EventTextDelta", event.Text)
	}
	if !strings.Contains(deltas[0].Text, secrets.Marker) {
		t.Fatalf("第一个 delta 没有占位符: %q", deltas[0].Text)
	}
	if deltas[1].Text != " tail" {
		t.Fatalf("无秘密的 delta 被改写: %q", deltas[1].Text)
	}
}

// ---------------------------------------------------------------------------
// 通道 4：错误
// ---------------------------------------------------------------------------

func TestRedactErrorText(t *testing.T) {
	sentinel := errors.New("upstream 400: body contained " + redactSecret)
	provider := &scriptProvider{fn: func(context.Context, rig.Request) (rig.Stream, error) {
		return nil, sentinel
	}}
	rec := &recorder{}
	a := newRedactingAgent(t, rig.AgentSpec{Model: "m", Provider: provider}, secrets.New())

	_, err := a.Run(context.Background(), rig.RunRequest{Input: []rig.Message{userMessage("go")}, Emit: rec.emit})
	if err == nil {
		t.Fatal("Run 应该返回错误")
	}
	assertNoSecret(t, "Run 返回的错误", err.Error())
	// 脱敏只改展示文本，包装链必须保留。
	if !errors.Is(err, sentinel) {
		t.Fatalf("errors.Is 失效: %v", err)
	}
	events := rec.ofType(rig.EventError)
	if len(events) != 1 || events[0].Err == nil {
		t.Fatalf("EventError = %+v", events)
	}
	assertNoSecret(t, "EventError", events[0].Err.Error())
}

// ---------------------------------------------------------------------------
// 不脱敏入参 + nil 不改变任何字节
// ---------------------------------------------------------------------------

func TestRedactToolCallArgumentsPreserved(t *testing.T) {
	arguments := `{"authorization":"Bearer ` + redactSecret + `"}`
	provider := &scriptProvider{scripts: [][]rig.Chunk{
		toolCallScript("call_1", "pass", arguments),
		textScript("done"),
	}}
	tool := &fakeTool{name: "pass", fn: func(context.Context, rig.ToolCall) (rig.ToolResult, error) {
		return rig.ToolResult{Content: "sent"}, nil
	}}
	rec := &recorder{}
	history := newCaptureHistory(nil)
	a := newRedactingAgent(t, rig.AgentSpec{
		Model: "m", Provider: provider, Tools: []rig.Tool{tool}, History: history,
	}, secrets.New())

	_, err := a.Run(context.Background(), rig.RunRequest{SessionID: "s", Input: []rig.Message{userMessage("go")}, Emit: rec.emit})
	requireNoError(t, err)

	// 工具必须收到原始入参（脱敏会破坏写 .env、带 Authorization 的 web_fetch 等用法）。
	if got := tool.lastCall(t).Arguments; got != arguments {
		t.Fatalf("工具收到的入参被改写:\n got: %s\nwant: %s", got, arguments)
	}
	// 历史里 assistant 的 tool_calls 也必须保留原文，否则重放会 400。
	var assistantMessage *rig.Message
	for i := range history.stored("s") {
		if msg := history.stored("s")[i]; msg.Role == rig.RoleAssistant && len(msg.ToolCalls) > 0 {
			assistantMessage = &msg
		}
	}
	if assistantMessage == nil {
		t.Fatal("历史里没有 assistant tool_calls 消息")
	}
	if got := assistantMessage.ToolCalls[0].Arguments; got != arguments {
		t.Fatalf("历史里的入参被改写: %s", got)
	}
	// 事件里的工具调用同样保留原文。
	calls := rec.ofType(rig.EventToolCall)
	if len(calls) != 1 || calls[0].Call == nil {
		t.Fatalf("EventToolCall = %+v", calls)
	}
	if calls[0].Call.Arguments != arguments {
		t.Fatalf("EventToolCall 的入参被改写: %s", calls[0].Call.Arguments)
	}
}

func TestRedactNilRedactorChangesNothing(t *testing.T) {
	provider := &scriptProvider{scripts: [][]rig.Chunk{
		toolCallScript("call_1", "leak", `{"q":"x"}`),
		textScript("answer " + redactSecret),
	}}
	tool := &fakeTool{name: "leak", fn: func(context.Context, rig.ToolCall) (rig.ToolResult, error) {
		return rig.ToolResult{Content: "raw " + redactSecret}, nil
	}}
	history := newCaptureHistory(nil)
	rec := &recorder{}
	a := newRedactingAgent(t, rig.AgentSpec{
		Model: "m", Provider: provider, Tools: []rig.Tool{tool}, History: history,
	}, nil)

	result, err := a.Run(context.Background(), rig.RunRequest{SessionID: "s", Input: []rig.Message{userMessage("go")}, Emit: rec.emit})
	requireNoError(t, err)

	// nil 脱敏器：一切逐字节不变（含明文）。
	toolMessage := toolMessageOf(t, result.Messages)
	if toolMessage.Text() != "raw "+redactSecret {
		t.Fatalf("tool 消息被改动: %q", toolMessage.Text())
	}
	var finalText string
	for _, msg := range result.Messages {
		if msg.Role == rig.RoleAssistant && msg.Text() != "" {
			finalText = msg.Text()
		}
	}
	if finalText != "answer "+redactSecret {
		t.Fatalf("assistant 消息被改动: %q", finalText)
	}
	if got := toolMessageOf(t, history.stored("s")).Text(); got != "raw "+redactSecret {
		t.Fatalf("历史被改动: %q", got)
	}
	deltas := rec.ofType(rig.EventTextDelta)
	var joined strings.Builder
	for _, event := range deltas {
		joined.WriteString(event.Text)
	}
	if joined.String() != "answer "+redactSecret {
		t.Fatalf("事件被改动: %q", joined.String())
	}
}

// ---------------------------------------------------------------------------
// 幂等：已脱敏的历史不会被二次脱敏
// ---------------------------------------------------------------------------

func TestRedactIdempotentExistingHistory(t *testing.T) {
	already := rig.NewMessage(rig.RoleUser, "old key was "+secrets.Marker)
	history := newCaptureHistory(map[string][]rig.Message{"s": {already}})
	provider := &scriptProvider{scripts: [][]rig.Chunk{textScript("fine")}}
	a := newRedactingAgent(t, rig.AgentSpec{Model: "m", Provider: provider, History: history}, secrets.New())

	_, err := a.Run(context.Background(), rig.RunRequest{SessionID: "s", Input: []rig.Message{userMessage("go")}})
	requireNoError(t, err)

	request := provider.request(0)
	if len(request.Messages) < 2 || request.Messages[0].Role != rig.RoleUser {
		t.Fatalf("请求消息 = %+v", request.Messages)
	}
	if got := request.Messages[0].Text(); got != "old key was "+secrets.Marker {
		t.Fatalf("已脱敏的消息被二次改写: %q", got)
	}
	// 历史本身也不该被重写（只追加）。
	stored := history.stored("s")
	if stored[0].Text() != "old key was "+secrets.Marker {
		t.Fatalf("历史被二次脱敏: %q", stored[0].Text())
	}
}

// ---------------------------------------------------------------------------
// 辅助函数：RedactMessages
// ---------------------------------------------------------------------------

func TestRedactMessagesHelper(t *testing.T) {
	redactor := secrets.New()
	t.Run("nil 脱敏器是 no-op", func(t *testing.T) {
		messages := []rig.Message{rig.NewMessage(rig.RoleAssistant, redactSecret)}
		agent.RedactMessages(nil, messages)
		if messages[0].Text() != redactSecret {
			t.Fatalf("nil 脱敏器改动了内容: %q", messages[0].Text())
		}
	})
	t.Run("文本分片与推理被改写，入参与图片原样", func(t *testing.T) {
		imageURL := "data:image/png;base64,AAA"
		messages := []rig.Message{{
			Role: rig.RoleAssistant,
			Content: []rig.ContentPart{
				rig.TextPart("token " + redactSecret),
				rig.TextPart("clean"),
				rig.ImagePart(imageURL, ""),
			},
			Reasoning: "reason about " + redactSecret,
			ToolCalls: []rig.ToolCall{{ID: "c1", Name: "t", Arguments: `{"k":"` + redactSecret + `"}`}},
		}}
		agent.RedactMessages(redactor, messages)
		msg := messages[0]
		assertNoSecret(t, "文本分片", msg.Content[0].Text)
		if msg.Content[1].Text != "clean" {
			t.Fatalf("无秘密分片被改写: %q", msg.Content[1].Text)
		}
		if msg.Content[2].Type != rig.PartImageURL || msg.Content[2].ImageURL != imageURL {
			t.Fatalf("图片分片被改写: %+v", msg.Content[2])
		}
		assertNoSecret(t, "推理", msg.Reasoning)
		if !strings.Contains(msg.ToolCalls[0].Arguments, redactSecret) {
			t.Fatalf("工具入参被改写: %q", msg.ToolCalls[0].Arguments)
		}
		if len(msg.Content) != 3 {
			t.Fatalf("分片数量变了: %d", len(msg.Content))
		}
	})
	t.Run("重复调用幂等", func(t *testing.T) {
		messages := []rig.Message{rig.NewMessage(rig.RoleTool, "x "+redactSecret)}
		agent.RedactMessages(redactor, messages)
		first := messages[0].Text()
		agent.RedactMessages(redactor, messages)
		if messages[0].Text() != first {
			t.Fatalf("二次脱敏改变了内容: %q -> %q", first, messages[0].Text())
		}
	})
	t.Run("空切片不 panic", func(t *testing.T) {
		agent.RedactMessages(redactor, nil)
		agent.RedactMessages(redactor, []rig.Message{})
	})
}

// ---------------------------------------------------------------------------
// spill 落盘同样脱敏
// ---------------------------------------------------------------------------

func TestRedactSpilledResultFile(t *testing.T) {
	dir := t.TempDir()
	provider := &scriptProvider{scripts: [][]rig.Chunk{
		toolCallScript("call_1", "big", `{}`),
		textScript("done"),
	}}
	tool := &fakeTool{name: "big", fn: func(context.Context, rig.ToolCall) (rig.ToolResult, error) {
		return rig.ToolResult{Content: strings.Repeat("line with "+redactSecret+"\n", 4)}, nil
	}}
	a := newRedactingAgent(t, rig.AgentSpec{
		Model: "m", Provider: provider, Tools: []rig.Tool{tool},
		Spill: &rig.SpillPolicy{Dir: dir, ThresholdBytes: 32},
	}, secrets.New())

	result, err := a.Run(context.Background(), rig.RunRequest{Input: []rig.Message{userMessage("go")}})
	requireNoError(t, err)

	toolMessage := toolMessageOf(t, result.Messages)
	if !rig.IsSpilledToolResult(toolMessage.Text()) {
		t.Fatalf("结果没有落盘: %q", toolMessage.Text())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("落盘文件数量 = %d, want 1", len(entries))
	}
	raw, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	assertNoSecret(t, "落盘文件", string(raw))
	if !strings.Contains(string(raw), secrets.Marker) {
		t.Fatalf("落盘文件没有占位符: %q", string(raw))
	}
}

// ---------------------------------------------------------------------------
// 完成通知（NoticeSource）同样脱敏
// ---------------------------------------------------------------------------

type noticeTool struct {
	*fakeTool
	notices []string
}

func (t *noticeTool) PendingNotices() []string {
	notices := t.notices
	t.notices = nil
	return notices
}

func TestRedactNoticeText(t *testing.T) {
	provider := &scriptProvider{scripts: [][]rig.Chunk{
		toolCallScript("call_1", "notice", `{}`),
		textScript("done"),
	}}
	tool := &noticeTool{
		fakeTool: &fakeTool{name: "notice", fn: func(context.Context, rig.ToolCall) (rig.ToolResult, error) {
			return rig.ToolResult{Content: "ok"}, nil
		}},
		notices: []string{"job finished with " + redactSecret},
	}
	history := newCaptureHistory(nil)
	a := newRedactingAgent(t, rig.AgentSpec{
		Model: "m", Provider: provider, Tools: []rig.Tool{tool}, History: history,
	}, secrets.New())

	_, err := a.Run(context.Background(), rig.RunRequest{SessionID: "s", Input: []rig.Message{userMessage("go")}})
	requireNoError(t, err)

	found := false
	for _, msg := range history.stored("s") {
		if strings.Contains(msg.Text(), "Background notifications:") {
			found = true
			assertNoSecret(t, "完成通知消息", msg.Text())
			if !strings.Contains(msg.Text(), secrets.Marker) {
				t.Fatalf("通知没有占位符: %q", msg.Text())
			}
		}
	}
	if !found {
		t.Fatalf("历史里没有通知消息: %+v", history.stored("s"))
	}
}

// ---------------------------------------------------------------------------
// 上下文压缩结果同样脱敏
// ---------------------------------------------------------------------------

type secretCompactor struct {
	calls int
}

func (c *secretCompactor) Compact(_ context.Context, _ string, _ []rig.Message, _ int) ([]rig.Message, error) {
	c.calls++
	return []rig.Message{rig.NewMessage(rig.RoleUser, "summary mentions "+redactSecret)}, nil
}

func TestRedactCompactedContext(t *testing.T) {
	provider := &scriptProvider{scripts: [][]rig.Chunk{
		toolCallScript("call_1", "echo", `{"text":"hi"}`),
		textScript("done"),
	}}
	tool := &fakeTool{name: "echo"}
	compactor := &secretCompactor{}
	a := newRedactingAgent(t, rig.AgentSpec{
		Model: "m", Provider: provider, Tools: []rig.Tool{tool},
		Compactor: compactor, ContextBudget: 1,
	}, secrets.New())

	_, err := a.Run(context.Background(), rig.RunRequest{Input: []rig.Message{userMessage("go")}})
	requireNoError(t, err)
	if compactor.calls == 0 {
		t.Fatal("压缩器没有被调用")
	}
	// 第二次请求的消息里不该有明文。
	if provider.callCount() < 2 {
		t.Fatalf("provider 调用次数 = %d", provider.callCount())
	}
	for _, msg := range provider.request(1).Messages {
		assertNoSecret(t, "压缩后的请求消息", msg.Text())
	}
}

// ---------------------------------------------------------------------------
// 并发：多个 Run 共享一个 Redactor，同时 Add
// ---------------------------------------------------------------------------

func TestRedactConcurrentRunsAndAdd(t *testing.T) {
	const runs = 4
	redactor := secrets.New()
	provider := &scriptProvider{repeatLast: true, scripts: [][]rig.Chunk{
		toolCallScript("call_1", "leak", `{}`),
	}}
	// 每个 Run 需要两个脚本（工具轮 + 收尾）；repeatLast 让 provider 复用最后一个，
	// 因此用一个会返回文本的工具轮脚本即可。
	provider.scripts = [][]rig.Chunk{
		toolCallScript("call_1", "leak", `{}`),
		textScript("done"),
	}
	tool := &fakeTool{name: "leak", fn: func(context.Context, rig.ToolCall) (rig.ToolResult, error) {
		return rig.ToolResult{Content: "leaked " + redactSecret}, nil
	}}
	a := newRedactingAgent(t, rig.AgentSpec{Model: "m", Provider: provider, Tools: []rig.Tool{tool}}, redactor)

	var wg sync.WaitGroup
	var addWG sync.WaitGroup
	addWG.Go(func() {
		for i := 0; i < 50; i++ {
			redactor.Add("known-" + strings.Repeat("k", i+8))
			redactor.AddNamed("n", "named-"+strings.Repeat("m", i+8))
		}
	})
	for run := 0; run < runs; run++ {
		wg.Go(func() {
			for attempt := 0; attempt < 5; attempt++ {
				result, err := a.Run(context.Background(), rig.RunRequest{Input: []rig.Message{userMessage("go")}})
				if err != nil {
					t.Errorf("Run: %v", err)
					return
				}
				for _, msg := range result.Messages {
					if strings.Contains(msg.Text(), redactSecret) {
						t.Errorf("并发运行泄露: %q", msg.Text())
						return
					}
				}
			}
		})
	}
	wg.Wait()
	addWG.Wait()
}

// ---------------------------------------------------------------------------
// subagent：RunnerOptions.Redactor 注入子 agent
// ---------------------------------------------------------------------------

func TestRedactSubagentInheritsRunnerRedactor(t *testing.T) {
	provider := &scriptProvider{repeatLast: true, scripts: [][]rig.Chunk{textScript("child says " + redactSecret)}}
	parent := rig.AgentSpec{Model: "m", Provider: provider}
	runner, err := agent.NewRunner(parent, agent.RunnerOptions{Redactor: secrets.New(), MaxDepth: 2})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	child, err := runner.Start(context.Background(), rig.SubagentSpec{Name: "kid", Prompt: "hi"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = child.Stop() }()
	result, err := child.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	assertNoSecret(t, "子 agent 的输出", result.Output)
	if !strings.Contains(result.Output, secrets.Marker) {
		t.Fatalf("子 agent 输出没有占位符: %q", result.Output)
	}
}
