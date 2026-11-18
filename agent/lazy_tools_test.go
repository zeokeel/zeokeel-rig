package agent_test

// 动态工具加载（lazy tool discovery）的**缓存不变量**。
//
// 设计目标不是"少发点 schema"，而是"发出去的 tools 数组在一段连续对话里逐字节
// 稳定"——provider 的 prefix cache 以请求前缀的字节为准，只要 tools 数组变了，
// 该轮缓存就不命中。因此本文件把三条不变量钉死：
//
//	1. 不激活 → 每一轮 Request.Tools 完全相同；
//	2. 激活 → 只在**末尾追加**，其余项逐字节不变；此后再保持稳定；
//	3. 从不移除 → 空/重复/未知名字都不改变 active 集合与 Request.Tools。
//
// 另外覆盖"未激活的工具不可执行"与"active 集合按 session 隔离"。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/agent"
)

// lazyToolsFixture 构造一个 3 工具注册表的 agent，初始只 active 第一个。
func lazyToolsFixture(t *testing.T, initial []string, scripts [][]rig.Chunk) (rig.Agent, *scriptProvider, map[string]*fakeTool) {
	t.Helper()
	tools := map[string]*fakeTool{
		"read_file":  {name: "read_file", desc: "read a file", schema: map[string]any{"type": "object"}},
		"write_file": {name: "write_file", desc: "write a file", schema: map[string]any{"type": "object"}},
		"grep":       {name: "grep", desc: "search inside files", schema: map[string]any{"type": "object"}},
	}
	ordered := []rig.Tool{tools["read_file"], tools["write_file"], tools["grep"]}
	provider := &scriptProvider{scripts: scripts}
	a := mustNewAgent(t, rig.AgentSpec{
		Model:              "m",
		Provider:           provider,
		Tools:              ordered,
		InitialActiveTools: initial,
	})
	return a, provider, tools
}

// toolsJSON 把一次请求的 Tools 编码成 JSON（比较"逐字节相同"的标准）。
func toolsJSON(t *testing.T, request rig.Request) string {
	t.Helper()
	encoded, err := json.Marshal(request.Tools)
	if err != nil {
		t.Fatalf("marshal tools: %v", err)
	}
	return string(encoded)
}

// toolNames 返回一次请求里工具名（按数组顺序）。
func toolNames(request rig.Request) []string {
	names := make([]string, 0, len(request.Tools))
	for _, spec := range request.Tools {
		names = append(names, spec.Name)
	}
	return names
}

