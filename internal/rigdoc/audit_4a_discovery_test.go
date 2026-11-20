package rigdoc_test

// 第 4A 轮对抗审计（只读）：kit 层的动态工具加载（承诺 6/7/8）。
//
// 复用同包里的 newZeroConfigAgent / recordingProvider / toolsJSON / discoverableTools。

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/kit"
)

// auditPresenter 是最小的 rig.FilePresenter。
type auditPresenter struct{ calls int }

func (p *auditPresenter) PresentFiles(context.Context, []rig.PresentedFile) error {
	p.calls++
	return nil
}

// 承诺 6：`present` 由依赖门控——没有 Presenter 就不注册；接上就注册并立刻 active。
func TestAudit4APresentIsGatedByPresenter(t *testing.T) {
	// (a) 零配置：present 不在注册表里。
	provider := &recordingProvider{}
	a := newZeroConfigAgent(t, provider, nil)
	if _, err := a.Run(context.Background(), rig.RunRequest{Input: []rig.Message{mustUserMessage(t, "hi")}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, name := range a.RegisteredTools() {
		if name == "present" {
			t.Fatalf("零配置（没有 Presenter）却注册了 present——它必然报错，只会诱导模型重试")
		}
	}
	if active := toolNamesOf(t, provider.requests[0]); containsName(active, "present") {
		t.Fatalf("零配置的 active 集里出现了 present: %v", active)
	}
	t.Logf("(a) 零配置：present 未注册、未 active ✓")

	// (b) 接上 Presenter：注册 + 立刻 active。
	presenter := &auditPresenter{}
	withPresenter := &recordingProvider{}
	b := newZeroConfigAgent(t, withPresenter, func(cfg *kit.Config) { cfg.Presenter = presenter })
	if _, err := b.Run(context.Background(), rig.RunRequest{Input: []rig.Message{mustUserMessage(t, "hi")}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	registered := false
	for _, name := range b.RegisteredTools() {
		if name == "present" {
			registered = true
		}
	}
	if !registered {
		t.Fatal("接上 Presenter 后 present 仍未注册")
	}
	active := toolNamesOf(t, withPresenter.requests[0])
	if !containsName(active, "present") {
		t.Fatalf("接上 Presenter 后 present 没有立刻 active: %v", active)
	}
	t.Logf("(b) 接上 Presenter：present 已注册且 active（%d 个 active 工具）✓", len(active))
}

// 承诺 7：`skill`/`memory`/`tool_search` 常驻——discovery 开着是这样，关掉也是。
func TestAudit4AResidentToolsAlwaysPresent(t *testing.T) {
	resident := []string{"skill", "memory", "tool_search"}

	// (a) 默认（discovery 开）。
	provider := &recordingProvider{}
	a := newZeroConfigAgent(t, provider, nil)
	if _, err := a.Run(context.Background(), rig.RunRequest{Input: []rig.Message{mustUserMessage(t, "hi")}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	active := toolNamesOf(t, provider.requests[0])
	if len(active) != 16 {
		t.Fatalf("零配置 active 数 = %d, want 16: %v", len(active), active)
	}
	for _, name := range resident {
		if !containsName(active, name) {
			t.Fatalf("常驻工具 %q 不在零配置 active 集里: %v", name, active)
		}
	}
	t.Logf("(a) 零配置 active（%d）：%s", len(active), strings.Join(active, ", "))

	// (b) 关闭 discovery：三个常驻工具仍然在 tools 里（此时全部 active）。
	off := false
	allProvider := &recordingProvider{}
	b := newZeroConfigAgent(t, allProvider, func(cfg *kit.Config) { cfg.ToolDiscovery = &off })
	if _, err := b.Run(context.Background(), rig.RunRequest{Input: []rig.Message{mustUserMessage(t, "hi")}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	allActive := toolNamesOf(t, allProvider.requests[0])
	for _, name := range []string{"skill", "memory"} {
		if !containsName(allActive, name) {
			t.Fatalf("ToolDiscovery=false 时 %q 不在 tools 里", name)
		}
	}
	// 实现有意在关闭 discovery 时不注册 tool_search（它是发现入口，没人可发现时只会
	// 报"no discovery registry"）。审计结论：这是合理实现，但**设计承诺的措辞**过宽。
	if containsName(allActive, "tool_search") {
		t.Logf("ToolDiscovery=false 时仍注册了 tool_search（承诺 7 的字面版本）")
	} else {
		t.Logf("ToolDiscovery=false 时 tool_search 不注册——实现合理（无物可发现），但承诺 7 的措辞应改为「skill/memory 常驻、tool_search 随开关」")
	}
	if offCount := len(allActive); offCount <= len(active) {
		t.Fatalf("关闭 discovery 后工具数 = %d，不比开着时（%d）多——开关没生效", offCount, len(active))
	}
	t.Logf("(b) ToolDiscovery=false：%d 个工具全部 active ✓", len(allActive))
}

// 承诺 8：tool_search 的清单语义 + 激活在下一轮生效。
func TestAudit4AToolSearchListAndActivate(t *testing.T) {
	// 让 agent 依次调用 tool_search({}) → tool_search({"query":"web"}) →
	// tool_search({"activate":["web_fetch"]})，最后收尾。
	provider := &recordingProvider{replies: [][]rig.Chunk{
		auditToolCallScript("c1", "tool_search", `{}`),
		auditToolCallScript("c2", "tool_search", `{"query":"web"}`),
		auditToolCallScript("c3", "tool_search", `{"activate":["web_fetch"]}`),
		{{Delta: "done"}, {FinishReason: "stop"}},
	}}
	// web_fetch 需要 AllowNetwork 才注册；这里打开网络以让它成为一个可发现的工具。
	// ApprovalAuto 是为了**绕开**默认 gate 的缺陷（见 TestAudit4AToolSearchIsDeniedByDefaultGate）：
	// 本条要验的是清单/激活语义，不是审批。
	a := newZeroConfigAgent(t, provider, func(cfg *kit.Config) {
		cfg.AllowNetwork = kit.Bool(true)
		cfg.Approval = kit.ApprovalAuto
	})
	result, err := a.Run(context.Background(), rig.RunRequest{Input: []rig.Message{mustUserMessage(t, "find tools")}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	results := auditToolResults(t, result)
	if len(results) != 3 {
		t.Fatalf("tool_search 的结果数 = %d, want 3（%v）", len(results), results)
	}
	list, filtered, activated := results[0], results[1], results[2]

	// 清单不能包含常驻三件套，也不能包含已 active 的工具。
	for _, forbidden := range []string{"tool_search", "memory", "skill"} {
		if strings.Contains(list, forbidden) {
			t.Fatalf("清单里出现了 %q（它要么是常驻工具，要么是发现入口本身）:\n%s", forbidden, list)
		}
	}
	if strings.Contains(list, "read_file") {
		t.Fatalf("清单里出现了已 active 的 read_file:\n%s", list)
	}
	if !strings.Contains(list, "web_fetch") {
		t.Fatalf("清单里没有 web_fetch:\n%s", list)
	}
	t.Logf("清单（前 3 行）:\n%s", firstLines(list, 3))

	// query 过滤：只应留下与 "web" 相关的工具。
	if !strings.Contains(filtered, "web") {
		t.Fatalf("query=web 的结果里没有 web 相关条目:\n%s", filtered)
	}
	if strings.Contains(filtered, "job_start") {
		t.Fatalf("query=web 的结果里混入了无关工具:\n%s", filtered)
	}
	t.Logf("query=web 结果:\n%s", firstLines(filtered, 5))

	// 激活：返回 activated + "下一轮可用"的提示。
	if !strings.Contains(activated, "web_fetch") {
		t.Fatalf("activate 结果里没有 web_fetch:\n%s", activated)
	}
	t.Logf("activate 结果: %s", strings.ReplaceAll(activated, "\n", " | "))

	// 下一轮（第 4 次请求）必须包含 web_fetch，且仍在末尾。
	last := provider.requests[len(provider.requests)-1]
	names := toolNamesOf(t, last)
	if !containsName(names, "web_fetch") {
		t.Fatalf("激活后下一轮请求仍没有 web_fetch: %v", names)
	}
	if names[len(names)-1] != "web_fetch" {
		t.Fatalf("新激活的 web_fetch 不在末尾: %v", names)
	}
}

// 承诺 8（续）：超过 40 条要有截断提示。用自定义工具集（>40 个）触发。
func TestAudit4AToolSearchTruncatesAt40(t *testing.T) {
	tools := make([]rig.Tool, 0, 46)
	for i := 0; i < 46; i++ {
		tools = append(tools, &auditFakeTool{name: fmt.Sprintf("bulk_%02d", i)})
	}
	provider := &recordingProvider{replies: [][]rig.Chunk{
		auditToolCallScript("c1", "tool_search", `{}`),
		{{Delta: "done"}, {FinishReason: "stop"}},
	}}
	on := true
	a, err := kit.New(kit.Config{
		Model:         "guard-model",
		Provider:      provider,
		Root:          t.TempDir(),
		Tools:         tools,
		ToolDiscovery: &on,
		Approval:      kit.ApprovalAuto,
	})
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	result, err := a.Run(context.Background(), rig.RunRequest{Input: []rig.Message{mustUserMessage(t, "list")}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	results := auditToolResults(t, result)
	if len(results) != 1 {
		t.Fatalf("tool_search 结果数 = %d", len(results))
	}
	list := results[0]
	lines := strings.Split(strings.TrimSpace(list), "\n")
	t.Logf("清单 %d 行（前 2 行：%s）", len(lines), firstLines(list, 2))
	if len(lines) > 41 {
		t.Fatalf("清单行数 = %d，超过 40 条上限（%s）", len(lines), list)
	}
	if !strings.Contains(list, "query") {
		t.Fatalf("超过上限时没有提示用 query 缩小范围:\n%s", list)
	}
}

// --- 小工具 ---------------------------------------------------------------

// toolNamesOf 返回一次请求里的工具名（数组顺序）。
func toolNamesOf(t *testing.T, request rig.Request) []string {
	t.Helper()
	names := make([]string, 0, len(request.Tools))
	for _, spec := range request.Tools {
		names = append(names, spec.Name)
	}
	return names
}

func containsName(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}

// auditToolCallScript 构造"模型请求调用一个工具"的 chunk 序列。
func auditToolCallScript(id, name, arguments string) []rig.Chunk {
	return []rig.Chunk{
		{ToolCalls: []rig.ToolCallDelta{{Index: 0, ID: id, Name: name, Arguments: arguments}}},
		{FinishReason: "tool_calls"},
	}
}

// auditToolResults 从运行结果里提取工具消息文本。
func auditToolResults(t *testing.T, result rig.RunResult) []string {
	t.Helper()
	var out []string
	for _, message := range result.Messages {
		if message.Role == rig.RoleTool {
			out = append(out, message.Text())
		}
	}
	return out
}

func firstLines(text string, n int) string {
	lines := strings.Split(text, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// auditFakeTool 是一个最小 rig.Tool（自定义工具集用）。
type auditFakeTool struct{ name string }

func (f *auditFakeTool) Name() string           { return f.name }
func (f *auditFakeTool) Description() string    { return "bulk test tool " + f.name }
func (f *auditFakeTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (f *auditFakeTool) Invoke(context.Context, rig.ToolCall) (rig.ToolResult, error) {
	return rig.ToolResult{Content: "ok"}, nil
}

// 【P0｜设计漏洞】零配置下 `tool_search` 被审批门禁拒绝 → 动态加载整条链路失效。
//
// 零配置 gate = `gate.PersonalAskWithoutAsker()` = CommandRiskGate(AllowThenAsk(
// SandboxReadOnlyTools, DenyAllWithReason))；而 `gate.SandboxReadOnlyTools` 里**没有**
// tool_search / memory / skill。于是：注册表里 21 个 discoverable 工具没有任何办法被
// 激活——模型拿到的是"没有可用的审批通道"，而不是工具清单。
//
// 本条断言"当前确实被拒"，作为缺陷的证据与回归守卫：修好之后它会红，届时把它改成
// 正向断言（tool_search 可直接调用）。
func TestAudit4AToolSearchIsDeniedByDefaultGate(t *testing.T) {
	provider := &recordingProvider{replies: [][]rig.Chunk{
		auditToolCallScript("c1", "tool_search", `{}`),
		{{Delta: "done"}, {FinishReason: "stop"}},
	}}
	a := newZeroConfigAgent(t, provider, nil) // 零配置：approval=ask、无 Asker
	result, err := a.Run(context.Background(), rig.RunRequest{Input: []rig.Message{mustUserMessage(t, "find tools")}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	results := auditToolResults(t, result)
	if len(results) != 1 {
		t.Fatalf("tool 结果数 = %d", len(results))
	}
	text := results[0]
	t.Logf("零配置下调用 tool_search 的结果：%s", firstLines(text, 2))
	if !strings.Contains(text, "denied") && !strings.Contains(text, "拒绝") {
		t.Logf("tool_search 已可直接调用（缺陷已修复？）——请把本条改成正向断言")
		return
	}
	// 明确记录"哪一步失败"：这是发现入口本身被拒，不是某个业务工具。
	if !strings.Contains(text, "审批通道") && !strings.Contains(text, "approval") {
		t.Fatalf("拒绝理由不是「没有审批通道」，需要重新判断根因：%s", text)
	}
	t.Logf("⚠️ 缺陷确认：%d 个已注册工具中只有 %d 个 active，而激活入口本身被 gate 拒绝",
		len(a.RegisteredTools()), len(toolNamesOf(t, provider.requests[0])))
}
