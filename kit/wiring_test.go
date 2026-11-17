package kit_test

// kit 新接线的集成测试：SessionStoreDir / Instructions+Environment / Redactor /
// UndoTracker / EventWriter，以及"事件回调不吞调用方回调"的顺序契约。
//
// 每个用例都同时包含一次**反向对照**（或独立的 negative-control 用例），确保它
// 真的能失败：持久化用例配"内存历史不持久"；脱敏用例配"不挂 Redactor 就泄露"；
// 事件用例配"写失败仍成功 + EventError 非 nil"。

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/events"
	"github.com/zeokeel/zeokeel-rig/gate"
	"github.com/zeokeel/zeokeel-rig/instructions"
	"github.com/zeokeel/zeokeel-rig/kit"
	"github.com/zeokeel/zeokeel-rig/secrets"
	"github.com/zeokeel/zeokeel-rig/session"
	"github.com/zeokeel/zeokeel-rig/session/store"
	"github.com/zeokeel/zeokeel-rig/tools/undo"
)

// wiringContext 返回一个带超时的 context，避免任何接线 bug 让测试挂死。
func wiringContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// wiringMessageWith 返回第一条包含 want 的消息文本（找不到返回空串）。
func wiringMessageWith(messages []kit.Message, want string) string {
	for _, message := range messages {
		if strings.Contains(message.Text(), want) {
			return message.Text()
		}
	}
	return ""
}

// wiringEchoTool 记录收到的原始参数并回显其中指定字段；用于验证"输出脱敏、入参
// 不脱敏"。
type wiringEchoTool struct {
	name string

	mu    sync.Mutex
	calls []kit.ToolCall
}

func (t *wiringEchoTool) Name() string           { return t.name }
func (t *wiringEchoTool) Description() string    { return "echo the arguments back" }
func (t *wiringEchoTool) Schema() map[string]any { return map[string]any{"type": "object"} }

func (t *wiringEchoTool) Invoke(_ context.Context, call kit.ToolCall) (kit.ToolResult, error) {
	t.mu.Lock()
	t.calls = append(t.calls, call)
	t.mu.Unlock()
	return kit.ToolResult{Content: "echo: " + call.Arguments}, nil
}

func (t *wiringEchoTool) lastArguments() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.calls) == 0 {
		return ""
	}
	return t.calls[len(t.calls)-1].Arguments
}

// wiringFailWriter 每次写都失败，用来验证事件流故障不影响主循环。
type wiringFailWriter struct{}

func (wiringFailWriter) Write([]byte) (int, error) { return 0, errors.New("event sink exploded") }

// ---------------------------------------------------------------------------
// 1. SessionStoreDir
// ---------------------------------------------------------------------------

// 同一目录重启后必须能读回上一轮消息（store 落盘，不是内存）。
func TestWiringSessionStorePersistsAcrossRestart(t *testing.T) {
	ctx := wiringContext(t)
	root := t.TempDir()
	storeDir := filepath.Join(t.TempDir(), "sessions")

	first := &scriptProvider{scripts: [][]kit.Chunk{textScript("alpha remembered")}}
	agent1 := mustNew(t, kit.Config{Model: "m", Provider: first, Root: root, SessionStoreDir: storeDir})
	if text, _, err := agent1.RunText(ctx, "s1", "remember alpha", nil); err != nil {
		t.Fatalf("first RunText: %v", err)
	} else if !strings.Contains(text, "alpha remembered") {
		t.Fatalf("first run text = %q", text)
	}

	// 第二次 New 打开同一个目录：相当于进程重启。
	second := &scriptProvider{scripts: [][]kit.Chunk{textScript("you said alpha")}}
	agent2 := mustNew(t, kit.Config{Model: "m", Provider: second, Root: root, SessionStoreDir: storeDir})
	if _, _, err := agent2.RunText(ctx, "s1", "what did I say", nil); err != nil {
		t.Fatalf("second RunText: %v", err)
	}

	request := second.request(0)
	if got := wiringMessageWith(request.Messages, "remember alpha"); got == "" {
		t.Fatalf("重启后的第一次请求里没有上一轮的用户消息: %+v", request.Messages)
	}
	if got := wiringMessageWith(request.Messages, "alpha remembered"); got == "" {
		t.Fatalf("重启后的第一次请求里没有上一轮的助手消息: %+v", request.Messages)
	}

	// 再从 store 侧确认：消息确实落在磁盘上，而不是被 provider 的请求缓存掩盖。
	reopened, err := store.NewDir(storeDir)
	if err != nil {
		t.Fatalf("store.NewDir: %v", err)
	}
	history := reopened.History(ctx, "default")
	messages, err := history.Load(ctx, "s1")
	if err != nil {
		t.Fatalf("history.Load: %v", err)
	}
	if got := wiringMessageWith(messages, "remember alpha"); got == "" {
		t.Fatalf("store 里没有第一轮的用户消息: %+v", messages)
	}
}

