package rigdoc_test

// token 与缓存的守门测试。
//
// 这些约束不是"风格问题"，而是产品的成本与性能天花板：
//
//  1. **active 工具的 schema 每次请求都要发**。工具越多、schema 越大，每个回合的
//     输入 token 就越多；动态工具加载（`kit.Config.ToolDiscovery`，默认开）把它压到
//     核心集，但核心集一旦被人塞进一个胖工具，所有人都在为它付钱。
//  2. **prefix cache 只在请求前缀逐字节稳定时才命中**。动态加载的"纯增量激活"就是
//     为了保住这一点；它的三条不变量在本文件里用**真实零配置 agent** 做跨包复验，
//     防止将来有人改默认集或改激活顺序时静默破坏它。
//  3. **默认集里每个工具都必须真的能用**（审计 2C 的教训：13 个"注册了但永远失败"
//     的工具有效地把模型引向了死路）。这条用探针逐个调用验证。
//
// 这里的三条测试与 `kit/lazy_tools_test.go`、`agent/lazy_tools_test.go` 有重叠，是
// 有意的：那两处守实现，这里守**默认装配**——默认值是最容易被"顺手改一下"的地方。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/agent"
	"github.com/zeokeel/zeokeel-rig/events"
	"github.com/zeokeel/zeokeel-rig/gate"
	"github.com/zeokeel/zeokeel-rig/kit"
	"github.com/zeokeel/zeokeel-rig/tools/full"
)

// activeToolsetBudgetBytes 是零配置 active 集 tools JSON 的上限。
//
// 19 KB ≈ 4.8k token（按 ~4 字节/token 估）。当前实测见 TestActiveToolsetTokenBudget
// 的日志输出。留出约 1 KB 的余量：微调描述可以，塞一个新工具（通常 1–2 KB）不行。
const activeToolsetBudgetBytes = 19 * 1024

// coreSchemaBudgetBytes 是"常驻工具"单个 schema 的上限。
//
// 2.5 KB ≈ 640 token。这是**硬上限**；几个必须持续瘦身的工具另有 coreSchemaTargets
// 的更紧目标（常驻 schema 是每个回合都付的钱，压得下去就该压）。
const coreSchemaBudgetBytes = int(2.5 * 1024)

// coreSchemaTargets 是个别常驻工具的**实测目标**（字节）。
//
// 这些值不是拍的：每一次都对应一次真实的瘦身，钉在这里防回涨。加条目的门槛是
// "它真的每个回合都要在"，否则应该改成 discoverable 而不是放宽目标。
var coreSchemaTargets = map[string]int{
	// memory 旧版 6 动作曾压到 1083 字节；v2 扩到 20 个动作 + 19 个参数（find/
	// remember/commit/rules/版本等），实测 2385 字节（≈596 tokens）。仍然只写
	// "一句话 + 一行 action 列表 + 短参数说明"，细节交给运行时错误；目标放到
	// 2400 字节，防止继续膨胀。
	"memory": 2400,
}

// coreSchemaWhitelist 是允许超过 coreSchemaBudgetBytes 的工具（名字 → 理由）。
//
// 加白名单之前先问：这个工具真的需要**每次**都在吗？能不能改成 discoverable？
var coreSchemaWhitelist = map[string]string{}

// recordingProvider 记录每次 Stream 的 Request.Tools，并回一段收尾文本。
type recordingProvider struct {
	requests []rig.Request
	replies  [][]rig.Chunk
	index    int
}

func (p *recordingProvider) Name() string { return "rigdoc-recorder" }

func (p *recordingProvider) Stream(ctx context.Context, req rig.Request) (rig.Stream, error) {
	p.requests = append(p.requests, req)
	var chunks []rig.Chunk
	if p.index < len(p.replies) {
		chunks = p.replies[p.index]
	} else {
		chunks = []rig.Chunk{{Delta: "ok"}, {FinishReason: "stop"}}
	}
	p.index++
	return &sliceStream{chunks: chunks}, nil
}

// sliceStream 是一个最小的 rig.Stream。
type sliceStream struct{ chunks []rig.Chunk }

