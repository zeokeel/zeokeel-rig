package kit_test

// 动态工具加载在 kit 层的接线：默认开启、核心集 + tool_search + memory、纯增量
// 激活、事件可见、关闭后回到 v0.20 行为。
//
// 缓存不变量（tools 数组逐字节稳定）在 agent 层已由
// agent/lazy_tools_test.go 钉住；这里验证 kit **真的把初始集合接对了**，并且
// 激活走的是同一条路径。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/events"
	"github.com/zeokeel/zeokeel-rig/kit"
)

// lazyConfiguredTools 是一组名字稳定的工具，覆盖"核心"与"可选"两类。
func lazyConfiguredTools() []kit.Tool {
	makeTool := func(name string) kit.Tool {
		return &wiringEchoTool{name: name}
	}
	return []kit.Tool{
		makeTool("read_file"),
		makeTool("write_file"),
		makeTool("grep"),
		makeTool("bash"),
		makeTool("memory"),
		makeTool("web_fetch"),
		makeTool("job_start"),
	}
}

// TestLazyToolsKitDefaultInitialActiveSet：零配置（显式 Tools）下初始 active =
// 核心 + tool_search + memory，其余注册但不 active。
func TestLazyToolsKitDefaultInitialActiveSet(t *testing.T) {
	agent := mustNewKitAgent(t, kit.Config{
		Model:         "m",
		Provider:      &scriptProvider{scripts: [][]kit.Chunk{textScript("ok")}},
		Root:          t.TempDir(),
		Tools:         lazyConfiguredTools(),
		AllowNetwork:  true,
		ToolDiscovery: kit.Bool(true),
	})
	active := agent.Tools()
	for _, want := range []string{"read_file", "write_file", "grep", "bash", "memory", "tool_search"} {
		if !contains(active, want) {
			t.Fatalf("初始 active 缺少 %q: %v", want, active)
		}
	}
	for _, notActive := range []string{"web_fetch", "job_start"} {
		if contains(active, notActive) {
			t.Fatalf("%q 不该初始 active: %v", notActive, active)
		}
	}
	// 注册表仍然包含全部（账本不变）。
	registered := agent.RegisteredTools()
	for _, want := range []string{"read_file", "web_fetch", "job_start", "tool_search", "memory"} {
		if !contains(registered, want) {
			t.Fatalf("RegisteredTools 缺少 %q: %v", want, registered)
		}
	}
}

// TestLazyToolsKitDiscoveryOffKeepsEverythingActive：关闭 discovery → 全部 active。
func TestLazyToolsKitDiscoveryOffKeepsEverythingActive(t *testing.T) {
	agent := mustNewKitAgent(t, kit.Config{
		Model:         "m",
		Provider:      &scriptProvider{scripts: [][]kit.Chunk{textScript("ok")}},
		Root:          t.TempDir(),
		Tools:         lazyConfiguredTools(),
		ToolDiscovery: kit.Bool(false),
		AllowNetwork:  true,
	})
	active := agent.Tools()
	for _, name := range []string{"read_file", "web_fetch", "job_start", "memory"} {
		if !contains(active, name) {
			t.Fatalf("关闭 discovery 后 %q 应当 active: %v", name, active)
		}
	}
	if contains(active, "tool_search") {
		t.Fatalf("关闭 discovery 后不该注册 tool_search: %v", active)
	}
}

// TestLazyToolsKitInitialActiveOverride：宿主覆盖初始集合，但 tool_search 与
// memory 强制并入。
func TestLazyToolsKitInitialActiveOverride(t *testing.T) {
	agent := mustNewKitAgent(t, kit.Config{
		Model:              "m",
		Provider:           &scriptProvider{scripts: [][]kit.Chunk{textScript("ok")}},
		Root:               t.TempDir(),
		Tools:              lazyConfiguredTools(),
		InitialActiveTools: []string{"web_fetch"},
		AllowNetwork:       true,
		ToolDiscovery:      kit.Bool(true),
	})
	active := agent.Tools()
	for _, want := range []string{"web_fetch", "tool_search", "memory"} {
		if !contains(active, want) {
			t.Fatalf("覆盖后的 active 缺少 %q: %v", want, active)
		}
	}
	if contains(active, "read_file") {
		t.Fatalf("覆盖后 read_file 不该 active: %v", active)
	}
}