// positive control：不配置 SessionStoreDir（默认内存历史）时，重启就看不到旧消息。
// 只有这条能过，上一条的"读回"才是 store 的功劳。
func TestWiringInMemoryHistoryDoesNotPersistAcrossRestart(t *testing.T) {
	ctx := wiringContext(t)
	root := t.TempDir()

	first := &scriptProvider{scripts: [][]kit.Chunk{textScript("alpha remembered")}}
	agent1 := mustNew(t, kit.Config{Model: "m", Provider: first, Root: root})
	if _, _, err := agent1.RunText(ctx, "s1", "remember alpha", nil); err != nil {
		t.Fatalf("first RunText: %v", err)
	}

	second := &scriptProvider{scripts: [][]kit.Chunk{textScript("no idea")}}
	agent2 := mustNew(t, kit.Config{Model: "m", Provider: second, Root: root})
	if _, _, err := agent2.RunText(ctx, "s1", "what did I say", nil); err != nil {
		t.Fatalf("second RunText: %v", err)
	}
	if got := wiringMessageWith(second.request(0).Messages, "remember alpha"); got != "" {
		t.Fatalf("内存历史不该跨实例存活，却读到了: %q", got)
	}
}

// History 与 SessionStoreDir 互斥：同时设置必须报错，而不是悄悄忽略一个。
func TestWiringSessionStoreRejectsExplicitHistory(t *testing.T) {
	_, err := kit.New(kit.Config{
		Model:           "m",
		Provider:        &scriptProvider{scripts: [][]kit.Chunk{textScript("x")}},
		Root:            t.TempDir(),
		History:         session.NewMemory(),
		SessionStoreDir: filepath.Join(t.TempDir(), "sessions"),
	})
	if err == nil {
		t.Fatal("kit.New accepted both History and SessionStoreDir")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("error = %v, want a mutual-exclusion error", err)
	}
}

// ---------------------------------------------------------------------------
// 2. Redactor
// ---------------------------------------------------------------------------

// 工具输出必须脱敏，工具入参必须保持原文（脱敏是"出站"方向，不能改写模型给工具
// 的输入）。
func TestWiringRedactorRedactsOutputNotInput(t *testing.T) {
	ctx := wiringContext(t)
	const secret = "sk-abcdefghijklmnopqrstuv"
	root := t.TempDir()
	tool := &wiringEchoTool{name: "echo_secret"}
	provider := &scriptProvider{scripts: [][]kit.Chunk{
		toolCallScript(kit.ToolCall{ID: "call_1", Name: "echo_secret", Arguments: `{"token":"` + secret + `"}`}),
		textScript("done"),
	}}
	agent := mustNew(t, kit.Config{
		Model:    "m",
		Provider: provider,
		Root:     root,
		Tools:    []kit.Tool{tool},
		Gate:     gate.AllowAll(),
		Redactor: secrets.New(secret),
	})
	if _, _, err := agent.RunText(ctx, "s1", "echo the token", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}

	message, ok := findToolMessage(provider.request(1).Messages, "echo_secret")
	if !ok {
		t.Fatalf("模型没有收到工具结果: %+v", provider.request(1).Messages)
	}
	if strings.Contains(message.Text(), secret) {
		t.Fatalf("模型看到的工具结果仍是明文: %q", message.Text())
	}
	if !strings.Contains(message.Text(), "[REDACTED]") {
		t.Fatalf("工具结果 = %q, want the redaction marker", message.Text())
	}

	if got := tool.lastArguments(); !strings.Contains(got, secret) {
		t.Fatalf("工具收到的入参被改写了: %q（脱敏只能作用于出站内容）", got)
	}
}

