package kit_test

// kit 接线的集成测试：SessionStoreDir / Instructions+Environment / 工具结果不被改写 /
// UndoTracker / EventWriter，以及"事件回调不吞调用方回调"的顺序契约。
//
// 每个用例都同时包含一次**反向对照**（或独立的 negative-control 用例），确保它
// 真的能失败：持久化用例配"内存历史不持久"；工具结果用例配"模型拿到的就是原文"；
// 事件用例配"写失败仍成功 + EventError 非 nil"。

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/events"
	"github.com/zeokeel/zeokeel-rig/gate"
	"github.com/zeokeel/zeokeel-rig/instructions"
	"github.com/zeokeel/zeokeel-rig/kit"
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

// wiringEchoTool 把收到的参数原样回显；用于验证"工具结果与入参都原样传递"。
type wiringEchoTool struct {
	name string
}

func (t *wiringEchoTool) Name() string           { return t.name }
func (t *wiringEchoTool) Description() string    { return "echo the arguments back" }
func (t *wiringEchoTool) Schema() map[string]any { return map[string]any{"type": "object"} }

func (t *wiringEchoTool) Invoke(_ context.Context, call kit.ToolCall) (kit.ToolResult, error) {
	return kit.ToolResult{Content: "echo: " + call.Arguments}, nil
}

// wiringFailWriter 每次写都失败，用来验证事件流故障不影响主循环。
type wiringFailWriter struct{}

func (wiringFailWriter) Write([]byte) (int, error) { return 0, errors.New("event sink exploded") }

// gitRepoRoot 建一个 git 仓库并返回根目录。
//
// undo 的回合快照基于 git（隐藏提交），因此需要快照的用例必须跑在仓库里；
// git 不可用时跳过。
func gitRepoRoot(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git 不可用: %v", err)
	}
	root := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", root}, args...)...)
		command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	run("init", "-q", ".")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	run("config", "commit.gpgsign", "false")
	return root
}

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