// TestLazyToolsRequestToolsAreStableWithoutActivation：5 轮不激活 → tools 逐字节相同。
func TestLazyToolsRequestToolsAreStableWithoutActivation(t *testing.T) {
	scripts := make([][]rig.Chunk, 0, 5)
	for i := 0; i < 4; i++ {
		scripts = append(scripts, toolCallScript("c"+string(rune('0'+i)), "read_file", `{}`))
	}
	scripts = append(scripts, textScript("done"))
	a, provider, _ := lazyToolsFixture(t, []string{"read_file"}, scripts)

	if _, err := a.Run(context.Background(), rig.RunRequest{Input: []rig.Message{userMessage("go")}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if provider.callCount() != 5 {
		t.Fatalf("provider 调用 %d 次，want 5", provider.callCount())
	}
	first := toolsJSON(t, provider.request(0))
	if got := toolNames(provider.request(0)); len(got) != 1 || got[0] != "read_file" {
		t.Fatalf("首轮 tools = %v, want 只有 read_file", got)
	}
	for index := 1; index < provider.callCount(); index++ {
		if got := toolsJSON(t, provider.request(index)); got != first {
			t.Fatalf("第 %d 轮 tools 与首轮不同（prefix cache 会失效）:\nfirst: %s\ngot:   %s",
				index, first, got)
		}
	}
}

// TestLazyToolsActivationIsAppendOnly：激活后只追加一项，其余逐字节不变，之后再稳定。
func TestLazyToolsActivationIsAppendOnly(t *testing.T) {
	// 每次 Run 消耗两条脚本（一轮工具调用 + 一轮收尾）。
	scripts := [][]rig.Chunk{
		toolCallScript("c1", "read_file", `{}`), textScript("first"),
		toolCallScript("c2", "read_file", `{}`), textScript("second"),
		textScript("third"),
	}
	a, provider, _ := lazyToolsFixture(t, []string{"read_file"}, scripts)

	// 第一轮：只有 read_file。
	if err := runOnce(t, a); err != nil {
		t.Fatalf("第一轮: %v", err)
	}
	firstRequest := provider.callCount() - 1
	before := append([]rig.ToolSpec(nil), provider.request(firstRequest).Tools...)
	if len(before) != 1 || before[0].Name != "read_file" {
		t.Fatalf("首轮 tools = %v", before)
	}

	// host 在两次 Run 之间激活 write_file。
	activator, ok := a.(rig.ToolActivator)
	if !ok {
		t.Fatal("agent 必须实现 rig.ToolActivator")
	}
	activated, unknown := activator.ActivateTools("", "write_file")
	if len(activated) != 1 || activated[0] != "write_file" || len(unknown) != 0 {
		t.Fatalf("ActivateTools = (%v, %v)", activated, unknown)
	}

	if err := runOnce(t, a); err != nil {
		t.Fatalf("第二轮: %v", err)
	}
	secondRequest := provider.callCount() - 1
	after := provider.request(secondRequest).Tools
	if len(after) != len(before)+1 {
		t.Fatalf("激活后 tools 数量 = %d, want %d", len(after), len(before)+1)
	}
	// 前缀逐字节不变。
	prefixJSON, _ := json.Marshal(after[:len(before)])
	beforeJSON, _ := json.Marshal(before)
	if string(prefixJSON) != string(beforeJSON) {
		t.Fatalf("激活改变了已有工具的顺序/内容（prefix cache 失效）:\nbefore: %s\nafter:  %s",
			beforeJSON, prefixJSON)
	}
	if after[len(after)-1].Name != "write_file" {
		t.Fatalf("新增项 = %q, want write_file 在末尾", after[len(after)-1].Name)
	}

	// 再跑一轮：tools 再次稳定（与上一轮逐字节相同）。
	if err := runOnce(t, a); err != nil {
		t.Fatalf("第三轮: %v", err)
	}
	if toolsJSON(t, provider.request(provider.callCount()-1)) != toolsJSON(t, provider.request(secondRequest)) {
		t.Fatal("激活之后的轮次 tools 又变了")
	}
}

// TestLazyToolsNeverRemoves：空/重复/未知名字都不改变 active 集合与 Request.Tools。
func TestLazyToolsNeverRemoves(t *testing.T) {
	scripts := [][]rig.Chunk{toolCallScript("c1", "read_file", `{}`), textScript("done"), textScript("done")}
	a, provider, tools := lazyToolsFixture(t, nil, scripts)
	activator := a.(rig.ToolActivator)

	activator.ActivateTools("", "write_file")
	before := activator.ActiveTools("")
	if err := runOnce(t, a); err != nil {
		t.Fatalf("第一轮: %v", err)
	}
	beforeTools := toolsJSON(t, provider.request(0))

	// 空、重复、未知：都不能改变集合。
	if activated, unknown := activator.ActivateTools(""); len(activated) != 0 || len(unknown) != 0 {
		t.Fatalf("空调用 = (%v, %v), want 都为空", activated, unknown)
	}
	if activated, unknown := activator.ActivateTools("", "write_file"); len(activated) != 1 || len(unknown) != 0 {
		t.Fatalf("重复激活 = (%v, %v)：已 active 的名字应出现在 activated（幂等）", activated, unknown)
	}
	activated, unknown := activator.ActivateTools("", "nope")
	if len(activated) != 0 || len(unknown) != 1 || unknown[0] != "nope" {
		t.Fatalf("未知名字 = (%v, %v)", activated, unknown)
	}
	after := activator.ActiveTools("")
	if strings.Join(before, ",") != strings.Join(after, ",") {
		t.Fatalf("active 集合被改变了: before=%v after=%v", before, after)
	}

	if err := runOnce(t, a); err != nil {
		t.Fatalf("第二轮: %v", err)
	}
	if got := toolsJSON(t, provider.request(1)); got != beforeTools {
		t.Fatalf("空/重复/未知激活改变了 Request.Tools:\nbefore: %s\nafter:  %s", beforeTools, got)
	}
	// 三个工具里只有两个 active；grep 从未被激活，也没被执行。
	if tools["grep"].callCount() != 0 {
		t.Fatalf("grep 被调用了 %d 次", tools["grep"].callCount())
	}
}

// TestLazyToolsInactiveToolCannotExecute：未激活的工具产生可读错误、不执行、不过 gate。
func TestLazyToolsInactiveToolCannotExecute(t *testing.T) {
	scripts := [][]rig.Chunk{
		toolCallScript("c1", "write_file", `{"path":"a"}`),
		textScript("ok"),
	}
	var gateCalls int
	gate := gateFunc(func(context.Context, rig.Tool, rig.ToolCall) (rig.Decision, error) {
		gateCalls++
		return rig.DecisionAllow, nil
	})
	tools := []rig.Tool{
		&fakeTool{name: "read_file"},
		&fakeTool{name: "write_file"},
	}
	provider := &scriptProvider{scripts: scripts}
	a := mustNewAgent(t, rig.AgentSpec{
		Model: "m", Provider: provider, Tools: tools, Gate: gate,
		InitialActiveTools: []string{"read_file"},
	})
	result, err := a.Run(context.Background(), rig.RunRequest{Input: []rig.Message{userMessage("write it")}})
	requireNoError(t, err)

	var blocked string
	for _, message := range result.Messages {
		if message.Role == rig.RoleTool && strings.Contains(message.Text(), "not active") {
			blocked = message.Text()
		}
	}
	if blocked == "" {
		t.Fatalf("没有产生「未激活」的工具错误: %+v", result.Messages)
	}
	if !strings.Contains(blocked, `tool "write_file"`) || !strings.Contains(blocked, "tool_search") {
		t.Fatalf("错误文案 = %q, want 含工具名与激活指引", blocked)
	}
	if gateCalls != 0 {
		t.Fatalf("未激活的调用过了 gate %d 次（应该在 gate 之前就拒绝）", gateCalls)
	}
	if got := tools[1].(*fakeTool).callCount(); got != 0 {
		t.Fatalf("未激活的工具被执行了 %d 次", got)
	}
}

// TestLazyToolsActiveSetIsPerSession：active 集合按 session 隔离。
func TestLazyToolsActiveSetIsPerSession(t *testing.T) {
	tools := []rig.Tool{&fakeTool{name: "read_file"}, &fakeTool{name: "write_file"}, &fakeTool{name: "grep"}}
	provider := &scriptProvider{scripts: [][]rig.Chunk{textScript("a"), textScript("b")}}
	a := mustNewAgent(t, rig.AgentSpec{
		Model: "m", Provider: provider, Tools: tools,
		InitialActiveTools: []string{"read_file"},
	})
	activator := a.(rig.ToolActivator)
	activator.ActivateTools("s1", "write_file")

	if got := activator.ActiveTools("s1"); strings.Join(got, ",") != "read_file,write_file" {
		t.Fatalf("s1 active = %v", got)
	}
	if got := activator.ActiveTools("s2"); strings.Join(got, ",") != "read_file" {
		t.Fatalf("s2 active = %v（不该继承 s1 的激活）", got)
	}
	if got := activator.ActiveTools(""); strings.Join(got, ",") != "read_file" {
		t.Fatalf("default session active = %v", got)
	}

	// 请求侧也要隔离。
	if _, err := a.Run(context.Background(), rig.RunRequest{SessionID: "s1", Input: []rig.Message{userMessage("x")}}); err != nil {
		t.Fatalf("Run(s1): %v", err)
	}
	if _, err := a.Run(context.Background(), rig.RunRequest{SessionID: "s2", Input: []rig.Message{userMessage("y")}}); err != nil {
		t.Fatalf("Run(s2): %v", err)
	}
	if got := toolNames(provider.request(0)); strings.Join(got, ",") != "read_file,write_file" {
		t.Fatalf("s1 请求 tools = %v", got)
	}
	if got := toolNames(provider.request(1)); strings.Join(got, ",") != "read_file" {
		t.Fatalf("s2 请求 tools = %v", got)
	}
}

// TestLazyToolsNilInitialMeansAllActive：v0.20 行为（nil = 全部 active）。
func TestLazyToolsNilInitialMeansAllActive(t *testing.T) {
	tools := []rig.Tool{&fakeTool{name: "a"}, &fakeTool{name: "b"}}
	provider := &scriptProvider{scripts: [][]rig.Chunk{textScript("ok")}}
	a := mustNewAgent(t, rig.AgentSpec{Model: "m", Provider: provider, Tools: tools})
	activator := a.(rig.ToolActivator)
	if got := activator.ActiveTools(""); strings.Join(got, ",") != "a,b" {
		t.Fatalf("active = %v, want 全部", got)
	}
	if _, err := a.Run(context.Background(), rig.RunRequest{Input: []rig.Message{userMessage("x")}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := toolNames(provider.request(0)); strings.Join(got, ",") != "a,b" {
		t.Fatalf("请求 tools = %v", got)
	}
}

// TestLazyToolsActivationFollowsRegistrationOrder：同一批激活按注册顺序追加（与参数
// 顺序无关），跨批则严格追加——"只在末尾追加"是不变量的第一优先。
func TestLazyToolsActivationFollowsRegistrationOrder(t *testing.T) {
	tools := []rig.Tool{
		&fakeTool{name: "a"}, &fakeTool{name: "b"}, &fakeTool{name: "c"}, &fakeTool{name: "d"},
	}
	provider := &scriptProvider{scripts: [][]rig.Chunk{textScript("ok")}}
	a := mustNewAgent(t, rig.AgentSpec{
		Model: "m", Provider: provider, Tools: tools, InitialActiveTools: []string{"a"},
	})
	activator := a.(rig.ToolActivator)
	// 故意用乱序激活。
	activator.ActivateTools("", "d", "b")
	if got := activator.ActiveTools(""); strings.Join(got, ",") != "a,b,d" {
		t.Fatalf("active = %v, want a,b,d（注册顺序）", got)
	}
	// 再激活 c：它**追加在末尾**（而不是插回注册位置），因为插入会让已发出的前缀
	// 失效——"只在末尾追加"优先于"全局按注册顺序"。
	activator.ActivateTools("", "c")
	if got := activator.ActiveTools(""); strings.Join(got, ",") != "a,b,d,c" {
		t.Fatalf("active = %v, want a,b,d,c（追加语义）", got)
	}
}

// --- 辅助 ---

// runOnce 跑一轮纯文本运行（断言里只关心请求的 tools）。
func runOnce(t *testing.T, a rig.Agent) error {
	t.Helper()
	_, err := a.Run(context.Background(), rig.RunRequest{Input: []rig.Message{userMessage("go")}})
	return err
}

// gateFunc 适配 rig.Gate。
type gateFunc func(context.Context, rig.Tool, rig.ToolCall) (rig.Decision, error)

func (f gateFunc) Approve(ctx context.Context, tool rig.Tool, call rig.ToolCall) (rig.Decision, error) {
	return f(ctx, tool, call)
}

// TestLazyToolsSessionReachesToolContext：工具能从 ctx 里拿到会话 ID（tool_search
// 靠它把激活落到正确的会话上）。
func TestLazyToolsSessionReachesToolContext(t *testing.T) {
	var got string
	tools := []rig.Tool{&fakeTool{name: "probe", fn: func(ctx context.Context, _ rig.ToolCall) (rig.ToolResult, error) {
		got = agent.ToolSessionID(ctx)
		return rig.ToolResult{Content: "ok"}, nil
	}}}
	provider := &scriptProvider{scripts: [][]rig.Chunk{
		toolCallScript("c1", "probe", `{}`),
		textScript("done"),
	}}
	a := mustNewAgent(t, rig.AgentSpec{Model: "m", Provider: provider, Tools: tools})
	if _, err := a.Run(context.Background(), rig.RunRequest{
		SessionID: "s-42", Input: []rig.Message{userMessage("go")},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got != "s-42" {
		t.Fatalf("ToolSessionID(ctx) = %q, want s-42", got)
	}
}
