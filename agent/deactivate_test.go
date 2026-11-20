package agent_test

// 取消激活（rig.ToolDeactivator）在 agent 层的契约：
//
//  1. 从会话的 active 集合里移除，下一次请求的 tools 数组里不再出现它；
//  2. tool_search / memory / skill 是**强制并入**的，移除要返回可读错误而不是
//     静默忽略（整批 all-or-nothing：报错时一个都不动）；
//  3. 移除结果写回 rig.ToolActivationStore（读-改-写：先重新 Load，再整体 Save），
//     因此共用 store 的另一个 Agent 恢复后看不到被取消激活的工具，也不会丢掉
//     别的 Agent 的激活。

import (
	"context"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
)

// runSession 在指定会话上跑一轮纯文本运行（断言里只关心请求的 tools）。
func runSession(t *testing.T, a rig.Agent, sessionID string) error {
	t.Helper()
	_, err := a.Run(context.Background(), rig.RunRequest{
		SessionID: sessionID,
		Input:     []rig.Message{userMessage("go")},
	})
	return err
}

// deactivateFixture 构造 6 个工具（含三个强制常驻）且初始全部 active 的规格。
func deactivateFixture(t *testing.T, store rig.ToolActivationStore, scripts [][]rig.Chunk) (rig.Agent, *scriptProvider) {
	t.Helper()
	names := []string{"tool_search", "memory", "skill", "read_file", "write_file", "grep"}
	tools := make([]rig.Tool, 0, len(names))
	for _, name := range names {
		tools = append(tools, &fakeTool{name: name})
	}
	provider := &scriptProvider{scripts: scripts}
	a := mustNewAgent(t, rig.AgentSpec{
		Model:              "m",
		Provider:           provider,
		Tools:              tools,
		InitialActiveTools: append([]string(nil), names...),
		ActivationStore:    store,
	})
	return a, provider
}

// toolActivation 是"激活 + 取消激活"两个可选扩展的合集，测试里一次断言。
type toolActivation interface {
	rig.ToolActivator
	rig.ToolDeactivator
}

// mustDeactivator 断言 Agent 实现了取消激活扩展。
func mustDeactivator(t *testing.T, a rig.Agent) toolActivation {
	t.Helper()
	deactivator, ok := a.(toolActivation)
	if !ok {
		t.Fatal("agent 必须实现 rig.ToolActivator 与 rig.ToolDeactivator")
	}
	return deactivator
}