// negative control：不挂 Redactor 时同一条路会把明文交给模型——证明上一条的绿是
// 接线生效，而不是"根本没走到"。
func TestWiringWithoutRedactorLeaksSecret(t *testing.T) {
	ctx := wiringContext(t)
	const secret = "sk-abcdefghijklmnopqrstuv"
	tool := &wiringEchoTool{name: "echo_secret"}
	provider := &scriptProvider{scripts: [][]kit.Chunk{
		toolCallScript(kit.ToolCall{ID: "call_1", Name: "echo_secret", Arguments: `{"token":"` + secret + `"}`}),
		textScript("done"),
	}}
	agent := mustNew(t, kit.Config{
		Model:    "m",
		Provider: provider,
		Root:     t.TempDir(),
		Tools:    []kit.Tool{tool},
		Gate:     gate.AllowAll(),
	})
	if _, _, err := agent.RunText(ctx, "s1", "echo the token", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	message, ok := findToolMessage(provider.request(1).Messages, "echo_secret")
	if !ok {
		t.Fatalf("模型没有收到工具结果")
	}
	if !strings.Contains(message.Text(), secret) {
		t.Fatalf("未挂 Redactor 时结果 = %q, want the raw secret（对照失败说明模型根本没拿到结果）", message.Text())
	}
}

// ---------------------------------------------------------------------------
// 3. Instructions + Environment
// ---------------------------------------------------------------------------

// wiringInstructions 在 root 下造一个 git 仓库 + AGENTS.md，并 Load 出 Set。
func wiringInstructions(t *testing.T, root string, content string) *instructions.Set {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write AGENTS.md: %v", err)
	}
	set, err := instructions.Load(root, instructions.WithUserPath(""))
	if err != nil {
		t.Fatalf("instructions.Load: %v", err)
	}
	return &set
}

// wiringEnvironment 返回时间固定的 Environment（保证跨次构建字节一致）。
func wiringEnvironment(root string) *instructions.Environment {
	return &instructions.Environment{
		WorkspaceRoot: root,
		Cwd:           root,
		OS:            "linux",
		Arch:          "amd64",
		Time:          time.Date(2026, 11, 17, 9, 30, 0, 0, time.UTC),
		GitBranch:     "main",
		TempDir:       "/tmp",
	}
}

// 系统提示词必须是 base → <environment> → AGENTS.md 的顺序，且三部分都在。
func TestWiringSystemPromptComposition(t *testing.T) {
	ctx := wiringContext(t)
	root := t.TempDir()
	set := wiringInstructions(t, root, "PROJECT RULES\n")
	provider := &scriptProvider{scripts: [][]kit.Chunk{textScript("ok")}}
	agent := mustNew(t, kit.Config{
		Model:        "m",
		Provider:     provider,
		Root:         root,
		SystemPrompt: "BASE PERSONA",
		Environment:  wiringEnvironment(root),
		Instructions: set,
	})
	if _, _, err := agent.RunText(ctx, "s1", "hi", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}

	messages := provider.request(0).Messages
	if len(messages) == 0 || messages[0].Role != kit.RoleSystem {
		t.Fatalf("第一条消息不是 system: %+v", messages)
	}
	prompt := messages[0].Text()
	if !strings.HasPrefix(prompt, "BASE PERSONA") {
		t.Fatalf("system prompt 没有以 base 开头: %q", prompt)
	}
	for _, want := range []string{"<environment>", "## AGENTS.md (source: root)", "PROJECT RULES", "platform: linux/amd64"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("system prompt 缺少 %q:\n%s", want, prompt)
		}
	}
	baseAt := strings.Index(prompt, "BASE PERSONA")
	envAt := strings.Index(prompt, "<environment>")
	agentsAt := strings.Index(prompt, "## AGENTS.md")
	if !(baseAt < envAt && envAt < agentsAt) {
		t.Fatalf("顺序不是 base → environment → instructions: base=%d env=%d agents=%d\n%s", baseAt, envAt, agentsAt, prompt)
	}
}

// 同样的配置再建一次，system prompt 必须逐字节相同（prompt 缓存友好的前提）。
func TestWiringSystemPromptIsStableAcrossRebuild(t *testing.T) {
	root := t.TempDir()
	set := wiringInstructions(t, root, "PROJECT RULES\n")
	build := func() string {
		provider := &scriptProvider{scripts: [][]kit.Chunk{textScript("ok")}}
		agent := mustNew(t, kit.Config{
			Model:        "m",
			Provider:     provider,
			Root:         root,
			SystemPrompt: "BASE PERSONA",
			Environment:  wiringEnvironment(root),
			Instructions: set,
		})
		if _, _, err := agent.RunText(wiringContext(t), "s1", "hi", nil); err != nil {
			t.Fatalf("RunText: %v", err)
		}
		return provider.request(0).Messages[0].Text()
	}
	first, second := build(), build()
	if first != second {
		t.Fatalf("两次构建的 system prompt 不同:\nfirst  %q\nsecond %q", first, second)
	}
	if !strings.Contains(first, "## AGENTS.md (source: root)") {
		t.Fatalf("prompt = %q, want the instructions block", first)
	}
}