// TestLazyToolsKitActivationIsAppendOnly：kit 层激活只追加，并且激活的工具出现在
// 下一次请求里。
func TestLazyToolsKitActivationIsAppendOnly(t *testing.T) {
	provider := &scriptProvider{scripts: [][]kit.Chunk{
		textScript("first"),
		textScript("second"),
	}}
	agent := mustNewKitAgent(t, kit.Config{
		Model: "m", Provider: provider, Root: t.TempDir(), Tools: lazyConfiguredTools(), AllowNetwork: true,
		ToolDiscovery: kit.Bool(true),
	})
	if _, err := agent.Run(context.Background(), kit.RunRequest{
		SessionID: "s", Input: []kit.Message{kit.NewMessage(kit.RoleUser, "hi")},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	before := requestToolsJSON(t, provider, 0)
	beforeCount := len(provider.request(0).Tools)

	activated, unknown := agent.ActivateTools("s", "web_fetch")
	if len(activated) != 1 || activated[0] != "web_fetch" || len(unknown) != 0 {
		t.Fatalf("ActivateTools = (%v, %v)", activated, unknown)
	}
	if _, err := agent.Run(context.Background(), kit.RunRequest{
		SessionID: "s", Input: []kit.Message{kit.NewMessage(kit.RoleUser, "again")},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	after := provider.request(1).Tools
	if len(after) != beforeCount+1 {
		t.Fatalf("激活后 tools 数量 = %d, want %d", len(after), beforeCount+1)
	}
	prefixJSON, _ := json.Marshal(after[:beforeCount])
	if string(prefixJSON) != before {
		t.Fatalf("激活改变了已有工具（prefix cache 失效）:\nbefore: %s\nafter:  %s", before, prefixJSON)
	}
	if after[len(after)-1].Name != "web_fetch" {
		t.Fatalf("新增项 = %q", after[len(after)-1].Name)
	}
}

// TestLazyToolsKitSearchActivatesFromModel：模型调用 tool_search 能真的激活工具，
// 并产生 tool.activated 事件（trigger=model）。
func TestLazyToolsKitSearchActivatesFromModel(t *testing.T) {
	provider := &scriptProvider{scripts: [][]kit.Chunk{
		toolCallScript(kit.ToolCall{ID: "c1", Name: "tool_search", Arguments: `{"activate":["web_fetch"]}`}),
		textScript("done"),
	}}
	var buffer bytes.Buffer
	agent := mustNewKitAgent(t, kit.Config{
		Model: "m", Provider: provider, Root: t.TempDir(),
		Tools:         lazyConfiguredTools(),
		Approval:      kit.ApprovalAuto,
		EventWriter:   &buffer,
		AllowNetwork:  true,
		ToolDiscovery: kit.Bool(true),
	})
	if _, err := agent.Run(context.Background(), kit.RunRequest{
		SessionID: "s", Input: []kit.Message{kit.NewMessage(kit.RoleUser, "fetch it")},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 第二轮请求里必须已经带上 web_fetch。
	if got := requestToolNames(provider.request(1)); !contains(got, "web_fetch") {
		t.Fatalf("激活后第二轮请求缺少 web_fetch: %v", got)
	}
	if got := agent.ToolsFor("s"); !contains(got, "web_fetch") {
		t.Fatalf("会话 active 集合缺少 web_fetch: %v", got)
	}
	// 事件：tool.activated，trigger=model。
	decoder := events.NewDecoder(bytes.NewReader(buffer.Bytes()))
	var activatedEvents []events.ToolActivated
	for {
		envelope, err := decoder.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if envelope.Kind != events.KindToolActivated {
			continue
		}
		var payload events.ToolActivated
		if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
			t.Fatalf("decode tool.activated: %v", err)
		}
		activatedEvents = append(activatedEvents, payload)
	}
	if len(activatedEvents) != 1 {
		t.Fatalf("tool.activated 事件数 = %d", len(activatedEvents))
	}
	if activatedEvents[0].Trigger != "model" || !contains(activatedEvents[0].Tools, "web_fetch") {
		t.Fatalf("事件 = %+v", activatedEvents[0])
	}
}

// TestLazyToolsKitHostActivationEmitsHostTrigger：宿主直调 → trigger=host。
func TestLazyToolsKitHostActivationEmitsHostTrigger(t *testing.T) {
	var buffer bytes.Buffer
	agent := mustNewKitAgent(t, kit.Config{
		Model: "m", Provider: &scriptProvider{scripts: [][]kit.Chunk{textScript("ok")}},
		Root: t.TempDir(), Tools: lazyConfiguredTools(), EventWriter: &buffer, AllowNetwork: true,
		ToolDiscovery: kit.Bool(true),
	})
	agent.ActivateTools("", "job_start")
	if !strings.Contains(buffer.String(), string(events.KindToolActivated)) {
		t.Fatalf("没有 tool.activated 事件:\n%s", buffer.String())
	}
	if !strings.Contains(buffer.String(), `"trigger":"host"`) {
		t.Fatalf("trigger 不是 host:\n%s", buffer.String())
	}
}

// TestLazyToolsKitSearchListsOnlyInactive：tool_search 的清单只含 inactive 工具，
// 且永不列出 memory / tool_search 自身。
func TestLazyToolsKitSearchListsOnlyInactive(t *testing.T) {
	provider := &scriptProvider{scripts: [][]kit.Chunk{
		toolCallScript(kit.ToolCall{ID: "c1", Name: "tool_search", Arguments: `{}`}),
		textScript("done"),
	}}
	agent := mustNewKitAgent(t, kit.Config{
		Model: "m", Provider: provider, Root: t.TempDir(),
		Tools: lazyConfiguredTools(), Approval: kit.ApprovalAuto, AllowNetwork: true,
		ToolDiscovery: kit.Bool(true),
	})
	result, err := agent.Run(context.Background(), kit.RunRequest{
		SessionID: "s", Input: []kit.Message{kit.NewMessage(kit.RoleUser, "what can I use?")},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var listing string
	for _, message := range result.Messages {
		if message.Role == kit.RoleTool {
			listing = message.Text()
		}
	}
	if !strings.Contains(listing, "web_fetch") || !strings.Contains(listing, "job_start") {
		t.Fatalf("清单缺少可发现的工具:\n%s", listing)
	}
	for _, never := range []string{"memory", "tool_search"} {
		if strings.Contains(listing, never) {
			t.Fatalf("清单不该列出 %q（它永远 active）:\n%s", never, listing)
		}
	}
}

// --- 辅助 ---

func mustNewKitAgent(t *testing.T, cfg kit.Config) *kit.Agent {
	t.Helper()
	agent, err := kit.New(cfg)
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	return agent
}

func contains(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}

func requestToolsJSON(t *testing.T, provider *scriptProvider, index int) string {
	t.Helper()
	encoded, err := json.Marshal(provider.request(index).Tools)
	if err != nil {
		t.Fatalf("marshal tools: %v", err)
	}
	return string(encoded)
}

func requestToolNames(request rig.Request) []string {
	names := make([]string, 0, len(request.Tools))
	for _, spec := range request.Tools {
		names = append(names, spec.Name)
	}
	return names
}

// TestLazyToolsSkillIsAlwaysActive：skill 是常驻工具（小 schema、开工前必需），
// 且**永不出现在 tool_search 清单里**。
func TestLazyToolsSkillIsAlwaysActive(t *testing.T) {
	provider := &scriptProvider{scripts: [][]kit.Chunk{
		toolCallScript(kit.ToolCall{ID: "c1", Name: "tool_search", Arguments: `{}`}),
		textScript("done"),
	}}
	agent := mustNewKitAgent(t, kit.Config{
		Model: "m", Provider: provider, Root: t.TempDir(), Approval: kit.ApprovalAuto,
	})
	if got := agent.Tools(); !contains(got, "skill") {
		t.Fatalf("skill 应当常驻（不依赖发现）：%v", got)
	}
	result, err := agent.Run(context.Background(), kit.RunRequest{
		SessionID: "s", Input: []kit.Message{kit.NewMessage(kit.RoleUser, "what skills are there?")},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var listing string
	for _, message := range result.Messages {
		if message.Role == kit.RoleTool {
			listing = message.Text()
		}
	}
	if strings.Contains(listing, "skill") {
		t.Fatalf("tool_search 清单不该列出常驻的 skill：\n%s", listing)
	}
}

// TestLazyToolsPresentNeedsPresenter：present 由宿主依赖门控——没有 Presenter 时
// 既不注册（死工具），有了就保留**并进初始 active 集**。
func TestLazyToolsPresentNeedsPresenter(t *testing.T) {
	without, err := kit.New(kit.Config{
		Model: "m", Provider: &scriptProvider{scripts: [][]kit.Chunk{textScript("x")}}, Root: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	if contains(without.RegisteredTools(), "present") {
		t.Fatalf("没有 Presenter 时 present 不该注册（它必然报错）：%v", without.RegisteredTools())
	}

	with, err := kit.New(kit.Config{
		Model: "m", Provider: &scriptProvider{scripts: [][]kit.Chunk{textScript("x")}}, Root: t.TempDir(),
		Presenter: &probeFilePresenter{},
	})
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	if !contains(with.RegisteredTools(), "present") {
		t.Fatalf("配了 Presenter 之后 present 应当注册：%v", with.RegisteredTools())
	}
	if !contains(with.Tools(), "present") {
		t.Fatalf("配了 Presenter 之后 present 应当初始 active：%v", with.Tools())
	}
}

// probeFilePresenter 满足 rig.FilePresenter（只记录调用）。
type probeFilePresenter struct{ calls int }

func (p *probeFilePresenter) PresentFiles(context.Context, []rig.PresentedFile) error {
	p.calls++
	return nil
}