func (s *sliceStream) Recv() (rig.Chunk, error) {
	if len(s.chunks) == 0 {
		return rig.Chunk{}, io.EOF
	}
	chunk := s.chunks[0]
	s.chunks = s.chunks[1:]
	return chunk, nil
}

// newZeroConfigAgent 装配一个**真实**的零配置 agent（只有一个 provider 与工作区根）。
func newZeroConfigAgent(t *testing.T, provider rig.Provider, extra func(*kit.Config)) *kit.Agent {
	t.Helper()
	cfg := kit.Config{
		Model:    "guard-model",
		Provider: provider,
		Root:     t.TempDir(),
	}
	if extra != nil {
		extra(&cfg)
	}
	a, err := kit.New(cfg)
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	return a
}

// toolsJSON 把一次请求的 Tools 编码成 JSON（比较"逐字节相同"的标准）。
func toolsJSON(t *testing.T, specs []rig.ToolSpec) []byte {
	t.Helper()
	encoded, err := json.Marshal(specs)
	if err != nil {
		t.Fatalf("marshal tools: %v", err)
	}
	return encoded
}

// tokenEstimate 用"字节 / 4"估算 token（英文 schema 的经验值，仅用于报告量级）。
func tokenEstimate(bytes int) int { return bytes / 4 }

// TestActiveToolsetTokenBudget 守零配置 active 集的体积天花板。
//
// 失败时按字节倒序列出最胖的 5 个工具，直接指出是谁把预算顶上去的。
func TestActiveToolsetTokenBudget(t *testing.T) {
	provider := &recordingProvider{}
	a := newZeroConfigAgent(t, provider, nil)
	if _, err := a.Run(context.Background(), rig.RunRequest{Input: []rig.Message{mustUserMessage(t, "hi")}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(provider.requests) == 0 {
		t.Fatal("provider 没有收到请求")
	}
	specs := provider.requests[0].Tools
	encoded := toolsJSON(t, specs)

	type entry struct {
		name  string
		bytes int
	}
	sizes := make([]entry, 0, len(specs))
	for _, spec := range specs {
		sizes = append(sizes, entry{name: spec.Name, bytes: len(toolsJSON(t, []rig.ToolSpec{spec}))})
	}
	sort.Slice(sizes, func(i, j int) bool {
		if sizes[i].bytes != sizes[j].bytes {
			return sizes[i].bytes > sizes[j].bytes
		}
		return sizes[i].name < sizes[j].name
	})
	top := sizes
	if len(top) > 5 {
		top = top[:5]
	}
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, spec.Name)
	}
	t.Logf("零配置 active 集：%d 个工具，tools JSON = %d 字节 ≈ %d tokens",
		len(specs), len(encoded), tokenEstimate(len(encoded)))
	t.Logf("  工具名单（注册顺序）：%s", strings.Join(names, ", "))
	for _, item := range top {
		t.Logf("  最胖：%-22s %5d 字节", item.name, item.bytes)
	}

	if len(encoded) > activeToolsetBudgetBytes {
		var detail strings.Builder
		for _, item := range top {
			fmt.Fprintf(&detail, "\n  %-22s %5d 字节", item.name, item.bytes)
		}
		t.Fatalf("零配置 active 集 tools JSON = %d 字节（≈%d tokens），超过上限 %d 字节。最胖的 5 个：%s\n"+
			"要么把工具改成 discoverable（不进初始集），要么缩小它的 schema。",
			len(encoded), tokenEstimate(len(encoded)), activeToolsetBudgetBytes, detail.String())
	}
}