// 只配置 Instructions 时不应出现环境块（避免宿主误以为两条线是绑定的）。
func TestWiringEnvironmentOptional(t *testing.T) {
	ctx := wiringContext(t)
	root := t.TempDir()
	set := wiringInstructions(t, root, "PROJECT RULES\n")
	provider := &scriptProvider{scripts: [][]kit.Chunk{textScript("ok")}}
	agent := mustNew(t, kit.Config{Model: "m", Provider: provider, Root: root, Instructions: set})
	if _, _, err := agent.RunText(ctx, "s1", "hi", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	prompt := provider.request(0).Messages[0].Text()
	if strings.Contains(prompt, "<environment>") {
		t.Fatalf("未配置 Environment 却出现了环境块: %q", prompt)
	}
	if !strings.Contains(prompt, "## AGENTS.md (source: root)") {
		t.Fatalf("prompt = %q, want the instructions block", prompt)
	}
}

// ---------------------------------------------------------------------------
// 4. UndoTracker
// ---------------------------------------------------------------------------

// 每次 Run 都要在工具执行前自动 Begin（回合号递增）。
func TestWiringUndoTrackerBeginsEachRun(t *testing.T) {
	ctx := wiringContext(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	tracker := undo.NewTracker(root, undo.Options{})
	provider := &scriptProvider{scripts: [][]kit.Chunk{
		toolCallScript(kit.ToolCall{ID: "call_1", Name: "echo", Arguments: `{"x":1}`}),
		textScript("done"),
		textScript("again"),
	}}
	tool := &wiringEchoTool{name: "echo"}
	agent := mustNew(t, kit.Config{
		Model:       "m",
		Provider:    provider,
		Root:        root,
		Tools:       []kit.Tool{tool},
		Gate:        gate.AllowAll(),
		UndoTracker: tracker,
	})
	if got := tracker.Turns(); len(got) != 0 {
		t.Fatalf("Turns() before any run = %v, want empty", got)
	}
	if _, _, err := agent.RunText(ctx, "s1", "first", nil); err != nil {
		t.Fatalf("first RunText: %v", err)
	}
	if got, want := tracker.Turns(), []int{1}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("Turns() after first run = %v, want %v", got, want)
	}
	if _, _, err := agent.RunText(ctx, "s2", "second", nil); err != nil {
		t.Fatalf("second RunText: %v", err)
	}
	if got := tracker.Turns(); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("Turns() after second run = %v, want [1 2]", got)
	}
}

// Begin 失败（root 不存在/不可读）只应记一条事件，不能影响本轮运行。
func TestWiringUndoBeginFailureDoesNotBreakRun(t *testing.T) {
	ctx := wiringContext(t)
	root := t.TempDir()
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	tracker := undo.NewTracker(missing, undo.Options{})
	var eventsBuffer bytes.Buffer
	provider := &scriptProvider{scripts: [][]kit.Chunk{textScript("still worked")}}
	agent := mustNew(t, kit.Config{
		Model:       "m",
		Provider:    provider,
		Root:        root,
		UndoTracker: tracker,
		EventWriter: &eventsBuffer,
	})
	text, _, err := agent.RunText(ctx, "s1", "hi", nil)
	if err != nil {
		t.Fatalf("RunText = %v, want success despite a failed undo snapshot", err)
	}
	if !strings.Contains(text, "still worked") {
		t.Fatalf("text = %q", text)
	}
	if got := tracker.Turns(); len(got) != 0 {
		t.Fatalf("Turns() = %v, want empty (Begin must have failed)", got)
	}
	if !strings.Contains(eventsBuffer.String(), "undo snapshot failed") {
		t.Fatalf("事件流里没有 undo 失败提示:\n%s", eventsBuffer.String())
	}
	if agent.EventError() != nil {
		t.Fatalf("EventError() = %v, want nil（undo 失败不是写失败）", agent.EventError())
	}
}