// 存储配置的互斥：三项里**任意两项**同时设置都必须报错，而不是悄悄忽略一个。
//
// 2026-11 钉子复核（名字级契约的钉子用例）：原来只测
// `History + SessionStoreDir` 这一对，并且只断言文案里有 "mutually exclusive"。
// 问题是 kit 里由**两段 if 分支**（SessionStore 分支、SessionStoreDir 分支）管三对
// 组合，只测一对等于只守住其中一条分支：另外两对哪天被写成"后者覆盖前者"，
// 用户会得到一个能跑但把数据写进错误后端的 agent——"静默忽略配置"比报错难查得多。
//
// 因此改成三条断言：
//  1. 三对组合**逐一**必须报错（将来把三个字段收成 `Storage` 时，这里是唯一
//     需要补一行的地方，而"单项可用"那条会守住新字段没有把合法配置也拒掉）；
//  2. 文案必须点名用户设置的那两个字段（否则用户不知道该删哪个）；
//  3. 反面对照：任意**单个**字段都必须被接受。
func TestWiringConflictingStoreConfigsAreRejected(t *testing.T) {
	provider := func() kit.Provider {
		return &scriptProvider{scripts: [][]kit.Chunk{textScript("x")}}
	}
	newStore := func(t *testing.T) *store.Dir {
		t.Helper()
		st, err := store.NewDir(t.TempDir())
		if err != nil {
			t.Fatalf("store.NewDir: %v", err)
		}
		return st
	}
	sessionDir := func(t *testing.T) string {
		t.Helper()
		return filepath.Join(t.TempDir(), "sessions")
	}
	makeConfig := func(t *testing.T, build func(*kit.Config)) kit.Config {
		t.Helper()
		cfg := kit.Config{Model: "m", Provider: provider(), Root: t.TempDir()}
		build(&cfg)
		return cfg
	}

	conflicts := []struct {
		name    string
		build   func(*kit.Config)
		mention []string
	}{
		{"History + SessionStoreDir", func(cfg *kit.Config) {
			cfg.History = session.NewMemory()
			cfg.SessionStoreDir = filepath.Join(t.TempDir(), "sessions")
		}, []string{"History", "SessionStoreDir"}},
		{"History + SessionStore", func(cfg *kit.Config) {
			cfg.History = session.NewMemory()
			cfg.SessionStore = newStore(t)
		}, []string{"History", "SessionStore"}},
		{"SessionStore + SessionStoreDir", func(cfg *kit.Config) {
			cfg.SessionStore = newStore(t)
			cfg.SessionStoreDir = filepath.Join(t.TempDir(), "sessions")
		}, []string{"SessionStore", "SessionStoreDir"}},
	}
	for _, c := range conflicts {
		t.Run("冲突必须报错/"+c.name, func(t *testing.T) {
			_, err := kit.New(makeConfig(t, c.build))
			if err == nil {
				t.Fatalf("kit.New 接受了互相冲突的存储配置（%s）：其中一项会被静默忽略，宿主无从发现", c.name)
			}
			if !strings.Contains(err.Error(), "mutually exclusive") {
				t.Fatalf("error = %v, want a mutual-exclusion error", err)
			}
			for _, field := range c.mention {
				if !strings.Contains(err.Error(), field) {
					t.Fatalf("error = %v, want it to name the field %q（用户要知道删哪个）", err, field)
				}
			}
		})
	}

	singles := []struct {
		name  string
		build func(*kit.Config)
	}{
		{"只有 History", func(cfg *kit.Config) { cfg.History = session.NewMemory() }},
		{"只有 SessionStoreDir", func(cfg *kit.Config) { cfg.SessionStoreDir = sessionDir(t) }},
		{"只有 SessionStore", func(cfg *kit.Config) { cfg.SessionStore = newStore(t) }},
	}
	for _, c := range singles {
		t.Run("单个配置必须可用/"+c.name, func(t *testing.T) {
			if _, err := kit.New(makeConfig(t, c.build)); err != nil {
				t.Fatalf("kit.New 把合法配置 %s 也拒了（互斥检查写宽了）: %v", c.name, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 2. 工具结果不被改写（SDK 不做输出脱敏）
// ---------------------------------------------------------------------------

// TestWiringToolResultIsNotRedacted 钉住"工具结果原样进模型上下文"：SDK 不再做
// 输出脱敏，工具返回什么，模型就看到什么（工具调用入参同样原样）。
func TestWiringToolResultIsNotRedacted(t *testing.T) {
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
		t.Fatalf("工具结果被改写了：%q（SDK 不做输出脱敏）", message.Text())
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
	root := gitRepoRoot(t)
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
	// 事件流里的工具结果同样原样写出（SDK 不做脱敏，事件桥看到的就是工具返回值）。
	if !strings.Contains(buffer.String(), "\"name\":\"echo\"") {
		t.Fatalf("事件流里没有工具名:\n%s", buffer.String())
	}
}

// 快照必须发生在**工具执行之前**：工具被调用时 tracker 已经有一个回合。
func TestWiringUndoTrackerBeginsBeforeToolsRun(t *testing.T) {
	ctx := wiringContext(t)
	root := gitRepoRoot(t)
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

// TestWiringEventStreamKeepsRawToolArguments 钉住"事件流不做脱敏"：tool.call 的
// arguments 与 tool.result 的内容都原样写出（SDK 不做输出脱敏；要脱敏的宿主自己在
// 自己的日志/UI 通道处理）。
func TestWiringEventStreamKeepsRawToolArguments(t *testing.T) {
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
		EventWriter: &buffer,
	})
	if _, _, err := agent.RunText(ctx, "s1", "hi", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}

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
		t.Fatalf("tool.call payload = %q, want the raw arguments", callPayload)
	}
	if !strings.Contains(resultPayload, secret) {
		t.Fatalf("tool.result payload = %q, want the raw content（SDK 不做输出脱敏）", resultPayload)
	}
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
//
// **2026-11 处置：降级为能力位测试（不再把缺陷当契约断言）**，见名字级契约的钉子用例。
// 原实现把 `session not found` 写成 t.Fatalf 的期望值：等于**要求这个 bug 继续存在**，
// 而"同一目录多实例"正是 Storage 聚合与会话租约要消灭的用法。
// 现在它只做三件事：
//   - 记录缺陷是否还在（Logf，不判定）；
//   - 会话存在之后创建的 agent 必须能写（这条是**真契约**，守住"没有退化成更糟"）；
//   - 出现未归类的失败形态时照旧失败（避免"修一半"被静默放过）。
//
// 谁来守正确的那条路：修好之后这里直接通过（不再需要 skip 提示），真正的护栏是
// agent 侧的会话租约（条目见 §10「待建立」）。
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

	switch {
	case secondErr == nil:
		t.Logf("目标形态已达成：先创建的 agent 也能写入后建的会话（可以把本条改成断言两个 agent 都成功）")
	case strings.Contains(secondErr.Error(), "session not found"):
		// 已知缺陷：记录，不作为判定依据（缺陷不是契约）。
		t.Logf("已知缺陷仍在（不判定）：先 New 的 agent 无法写入后创建的会话: %v", secondErr)
	default:
		t.Fatalf("出现了未归类的失败形态（既不是已知的陈旧索引缺陷，也不是修好）: %v", secondErr)
	}
}

// GitUndo 打开时：undo 工具出现在工具集里，且每 Run 自动拍快照（可回退到更早回合）。
func TestWiringGitUndoEnablesToolAndSnapshots(t *testing.T) {
	ctx := wiringContext(t)
	root := gitRepoRoot(t)
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	provider := &scriptProvider{scripts: [][]kit.Chunk{
		textScript("first turn"),
		toolCallScript(kit.ToolCall{ID: "c1", Name: "undo", Arguments: `{"turn":1}`}),
		textScript("done"),
	}}
	agent := mustNew(t, kit.Config{
		Model:    "m",
		Provider: provider,
		Root:     root,
		Gate:     gate.AllowAll(),
		GitUndo:  true,
	})
	if _, _, err := agent.RunText(ctx, "s1", "hi", nil); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	// 回合之间用户/模型改了文件，第二个回合开始时快照记录的是 v2。
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, err := agent.RunText(ctx, "s2", "undo please", nil); err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	if !containsName(toolNamesInRequest(provider.request(1)), "undo") {
		t.Fatalf("GitUndo=true 时工具集里应当有 undo: %v", toolNamesInRequest(provider.request(1)))
	}
	message, ok := findToolMessage(provider.request(2).Messages, "undo")
	if !ok {
		t.Fatalf("没有 undo 的工具结果: %+v", provider.request(2).Messages)
	}
	if !strings.Contains(message.Text(), "reverted 1 change(s) from turn 1") {
		t.Fatalf("undo 结果 = %q", message.Text())
	}
	raw, err := os.ReadFile(filepath.Join(root, "a.txt"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(raw) != "v1\n" {
		t.Fatalf("a.txt = %q, want v1（回退到第 1 回合）", raw)
	}
}

// GitUndo 默认关闭；非 git 工作区即使打开也不提供 undo（安静降级）。
func TestWiringGitUndoDefaultsAndNonRepo(t *testing.T) {
	// 文档承诺"默认 false"，这条断言防止有人改默认值却没改文档（或反过来）。
	source, err := os.ReadFile("kit.go")
	if err != nil {
		t.Fatalf("read kit.go: %v", err)
	}
	text := string(source)
	index := strings.Index(text, "GitUndo bool")
	if index < 0 {
		t.Fatal("kit.Config 里找不到 GitUndo 字段")
	}
	comments := text[:index]
	if !strings.Contains(comments, "默认 **false**") {
		t.Fatal("Config.GitUndo 的文档应当写明默认 false")
	}

	// 默认配置（不开 GitUndo）：工具集里没有 undo。
	defaultProvider := &scriptProvider{scripts: [][]kit.Chunk{textScript("ok")}}
	defaultAgent := mustNew(t, kit.Config{
		Model: "m", Provider: defaultProvider, Root: t.TempDir(), Gate: gate.AllowAll(),
	})
	if _, _, err := defaultAgent.RunText(context.Background(), "s1", "hi", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	if names := toolNamesInRequest(defaultProvider.request(0)); containsName(names, "undo") {
		t.Fatalf("默认配置不该有 undo 工具: %v", names)
	}

	// 非 git 工作区 + GitUndo=true：安静地不提供 undo，Run 也不失败。
	plainProvider := &scriptProvider{scripts: [][]kit.Chunk{textScript("ok")}}
	plainAgent := mustNew(t, kit.Config{
		Model: "m", Provider: plainProvider, Root: t.TempDir(), Gate: gate.AllowAll(), GitUndo: true,
	})
	if _, _, err := plainAgent.RunText(context.Background(), "s1", "hi", nil); err != nil {
		t.Fatalf("非 git 工作区的 Run 不该失败: %v", err)
	}
	if names := toolNamesInRequest(plainProvider.request(0)); containsName(names, "undo") {
		t.Fatalf("非 git 工作区不该有 undo 工具: %v", names)
	}
}

// toolNamesInRequest 返回一次 provider 请求里声明的工具名。
func toolNamesInRequest(req kit.Request) []string {
	names := make([]string, 0, len(req.Tools))
	for _, tool := range req.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// containsName 报告名字是否在列表里。
func containsName(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}

// TestWiringUndoTrackerAccessorForUIButton 证明宿主能拿到 tracker 来驱动"撤销按钮"。
func TestWiringUndoTrackerAccessorForUIButton(t *testing.T) {
	ctx := wiringContext(t)
	root := gitRepoRoot(t)
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	agent := mustNew(t, kit.Config{
		Model:    "m",
		Provider: &scriptProvider{scripts: [][]kit.Chunk{textScript("done")}},
		Root:     root,
		Gate:     gate.AllowAll(),
		GitUndo:  true,
	})
	tracker := agent.UndoTracker()
	if tracker == nil {
		t.Fatal("GitUndo=true 时 UndoTracker() 不该是 nil")
	}
	if _, _, err := agent.RunText(ctx, "s1", "hi", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	if got := tracker.Turns(); len(got) != 1 {
		t.Fatalf("Turns() = %v, want [1]", got)
	}
	// 撤销按钮：先预览、再执行。
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	preview, err := tracker.Changes(0, "")
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(preview) != 1 || preview[0].Path != "a.txt" {
		t.Fatalf("preview = %+v, want one change on a.txt", preview)
	}
	applied, err := tracker.Revert(0)
	if err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if len(applied) != 1 || !applied[0].Reverted {
		t.Fatalf("applied = %+v, want one reverted change", applied)
	}
	raw, err := os.ReadFile(filepath.Join(root, "a.txt"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(raw) != "v1\n" {
		t.Fatalf("a.txt = %q, want v1", raw)
	}
	// 没启用撤销时返回 nil，而不是 panic。
	plain := mustNew(t, kit.Config{
		Model:    "m",
		Provider: &scriptProvider{scripts: [][]kit.Chunk{textScript("ok")}},
		Root:     t.TempDir(),
		Gate:     gate.AllowAll(),
	})
	if plain.UndoTracker() != nil {
		t.Fatal("未启用撤销时 UndoTracker() 应当为 nil")
	}
}