// TestCoreToolSchemaBudget 守"常驻工具"的单个 schema 体积。
//
// 只量**初始 active 集**里的工具：discoverable 的胖工具不影响每个回合的成本（用到
// 才加载），没必要限制。
func TestCoreToolSchemaBudget(t *testing.T) {
	provider := &recordingProvider{}
	a := newZeroConfigAgent(t, provider, nil)
	if _, err := a.Run(context.Background(), rig.RunRequest{Input: []rig.Message{mustUserMessage(t, "hi")}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	specs := provider.requests[0].Tools
	if len(specs) == 0 {
		t.Fatal("active 集为空")
	}
	for _, spec := range specs {
		size := len(toolsJSON(t, []rig.ToolSpec{spec}))
		if target, ok := coreSchemaTargets[spec.Name]; ok {
			if size > target {
				t.Fatalf("常驻工具 %q 的 schema = %d 字节（≈%d tokens），超过实测目标 %d 字节。\n"+
					"它的体积是每个回合都付的钱：请把细节移到运行时错误/输出，或更新目标并说明理由。",
					spec.Name, size, tokenEstimate(size), target)
			}
			t.Logf("  %-22s %5d 字节 ✓（目标 ≤%d）", spec.Name, size, target)
			continue
		}
		if size <= coreSchemaBudgetBytes {
			t.Logf("  %-22s %5d 字节 ✓", spec.Name, size)
			continue
		}
		if reason, ok := coreSchemaWhitelist[spec.Name]; ok {
			t.Logf("  %-22s %5d 字节（白名单：%s）", spec.Name, size, reason)
			continue
		}
		t.Fatalf("常驻工具 %q 的 schema = %d 字节（≈%d tokens），超过上限 %d 字节。\n"+
			"它每个回合都要发送：请缩小 schema，或把它移出初始 active 集（改成 discoverable），"+
			"或在 coreSchemaWhitelist 里登记并写明理由。",
			spec.Name, size, tokenEstimate(size), coreSchemaBudgetBytes)
	}
}

// TestActivationAppendOnlyInvariant 用**真实零配置 agent** 复验三条缓存不变量。
//
// 与 kit/agent 包内的用例相比，这里守的是"默认装配"这一层：将来有人改了默认 active
// 集、改了 `alwaysActiveTools`、或把激活改成"重新过滤"，这个测试会立刻红。
func TestActivationAppendOnlyInvariant(t *testing.T) {
	provider := &recordingProvider{replies: [][]rig.Chunk{
		// 三轮：都只请求 read_file（保持形状），最后收尾。
		{{ToolCalls: []rig.ToolCallDelta{{Index: 0, ID: "c1", Name: "read_file"}}}, {FinishReason: "tool_calls"}},
		{{ToolCalls: []rig.ToolCallDelta{{Index: 0, ID: "c2", Name: "read_file"}}}, {FinishReason: "tool_calls"}},
		{{ToolCalls: []rig.ToolCallDelta{{Index: 0, ID: "c3", Name: "read_file"}}}, {FinishReason: "tool_calls"}},
		{{Delta: "done"}, {FinishReason: "stop"}},
	}}
	a := newZeroConfigAgent(t, provider, nil)
	if _, err := a.Run(context.Background(), rig.RunRequest{Input: []rig.Message{mustUserMessage(t, "hi")}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(provider.requests) != 4 {
		t.Fatalf("请求数 = %d, want 4", len(provider.requests))
	}

	// ① 不激活：连续 3 轮逐字节相同。
	first := toolsJSON(t, provider.requests[0].Tools)
	for index := 1; index <= 3; index++ {
		if got := toolsJSON(t, provider.requests[index].Tools); !bytes.Equal(got, first) {
			t.Fatalf("第 %d 轮 tools 与首轮不同（prefix cache 会失效）：\nfirst: %s\ngot:   %s",
				index, first, got)
		}
	}
	t.Logf("① 不激活：4 轮 tools 逐字节相同（%d 字节）", len(first))

	// ② 激活一个 discoverable 工具：只在末尾追加。
	activator := a // kit.Agent 实现 rig.ToolActivator（按会话、幂等）
	_ = rig.ToolActivator(activator)
	discoverable := discoverableTools(t, a)
	if len(discoverable) == 0 {
		t.Fatal("零配置下没有任何可发现的工具（清单为空，无法验证激活）")
	}
	target := discoverable[0]
	before := append([]rig.ToolSpec(nil), provider.requests[3].Tools...)
	beforeJSON := toolsJSON(t, before)
	if _, unknown := activator.ActivateTools("", target); len(unknown) != 0 {
		t.Fatalf("ActivateTools(%q) 报了 unknown：%v", target, unknown)
	}
	if _, err := a.Run(context.Background(), rig.RunRequest{Input: []rig.Message{mustUserMessage(t, "again")}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	after := provider.requests[len(provider.requests)-1].Tools
	afterJSON := toolsJSON(t, after)
	if len(after) != len(before)+1 {
		t.Fatalf("激活后数量 = %d, want %d（只加不减）", len(after), len(before)+1)
	}
	if after[len(after)-1].Name != target {
		t.Fatalf("新增项 = %q, want %q（必须是末尾追加）", after[len(after)-1].Name, target)
	}
	// 旧数组的每一个字节（除末尾 ']'）都要在新串的同一位置出现。
	prefix := beforeJSON[:len(beforeJSON)-1]
	if !bytes.HasPrefix(afterJSON, prefix) {
		t.Fatalf("激活改变了已有工具的顺序/内容（prefix cache 失效）：\nbefore: %s\nafter:  %s",
			beforeJSON, afterJSON)
	}
	t.Logf("② 激活 %q：%d → %d 个工具，%d → %d 字节，整个旧数组是新数组的前缀",
		target, len(before), len(after), len(beforeJSON), len(afterJSON))

	// ③ 重复激活 / 未知名字：不改变。
	beforeRepeat := toolsJSON(t, after)
	activator.ActivateTools("", target)
	activator.ActivateTools("", "definitely-not-a-tool")
	if _, err := a.Run(context.Background(), rig.RunRequest{Input: []rig.Message{mustUserMessage(t, "third")}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := toolsJSON(t, provider.requests[len(provider.requests)-1].Tools); !bytes.Equal(got, beforeRepeat) {
		t.Fatalf("重复/未知激活改变了 tools：\nbefore: %s\nafter:  %s", beforeRepeat, got)
	}
	t.Logf("③ 重复 + 未知激活：tools 不变（%d 字节）", len(beforeRepeat))
}

// TestEveryActiveToolIsWired 断言默认 active 集里**每个工具都真的能用**。
//
// 做法：用探针逐个调用（参数取最小合法值），任何 "not configured / no X
// implementation / 未配置" 的返回都算失败——那是"注册了但永远失败"的死工具，
// 正是审计 2C 抓到的问题。业务语义上的失败（文件不存在、参数不合法）不算。
func TestEveryActiveToolIsWired(t *testing.T) {
	// 探针参数：每个工具的"最小合法调用"。没列的工具用 {}（多数工具会回参数错误，
	// 那是可接受的：说明它被装配了，只是我们没给参数）。
	arguments := map[string]string{
		"read_file":          `{"path":"probe.txt"}`,
		"write_file":         `{"path":"probe.txt","content":"x"}`,
		"edit_file":          `{"path":"probe.txt","old":"a","new":"b"}`,
		"str_replace_editor": `{"command":"view","path":"probe.txt"}`,
		"apply_patch":        `{"patch":"*** Begin Patch\n*** End Patch\n"}`,
		"list_dir":           `{"path":"."}`,
		"glob":               `{"pattern":"*"}`,
		"grep":               `{"pattern":"x","path":"."}`,
		"search":             `{"query":"x"}`,
		"bash":               `{"command":"true"}`,
		"todo_write":         `{"todos":[]}`,
		"working_directory":  `{"path":"."}`,
		"read_image":         `{"path":"probe.png"}`,
		"memory":             `{"action":"list"}`,
		"skill":              `{}`,
		"present":            `{"files":["probe.txt"]}`,
		"tool_search":        `{}`,
	}
	directory := t.TempDir()
	provider := &recordingProvider{}
	a := newZeroConfigAgent(t, provider, func(cfg *kit.Config) { cfg.Root = directory })
	if _, err := a.Run(context.Background(), rig.RunRequest{Input: []rig.Message{mustUserMessage(t, "hi")}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	active := make([]rig.ToolSpec, 0, len(provider.requests[0].Tools))
	active = append(active, provider.requests[0].Tools...)
	if len(active) == 0 {
		t.Fatal("零配置下没有 active 工具")
	}

	// 零配置的工具集就是 full.All（kit 只在其上做预设/体检过滤），因此直接用同一
	// 构造器拿具体工具来探针；顺带断言每个 active 名字都真的在其中。
	tools := map[string]rig.Tool{}
	for _, tool := range full.All(full.AllOptions{Root: directory}) {
		tools[tool.Name()] = tool
	}
	for _, spec := range active {
		tool, ok := tools[spec.Name]
		if !ok {
			t.Fatalf("active 工具 %q 不在注册表里", spec.Name)
		}
		payload, ok := arguments[spec.Name]
		if !ok {
			payload = `{}`
		}
		result, err := tool.Invoke(context.Background(), rig.ToolCall{
			ID: "probe-" + spec.Name, Name: spec.Name, Arguments: payload,
		})
		if err != nil {
			t.Fatalf("active 工具 %q 调用返回 error（说明它没被正确装配）：%v", spec.Name, err)
		}
		text := strings.ToLower(result.Content)
		for _, pattern := range deadToolPatterns {
			if strings.Contains(text, pattern) {
				t.Fatalf("active 工具 %q 是死工具（命中 %q）：%s", spec.Name, pattern, firstLineOf(result.Content))
			}
		}
	}
	t.Logf("active 集 %d 个工具全部可调用（无 not-configured / no-implementation）", len(active))

	// 光"能调用"不够：它们还得在**零配置默认审批档**（ask + 无 Asker）下被放行。
	// 这条断言是 4A 审计抓到的教训——当时 tool_search/skill/memory/todo_write 都能
	// 调用，但默认档下被门禁拒绝，等于不存在。
	needsApproval := map[string]bool{
		"write_file": true, "edit_file": true, "apply_patch": true,
		"str_replace_editor": true, "bash": true, "present": true,
	}
	defaultGate := gate.PersonalAskWithoutAsker()
	freeOfAsk := 0
	for _, spec := range active {
		decision, err := defaultGate.Approve(context.Background(),
			&probeTool{name: spec.Name}, rig.ToolCall{ID: "gate-probe", Name: spec.Name, Arguments: `{}`})
		if err != nil {
			t.Fatalf("gate.Approve(%q): %v", spec.Name, err)
		}
		allowed := decision == rig.DecisionAllow || decision == rig.DecisionAllowAlways
		if needsApproval[spec.Name] {
			if allowed {
				t.Fatalf("active 工具 %q 触及用户资源，默认档下不该放行", spec.Name)
			}
			continue
		}
		if !allowed {
			t.Fatalf("active 工具 %q 在默认档下被门禁拒绝（decision=%s）——「常驻但不可用」就是欺骗。"+
				"要么把它加进 gate.SandboxReadOnlyTools（连同理由），要么别让它常驻。",
				spec.Name, decision)
		}
		freeOfAsk++
	}
	t.Logf("默认审批档：%d 个免问、%d 个问人（写/执行类）", freeOfAsk, len(active)-freeOfAsk)
}

// deadToolPatterns 是"工具没接上宿主能力"的特征串。
var deadToolPatterns = []string{
	"not configured",
	"no interactive ui",
	"did not provide a",
	"no such implementation",
	"未配置",
	"没有配置",
}

// probeTool 是最小的 rig.Tool（只用于门禁判定，不会被调用）。
type probeTool struct{ name string }

func (p *probeTool) Name() string           { return p.name }
func (p *probeTool) Description() string    { return "probe" }
func (p *probeTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (p *probeTool) Invoke(context.Context, rig.ToolCall) (rig.ToolResult, error) {
	return rig.ToolResult{Content: "probe"}, nil
}

// discoverableTools 返回零配置下"已注册但未 active"的工具名（按注册顺序）。
func discoverableTools(t *testing.T, a *kit.Agent) []string {
	t.Helper()
	active := make(map[string]struct{})
	for _, name := range a.Tools() {
		active[name] = struct{}{}
	}
	var out []string
	for _, name := range a.RegisteredTools() {
		if _, ok := active[name]; !ok {
			out = append(out, name)
		}
	}
	return out
}

func mustUserMessage(t *testing.T, text string) rig.Message {
	t.Helper()
	return rig.NewMessage(rig.RoleUser, text)
}

// firstLineOf 取输出的第一行（失败信息要短）。
func firstLineOf(text string) string {
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		text = text[:index]
	}
	if len(text) > 200 {
		text = text[:200] + "…"
	}
	return text
}

// 让 events 的引用不落空：动态加载事件的 kind 在 kit 里用，这里顺带断言它是稳定的。
var _ = events.KindToolActivated
var _ = agent.ToolSessionID