// ---------------------------------------------------------------------------
// 5. EventWriter
// ---------------------------------------------------------------------------

// 事件流必须是可解析的 NDJSON，seq 从 1 递增，且含 tool.call / tool.result。
func TestWiringEventWriterEmitsValidNDJSON(t *testing.T) {
	ctx := wiringContext(t)
	root := t.TempDir()
	var buffer bytes.Buffer
	tool := &wiringEchoTool{name: "echo"}
	provider := &scriptProvider{scripts: [][]kit.Chunk{
		toolCallScript(kit.ToolCall{ID: "call_1", Name: "echo", Arguments: `{"x":1}`}),
		textScript("done"),
	}}
	agent := mustNew(t, kit.Config{
		Model:       "m",
		Provider:    provider,
		Root:        root,
		Tools:       []kit.Tool{tool},
		Gate:        gate.AllowAll(),
		EventWriter: &buffer,
	})
	if _, _, err := agent.RunText(ctx, "s1", "hi", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	if agent.EventError() != nil {
		t.Fatalf("EventError() = %v, want nil", agent.EventError())
	}

	envelopes := wiringDecodeAll(t, buffer.Bytes())
	if len(envelopes) == 0 {
		t.Fatal("事件流为空")
	}
	if envelopes[0].Seq != 1 {
		t.Fatalf("第一条事件 seq = %d, want 1", envelopes[0].Seq)
	}
	kinds := make(map[string]int, len(envelopes))
	for index, envelope := range envelopes {
		if want := int64(index + 1); envelope.Seq != want {
			t.Fatalf("第 %d 条事件 seq = %d, want %d（必须从 1 连续递增）", index, envelope.Seq, want)
		}
		if envelope.Time.IsZero() {
			t.Fatalf("第 %d 条事件没有时间戳", index)
		}
		kinds[envelope.Kind]++
	}
	for _, want := range []string{events.KindToolCall, events.KindToolResult, events.KindMessageDelta} {
		if kinds[want] == 0 {
			t.Fatalf("事件流缺少 %q，实际: %v", want, kinds)
		}
	}
}

// wiringDecodeAll 用 events.NewDecoder 完整解析一段 NDJSON。
func wiringDecodeAll(t *testing.T, raw []byte) []events.Envelope {
	t.Helper()
	decoder := events.NewDecoder(bytes.NewReader(raw))
	var out []events.Envelope
	for {
		envelope, err := decoder.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("events.Decoder: %v（原始输出: %q）", err, string(raw))
		}
		out = append(out, envelope)
	}
}

// 事件写入失败不能拖垮 Run，但必须能通过 EventError() 读到。
func TestWiringEventWriterFailureKeepsRunAndReportsError(t *testing.T) {
	ctx := wiringContext(t)
	provider := &scriptProvider{scripts: [][]kit.Chunk{textScript("fine")}}
	agent := mustNew(t, kit.Config{
		Model:       "m",
		Provider:    provider,
		Root:        t.TempDir(),
		EventWriter: wiringFailWriter{},
	})
	text, _, err := agent.RunText(ctx, "s1", "hi", nil)
	if err != nil {
		t.Fatalf("RunText = %v, want success even though the event sink fails", err)
	}
	if !strings.Contains(text, "fine") {
		t.Fatalf("text = %q", text)
	}
	if agent.EventError() == nil {
		t.Fatal("EventError() = nil, want the write error")
	}
	if !strings.Contains(agent.EventError().Error(), "exploded") {
		t.Fatalf("EventError() = %v, want it to carry the sink error", agent.EventError())
	}
}