// TestDeactivateToolsRemovesFromActiveSet：移除后 active 集合与请求里的 tools
// 都不再包含它，其余工具保持原顺序。
func TestDeactivateToolsRemovesFromActiveSet(t *testing.T) {
	a, provider := deactivateFixture(t, nil, [][]rig.Chunk{textScript("ok")})
	deactivator := mustDeactivator(t, a)

	removed, err := deactivator.DeactivateTools("s1", "write_file")
	if err != nil {
		t.Fatalf("DeactivateTools: %v", err)
	}
	if strings.Join(removed, ",") != "write_file" {
		t.Fatalf("removed = %v, want [write_file]", removed)
	}
	if got := strings.Join(deactivator.ActiveTools("s1"), ","); got != "tool_search,memory,skill,read_file,grep" {
		t.Fatalf("active = %v", got)
	}
	// 幂等：再移除一次不是错误，也不算"移除了"。
	again, err := deactivator.DeactivateTools("s1", "write_file")
	if err != nil || len(again) != 0 {
		t.Fatalf("重复移除 = (%v, %v)，want 空且无错", again, err)
	}
	// 未注册的名字同样静默忽略。
	if removed, err := deactivator.DeactivateTools("s1", "nope"); err != nil || len(removed) != 0 {
		t.Fatalf("未知名字 = (%v, %v)", removed, err)
	}

	if err := runSession(t, a, "s1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := strings.Join(toolNames(provider.request(0)), ",")
	if got != "tool_search,memory,skill,read_file,grep" {
		t.Fatalf("Run 的 tools = %v（write_file 仍在请求里）", got)
	}
}

// TestDeactivateToolsRefusesMandatoryTools：三个强制并入的工具不可移除；报错信息
// 要指名道姓，且整批 all-or-nothing。
func TestDeactivateToolsRefusesMandatoryTools(t *testing.T) {
	a, _ := deactivateFixture(t, nil, nil)
	deactivator := mustDeactivator(t, a)
	before := strings.Join(deactivator.ActiveTools("s1"), ",")

	for _, mandatory := range []string{"tool_search", "memory", "skill"} {
		removed, err := deactivator.DeactivateTools("s1", mandatory)
		if err == nil {
			t.Fatalf("DeactivateTools(%q) 应当报错，removed = %v", mandatory, removed)
		}
		if !strings.Contains(err.Error(), mandatory) || !strings.Contains(err.Error(), "always active") {
			t.Fatalf("DeactivateTools(%q) 的错误不可读: %v", mandatory, err)
		}
		if got := strings.Join(deactivator.ActiveTools("s1"), ","); got != before {
			t.Fatalf("拒绝后 active 被改动: before=%v after=%v", before, got)
		}
	}
	// 一批里混入强制工具：整批拒绝，合法名字也不动。
	if _, err := deactivator.DeactivateTools("s1", "write_file", "memory"); err == nil {
		t.Fatal("混入 memory 的批量移除应当报错")
	}
	if got := strings.Join(deactivator.ActiveTools("s1"), ","); got != before {
		t.Fatalf("整批拒绝后 active 被改动: before=%v after=%v", before, got)
	}
}

// TestDeactivateToolsPersistsToActivationStore：移除结果按会话落盘，共用 store 的
// 第二个 Agent 恢复后仍看不到它；同时不能覆盖别的 Agent 的激活。
func TestDeactivateToolsPersistsToActivationStore(t *testing.T) {
	store := newActivationStoreSpy()
	first, _ := deactivateFixture(t, store, nil)
	firstActivator := first.(rig.ToolActivator)
	firstDeactivator := mustDeactivator(t, first)

	if activated, _ := firstActivator.ActivateTools("s1", "write_file"); len(activated) != 1 {
		t.Fatalf("ActivateTools = %v", activated)
	}
	if got := strings.Join(store.saved("s1"), ","); !strings.Contains(got, "write_file") {
		t.Fatalf("激活没有落盘: %v", got)
	}
	removed, err := firstDeactivator.DeactivateTools("s1", "write_file")
	if err != nil || len(removed) != 1 {
		t.Fatalf("DeactivateTools = (%v, %v)", removed, err)
	}
	if got := strings.Join(store.saved("s1"), ","); strings.Contains(got, "write_file") {
		t.Fatalf("移除没有落盘: %v", got)
	}

	// 另一个 Agent 用同一 store / SessionID：恢复后 write_file 仍不在 active。
	second, _ := deactivateFixture(t, store, nil)
	if got := second.(rig.ToolActivator).ActiveTools("s1"); strings.Contains(strings.Join(got, ","), "write_file") {
		t.Fatalf("第二个 Agent 的 s1 active = %v（恢复出了已取消激活的工具）", got)
	}

	// 移除必须基于重新 Load 的最新集合，不能把别的 Agent 的激活覆盖掉。
	third, _ := deactivateFixture(t, store, nil)
	third.(rig.ToolActivator).ActivateTools("s1", "grep")
	if _, err := firstDeactivator.DeactivateTools("s1", "read_file"); err != nil {
		t.Fatalf("DeactivateTools: %v", err)
	}
	saved := sortedNames(store.saved("s1"))
	if !strings.Contains(saved, "grep") {
		t.Fatalf("取消激活覆盖了别的 Agent 的激活：store = %v", saved)
	}
	if strings.Contains(saved, "read_file") {
		t.Fatalf("取消激活没有落盘：store = %v", saved)
	}
}

// TestDeactivateToolsWithoutInitialActiveSet：InitialActiveTools == nil（全部 active）
// 时也能移除，并立即反映到 ActiveTools 与请求里（此时不涉及持久化）。
func TestDeactivateToolsWithoutInitialActiveSet(t *testing.T) {
	provider := &scriptProvider{scripts: [][]rig.Chunk{textScript("ok")}}
	a := mustNewAgent(t, rig.AgentSpec{
		Model:    "m",
		Provider: provider,
		Tools:    []rig.Tool{&fakeTool{name: "read_file"}, &fakeTool{name: "grep"}},
	})
	deactivator := mustDeactivator(t, a)
	if removed, err := deactivator.DeactivateTools("", "grep"); err != nil || len(removed) != 1 {
		t.Fatalf("DeactivateTools = (%v, %v)", removed, err)
	}
	if got := strings.Join(deactivator.ActiveTools(""), ","); got != "read_file" {
		t.Fatalf("active = %v", got)
	}
	if err := runOnce(t, a); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := strings.Join(toolNames(provider.request(0)), ","); got != "read_file" {
		t.Fatalf("Run 的 tools = %v", got)
	}
}

// TestDeactivateAllPersistsEmptySetAcrossRestart：把会话里**全部可移除工具**都取消
// 激活后，落盘记录是"有记录且为空"，不是"没有记录"——新 Agent 载入后仍然是空集，
// 不会把 InitialActiveTools 复活。空切片与无记录必须由 store 用不同信号表达
// （rig.ErrNoActivationRecord）。
func TestDeactivateAllPersistsEmptySetAcrossRestart(t *testing.T) {
	store := newActivationStoreSpy()
	names := []string{"read_file", "write_file", "grep"}
	build := func() rig.Agent {
		tools := make([]rig.Tool, 0, len(names))
		for _, name := range names {
			tools = append(tools, &fakeTool{name: name})
		}
		return mustNewAgent(t, rig.AgentSpec{
			Model:              "m",
			Provider:           &scriptProvider{},
			Tools:              tools,
			InitialActiveTools: append([]string(nil), names...),
			ActivationStore:    store,
		})
	}
	first := build()
	removed, err := mustDeactivator(t, first).DeactivateTools("s1", names...)
	if err != nil || len(removed) != len(names) {
		t.Fatalf("DeactivateTools = (%v, %v)", removed, err)
	}
	if got := first.(rig.ToolActivator).ActiveTools("s1"); len(got) != 0 {
		t.Fatalf("取消激活后 active = %v, want 空", got)
	}
	// 新 Agent（同 store / SessionID）不能让初始 active 复活。
	second := build()
	if got := second.(rig.ToolActivator).ActiveTools("s1"); len(got) != 0 {
		t.Fatalf("重启后取消激活的工具复活了：active = %v（store = %v）", got, store.saved("s1"))
	}
	// 别的会话没有记录，仍然从 InitialActiveTools 出发。
	if got := second.(rig.ToolActivator).ActiveTools("s2"); len(got) != len(names) {
		t.Fatalf("s2 的 active = %v, want %v（空记录被当成了全局空集）", got, names)
	}
}

// TestLoadRecordAlwaysKeepsMandatoryTools：记录是权威全集，但 tool_search / memory /
// skill 是**无条件并入驻留**的：别的工具面更小的 Agent（或宿主自写 store、旧记录）
// 写下的记录里没有它们时，载入后仍必须在 active 集合里（否则模型失去发现入口，而
// DeactivateTools 又拒绝对它们做同样的事）。非强制常驻的初始工具仍按记录被取消。
func TestLoadRecordAlwaysKeepsMandatoryTools(t *testing.T) {
	store := newActivationStoreSpy()
	// A：工具面里只有 read_file，写下记录 [read_file]。
	small := mustNewAgent(t, rig.AgentSpec{
		Model:              "m",
		Provider:           &scriptProvider{},
		Tools:              []rig.Tool{&fakeTool{name: "read_file"}},
		InitialActiveTools: []string{"read_file"},
		ActivationStore:    store,
	})
	if activated, _ := small.(rig.ToolActivator).ActivateTools("s1", "read_file"); len(activated) != 1 {
		t.Fatalf("ActivateTools = %v", activated)
	}
	if got := store.saved("s1"); strings.Join(got, ",") != "read_file" {
		t.Fatalf("A 落盘记录 = %v, want [read_file]", got)
	}

	// B：工具面含三个强制常驻工具，初始全部 active。载入 A 的记录后强制工具不能消失，
	// 其余初始工具（grep）按记录保持取消激活。
	names := []string{"read_file", "tool_search", "memory", "grep"}
	tools := make([]rig.Tool, 0, len(names))
	for _, name := range names {
		tools = append(tools, &fakeTool{name: name})
	}
	big := mustNewAgent(t, rig.AgentSpec{
		Model:              "m",
		Provider:           &scriptProvider{},
		Tools:              tools,
		InitialActiveTools: append([]string(nil), names...),
		ActivationStore:    store,
	})
	got := big.(rig.ToolActivator).ActiveTools("s1")
	if want := "read_file,tool_search,memory"; strings.Join(got, ",") != want {
		t.Fatalf("载入记录后的 active = %v, want %v（强制常驻并入、非强制按记录取消）", got, want)
	}
	// 强制常驻工具不能因为"被别人的记录移除"而落到不可恢复的境地：仍然拒绝取消激活。
	for _, mandatory := range []string{"tool_search", "memory"} {
		if _, err := mustDeactivator(t, big).DeactivateTools("s1", mandatory); err == nil {
			t.Fatalf("DeactivateTools(%q) 应当报错", mandatory)
		}
	}
}