// 没有配置 EventWriter 时 EventError() 必须安全返回 nil（nil receiver 也不 panic）。
func TestWiringEventErrorNilWithoutWriter(t *testing.T) {
	provider := &scriptProvider{scripts: [][]kit.Chunk{textScript("ok")}}
	agent := mustNew(t, kit.Config{Model: "m", Provider: provider, Root: t.TempDir()})
	if err := agent.EventError(); err != nil {
		t.Fatalf("EventError() = %v, want nil", err)
	}
	if _, _, err := agent.RunText(wiringContext(t), "s1", "hi", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	if err := agent.EventError(); err != nil {
		t.Fatalf("EventError() = %v after a run without a writer, want nil", err)
	}
}

// ---------------------------------------------------------------------------
// 6. 事件回调顺序：调用方 req.Emit 不能被事件桥吞掉
// ---------------------------------------------------------------------------

func TestWiringEmitCallbackStillCalledAlongsideWriter(t *testing.T) {
	ctx := wiringContext(t)
	var buffer bytes.Buffer
	tool := &wiringEchoTool{name: "echo"}
	provider := &scriptProvider{scripts: [][]kit.Chunk{
		toolCallScript(kit.ToolCall{ID: "call_1", Name: "echo", Arguments: `{"x":1}`}),
		textScript("done"),
	}}
	agent := mustNew(t, kit.Config{
		Model:       "m",
		Provider:    provider,
		Root:        t.TempDir(),
		Tools:       []kit.Tool{tool},
		Gate:        gate.AllowAll(),
		EventWriter: &buffer,
	})

	var seenMu sync.Mutex
	var seen []kit.Event
	_, err := agent.Run(ctx, kit.RunRequest{
		SessionID: "s1",
		Input:     []kit.Message{kit.NewMessage(kit.RoleUser, "hi")},
		Emit: func(event kit.Event) {
			seenMu.Lock()
			seen = append(seen, event)
			seenMu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	seenMu.Lock()
	captured := append([]kit.Event(nil), seen...)
	seenMu.Unlock()
	if len(captured) == 0 {
		t.Fatal("调用方的 Emit 没有收到任何事件（被事件桥吞掉了）")
	}
	types := eventTypes(captured)
	var sawCall, sawResult, sawText bool
	for _, eventType := range types {
		switch eventType {
		case kit.EventToolCall:
			sawCall = true
		case kit.EventToolResult:
			sawResult = true
		case kit.EventTextDelta:
			sawText = true
		}
	}
	if !sawCall || !sawResult || !sawText {
		t.Fatalf("调用方回调漏了事件: toolCall=%v toolResult=%v text=%v（全部: %v）", sawCall, sawResult, sawText, types)
	}
	// 同一批事件也必须进了事件流。
	envelopes := wiringDecodeAll(t, buffer.Bytes())
	if len(envelopes) == 0 {
		t.Fatal("事件流为空")
	}
	var streamCall, streamResult bool
	for _, envelope := range envelopes {
		switch envelope.Kind {
		case events.KindToolCall:
			streamCall = true
		case events.KindToolResult:
			streamResult = true
		}
	}
	if !streamCall || !streamResult {
		t.Fatalf("事件流缺少 tool.call/tool.result: call=%v result=%v", streamCall, streamResult)
	}
	// 事件流里的工具结果同样必须是脱敏后的内容（脱敏在工具边界，早于事件桥）。
	if !strings.Contains(buffer.String(), "\"name\":\"echo\"") {
		t.Fatalf("事件流里没有工具名:\n%s", buffer.String())
	}
}

// 快照必须发生在**工具执行之前**：工具被调用时 tracker 已经有一个回合。
func TestWiringUndoTrackerBeginsBeforeToolsRun(t *testing.T) {
	ctx := wiringContext(t)
	root := t.TempDir()
	tracker := undo.NewTracker(root, undo.Options{})

	var mu sync.Mutex
	turnsAtCall := -1
	tool := &fakeTool{name: "probe", fn: func(context.Context, kit.ToolCall) (kit.ToolResult, error) {
		mu.Lock()
		turnsAtCall = len(tracker.Turns())
		mu.Unlock()
		return kit.ToolResult{Content: "ok"}, nil
	}}
	provider := &scriptProvider{scripts: [][]kit.Chunk{
		toolCallScript(kit.ToolCall{ID: "call_1", Name: "probe", Arguments: `{}`}),
		textScript("done"),
	}}
	agent := mustNew(t, kit.Config{
		Model:       "m",
		Provider:    provider,
		Root:        root,
		Tools:       []kit.Tool{tool},
		Gate:        gate.AllowAll(),
		UndoTracker: tracker,
	})
	if _, _, err := agent.RunText(ctx, "s1", "hi", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	mu.Lock()
	got := turnsAtCall
	mu.Unlock()
	if got != 1 {
		t.Fatalf("工具执行时 Turns() = %d, want 1（快照必须在工具之前拍）", got)
	}
}

// 【已报告的接线缺陷】Redactor 只作用于工具**输出**：tool.result 是脱敏的，但
// tool.call 事件里的 arguments 是模型原文，因此事件流（= 日志）里会出现明文凭据。
// Config.Redactor 的文档写的是"模型与日志都看不到明文凭据"，这条路径违反了它。
// 修法在 kit 侧：事件桥 emit 前对 payload 文本做 Redact（或复用 redactor 包一层
// Emit）。修好后本用例会 skip，提示把它改成"断言事件流无明文"。
func TestWiringEventStreamLeaksToolArguments(t *testing.T) {
	ctx := wiringContext(t)
	const secret = "sk-abcdefghijklmnopqrstuv"
	var buffer bytes.Buffer
	tool := &wiringEchoTool{name: "echo_secret"}
	provider := &scriptProvider{scripts: [][]kit.Chunk{
		toolCallScript(kit.ToolCall{ID: "call_1", Name: "echo_secret", Arguments: `{"token":"` + secret + `"}`}),
		textScript("done"),
	}}
	agent := mustNew(t, kit.Config{
		Model:       "m",
		Provider:    provider,
		Root:        t.TempDir(),
		Tools:       []kit.Tool{tool},
		Gate:        gate.AllowAll(),
		Redactor:    secrets.New(secret),
		EventWriter: &buffer,
	})
	if _, _, err := agent.RunText(ctx, "s1", "hi", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	raw := buffer.String()
	if !strings.Contains(raw, secret) {
		t.Skip("kit 已修复：事件流已无明文，请把本用例改成断言无明文（并保留 tool.result 脱敏断言）")
	}

	// 现状：脱敏只覆盖输出，工具调用的入参原样进了事件流。
	var callPayload, resultPayload string
	for _, envelope := range wiringDecodeAll(t, buffer.Bytes()) {
		switch envelope.Kind {
		case events.KindToolCall:
			callPayload = string(envelope.Payload)
		case events.KindToolResult:
			resultPayload = string(envelope.Payload)
		}
	}
	if !strings.Contains(callPayload, secret) {
		t.Fatalf("tool.call payload = %q, want the raw arguments (缺陷点)", callPayload)
	}
	if strings.Contains(resultPayload, secret) || !strings.Contains(resultPayload, "[REDACTED]") {
		t.Fatalf("tool.result payload = %q, want the redacted content", resultPayload)
	}
	t.Logf("缺陷确认：tool.call 泄露入参明文，tool.result 已脱敏")
}

// 同一个 writer 上连续两个 Run 共享一个 emitter：seq 必须接着涨，不能重置。
func TestWiringEventSeqContinuesAcrossRuns(t *testing.T) {
	ctx := wiringContext(t)
	var buffer bytes.Buffer
	provider := &scriptProvider{scripts: [][]kit.Chunk{textScript("one"), textScript("two")}}
	agent := mustNew(t, kit.Config{Model: "m", Provider: provider, Root: t.TempDir(), EventWriter: &buffer})
	for _, prompt := range []string{"first", "second"} {
		if _, _, err := agent.RunText(ctx, "s1", prompt, nil); err != nil {
			t.Fatalf("RunText(%q): %v", prompt, err)
		}
	}
	envelopes := wiringDecodeAll(t, buffer.Bytes())
	if len(envelopes) < 2 {
		t.Fatalf("事件太少: %d", len(envelopes))
	}
	for index, envelope := range envelopes {
		if want := int64(index + 1); envelope.Seq != want {
			t.Fatalf("seq 在第 %d 条断裂: got %d, want %d（两个 Run 之间不能重置）", index, envelope.Seq, want)
		}
	}
}

// 【已报告的接线缺陷，固化为对照】cfg.Root 为空（默认 "."）时，AGENTS.md 标题退化成
// 绝对路径：assembleSystemPrompt 把原始 root 传给了 Set.Render，而不是解析后的绝对根。
// 影响：同一工作区因宿主是否显式传 Root 而渲染出不同的 system prompt（prompt 缓存
// 不友好），且进程 cwd 变化会改变标题。修改点应在 kit 侧（传 policy.Roots()[0]）。
func TestWiringDefaultRootRendersAbsoluteHeading(t *testing.T) {
	root := t.TempDir()
	set := wiringInstructions(t, root, "PROJECT RULES\n")
	heading := func(cfgRoot string) string {
		provider := &scriptProvider{scripts: [][]kit.Chunk{textScript("ok")}}
		agent := mustNew(t, kit.Config{Model: "m", Provider: provider, Root: cfgRoot, Instructions: set})
		if _, _, err := agent.RunText(wiringContext(t), "s1", "hi", nil); err != nil {
			t.Fatalf("RunText: %v", err)
		}
		for _, line := range strings.Split(provider.request(0).Messages[0].Text(), "\n") {
			if strings.HasPrefix(line, "## ") {
				return line
			}
		}
		return ""
	}
	explicit := heading(root)
	defaulted := heading("")
	if explicit != "## AGENTS.md (source: root)" {
		t.Fatalf("显式 Root 的标题 = %q, want the relative form", explicit)
	}
	if defaulted == explicit {
		t.Skip("kit 已修复：默认 Root 也能渲染相对标题")
	}
	t.Logf("缺陷确认：cfg.Root 为空时标题 = %q（显式 Root 时为 %q）", defaulted, explicit)
	if !strings.Contains(defaulted, filepath.ToSlash(root)) {
		t.Fatalf("默认 Root 的标题不包含绝对根: %q", defaulted)
	}
}

// SessionStoreDir 打不开时必须 fail loud（而不是静默退回内存历史）。
func TestWiringSessionStoreOpenFailureIsReported(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	_, err := kit.New(kit.Config{
		Model:           "m",
		Provider:        &scriptProvider{scripts: [][]kit.Chunk{textScript("x")}},
		Root:            t.TempDir(),
		SessionStoreDir: filepath.Join(blocker, "sessions"),
	})
	if err == nil {
		t.Fatal("kit.New accepted an unusable SessionStoreDir")
	}
	if !strings.Contains(err.Error(), "session store") {
		t.Fatalf("error = %v, want it to name the session store", err)
	}
}

// 【已报告的接线缺陷】同一 SessionStoreDir 上，先 New 的 agent 看不到"之后才创建"
// 的会话：store.Dir 在 NewDir 时缓存索引，History.Append 在 ErrNotFound 后重试
// 仍然查同一份陈旧索引，于是第二次 New 之后仍失败：
//
//	B run ERROR: rig: append history: session store: session not found: "s1"
//
// 影响：宿主同时持有多个 agent（主 agent + 常驻 worker/subagent）时，先建的那个
// 永远写不进后建的会话。修法建议在 session/store（Create 在 ErrExists 时合并磁盘
// 索引，或 AppendBatch 在 lookup 未命中时重读 index.json），kit 侧至少应在
// SessionStoreDir 的文档里写明"一个目录一个 agent 实例，或每次都新建 agent"。
// 修复后本用例会 skip，提示改成断言"两个 agent 都能写"。
func TestWiringTwoAgentsSameStoreDir(t *testing.T) {
	ctx := wiringContext(t)
	base := t.TempDir()
	root := filepath.Join(base, "ws")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	storeDir := filepath.Join(base, "sessions")

	// 关键顺序：两个 agent 都在任何会话存在之前创建。
	first := mustNew(t, kit.Config{Model: "m", Provider: &scriptProvider{scripts: [][]kit.Chunk{textScript("first ok")}}, Root: root, SessionStoreDir: storeDir})
	second := mustNew(t, kit.Config{Model: "m", Provider: &scriptProvider{scripts: [][]kit.Chunk{textScript("second ok")}}, Root: root, SessionStoreDir: storeDir})

	if _, _, err := first.RunText(ctx, "s1", "first", nil); err != nil {
		t.Fatalf("first agent run: %v", err)
	}
	_, _, secondErr := second.RunText(ctx, "s1", "second", nil)

	// 对照组：会话存在之后再创建的 agent 一定能写（证明问题出在"索引陈旧"，不是权限/路径）。
	third := mustNew(t, kit.Config{Model: "m", Provider: &scriptProvider{scripts: [][]kit.Chunk{textScript("third ok")}}, Root: root, SessionStoreDir: storeDir})
	if _, _, err := third.RunText(ctx, "s1", "third", nil); err != nil {
		t.Fatalf("third agent (created after the session exists) run: %v", err)
	}

	if secondErr == nil {
		t.Skip("kit/store 已修复：先创建的 agent 也能写入新会话，请把本用例改成断言两个 agent 都成功")
	}
	if !strings.Contains(secondErr.Error(), "session not found") {
		t.Fatalf("second agent run = %v, want the stale-index 'session not found' failure", secondErr)
	}
	t.Logf("缺陷确认：先 New 的 agent 无法写入后创建的会话: %v", secondErr)
}
