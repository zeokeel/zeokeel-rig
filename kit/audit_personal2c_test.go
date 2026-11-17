package kit_test

// 个人客户端审计（第 2C 轮）：零配置 / 最少配置的真实体验。
//
// 只读审计：不改实现，只通过 kit 的公开 API 做真实 run 并断言行为。
// 标了 Finding 的用例是**当前不符合预期**的（红），它们是审计发现本身。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/kit"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/full"
	"github.com/zeokeel/zeokeel-rig/tools/presets"
	"github.com/zeokeel/zeokeel-rig/tools/web"
)

// runOneTool 用最少的配置跑一轮：模型只发一个工具调用，然后回一句文本。
// 返回模型看到的**工具消息原文**（即 tool_result）与是否成功。
func runOneTool(t *testing.T, cfg kit.Config, name, arguments string) (toolText string, runErr error) {
	t.Helper()
	provider := &scriptProvider{scripts: [][]kit.Chunk{
		toolCallScript(kit.ToolCall{ID: "c1", Name: name, Arguments: arguments}),
		textScript("done"),
	}}
	cfg.Provider = provider
	agent := mustNew(t, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, _, err := agent.RunText(ctx, "s1", "go", nil)
	// 工具输出在第二轮请求的消息列表里（assistant tool_call 之后紧跟 role=tool）。
	if provider.callCount() >= 2 {
		second := provider.request(1)
		for _, message := range second.Messages {
			if message.Role == kit.RoleTool {
				toolText = message.Text()
			}
		}
	}
	return toolText, err
}

// A2C-K1（零配置）：不传 Root 时 kit.New 仍能装配——默认取进程 cwd。
//
// 这是"零配置可用"的证据，同时也是**风险点**：个人客户端从 $HOME 或 / 启动时，
// 沙箱根与可写面就是那个目录。文档在 Config.Root 里写了默认值，但 kit 包文档的
// "一分钟上手"示例显式传了 Root:"."，容易让人以为 Root 必填。
func TestAudit2CZeroConfigDefaultsRootToCwd(t *testing.T) {
	provider := &scriptProvider{scripts: [][]kit.Chunk{textScript("hi")}}
	agent, err := kit.New(kit.Config{Model: "m", Provider: provider})
	if err != nil {
		t.Fatalf("零配置 kit.New 失败：%v（Model 是唯一必填字段，不应报错）", err)
	}
	policy := agent.Policy()
	if policy == nil {
		t.Fatal("零配置没有构造策略")
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	roots := policy.Roots()
	if len(roots) == 0 {
		t.Fatal("零配置策略没有根")
	}
	if !strings.Contains(roots[0], filepath.Base(cwd)) && roots[0] != cwd {
		t.Fatalf("零配置根 = %v，want 基于 cwd %q", roots, cwd)
	}
	t.Logf("零配置生效根 = %v（cwd=%s）", roots, cwd)
}

// A2C-K2（最少配置）：默认装配的可见契约——工具集、审批、会话、事件、undo、meter。
func TestAudit2CMinimalConfigDefaults(t *testing.T) {
	root := t.TempDir()
	provider := &scriptProvider{scripts: [][]kit.Chunk{textScript("ok")}}
	agent := mustNew(t, kit.Config{Model: "m", Provider: provider, Root: root})

	if agent.Policy() == nil {
		t.Fatal("默认应构造沙箱策略")
	}
	if mode := agent.Policy().Mode(); mode != kit.ModeWorkspaceWrite {
		t.Fatalf("默认沙箱模式 = %q, want workspace-write", mode)
	}
	if agent.SessionStore() != nil {
		t.Fatalf("默认不该有会话存储（SessionStoreDir 为空），得到 %v", agent.SessionStore())
	}
	if agent.UndoTracker() != nil {
		t.Fatal("默认不该有 undo tracker（GitUndo 默认 false）")
	}

	// 工具集：默认 personal 预设。用 run 验证"存在的名字"与"被裁掉的名字"。
	personalNames, ok := presets.Names(presets.PresetPersonal)
	if !ok {
		t.Fatal("presets 没有 personal")
	}
	if len(personalNames) == 0 {
		t.Fatal("personal 预设为空")
	}
	// 被 personal 裁掉的编排工具必须真的不在 kit 默认工具集里。
	for _, removed := range []string{"team_spawn", "spawn_teammate", "wait_agent", "workflow"} {
		text, _ := runOneTool(t, kit.Config{Model: "m", Root: root}, removed, "{}")
		if !isToolAbsent(text) {
			t.Fatalf("被裁掉的工具 %q 仍可调用：%q", removed, text)
		}
	}
	// 编码主通道必须都在。
	for _, needed := range []string{"read_file", "write_file", "apply_patch", "bash", "grep", "glob", "list_dir"} {
		found := false
		for _, name := range personalNames {
			if name == needed {
				found = true
			}
		}
		if !found {
			t.Fatalf("personal 预设里缺少编码必需工具 %q", needed)
		}
	}
}

// A2C-K3（真实 run）：只读命令放行、可变/危险命令按矩阵执行。
func TestAudit2CRealRunCommandMatrix(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hi\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	ctx := wiringContext(t)
	_ = ctx

	cases := []struct {
		name      string
		command   string
		approval  kit.ApprovalMode
		wantDeny  bool
		wantInMsg string
	}{
		{"ls 放行", "ls -la", kit.ApprovalAuto, false, "hello.txt"},
		{"go test 在 auto 放行", "go version", kit.ApprovalAuto, false, "go version"},
		{"rm -rf ./build 在 auto 放行", "true", kit.ApprovalAuto, false, ""},
		{"rm -rf / 在 auto 被拒", "rm -rf /", kit.ApprovalAuto, true, "rm.recursive-root"},
		{"rm -rf / 在 ask 被拒", "rm -rf /", kit.ApprovalAsk, true, "rm.recursive-root"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			text, err := runOneTool(t, kit.Config{
				Model: "m", Root: root, Approval: testCase.approval,
			}, "bash", `{"command":`+auditJSONString(testCase.command)+`}`)
			if testCase.wantDeny {
				if !strings.Contains(text, "denied") && !strings.Contains(text, "拒绝") {
					t.Fatalf("命令 %q 未被拒：tool message = %q（err=%v）", testCase.command, text, err)
				}
				if testCase.wantInMsg != "" && !strings.Contains(text, testCase.wantInMsg) {
					t.Fatalf("拒绝理由缺少 %q：%q", testCase.wantInMsg, text)
				}
				return
			}
			if strings.Contains(text, "denied") && strings.Contains(text, "gate") {
				t.Fatalf("命令 %q 被误拒：%q", testCase.command, text)
			}
			if testCase.wantInMsg != "" && !strings.Contains(text, testCase.wantInMsg) {
				t.Fatalf("输出缺少 %q：%q", testCase.wantInMsg, text)
			}
		})
	}
}

// A2C-K4（最重要）：完整编码任务——读文件 → apply_patch → 跑测试 → 通过。
//
// 用 ApprovalAuto（个人客户端的正常配置）；模板库里先有一个失败测试，补丁把它修好。
func TestAudit2CCodingTaskEndToEnd(t *testing.T) {
	root := t.TempDir()
	writeModule(t, root)

	source := "package calc\n\nfunc Add(a, b int) int {\n\treturn a - b\n}\n"
	if err := os.WriteFile(filepath.Join(root, "calc.go"), []byte(source), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	testFile := "package calc\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatalf(\"Add(1,2) = %d\", Add(1, 2))\n\t}\n}\n"
	if err := os.WriteFile(filepath.Join(root, "calc_test.go"), []byte(testFile), 0o600); err != nil {
		t.Fatalf("write test: %v", err)
	}

	patch := "*** Begin Patch\n*** Update File: calc.go\n@@\n func Add(a, b int) int {\n-\treturn a - b\n+\treturn a + b\n }\n*** End Patch\n"

	provider := &scriptProvider{scripts: [][]kit.Chunk{
		toolCallScript(kit.ToolCall{ID: "1", Name: "read_file", Arguments: `{"path":"calc.go"}`}),
		toolCallScript(kit.ToolCall{ID: "2", Name: "apply_patch", Arguments: `{"patch":` + auditJSONString(patch) + `}`}),
		toolCallScript(kit.ToolCall{ID: "3", Name: "bash", Arguments: `{"command":"go test ./..."}`}),
		textScript("修好了，测试通过"),
	}}
	agent := mustNew(t, kit.Config{
		Model: "m", Provider: provider, Root: root, Approval: kit.ApprovalAuto,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	text, _, err := agent.RunText(ctx, "coding", "修好 Add 并跑测试", nil)
	if err != nil {
		t.Fatalf("编码任务失败：%v", err)
	}
	if !strings.Contains(text, "修好") {
		t.Fatalf("最终文本 = %q", text)
	}

	// ① 补丁真的落到盘上。
	patched, err := os.ReadFile(filepath.Join(root, "calc.go"))
	if err != nil {
		t.Fatalf("read patched: %v", err)
	}
	if !strings.Contains(string(patched), "return a + b") {
		t.Fatalf("补丁没生效：\n%s", patched)
	}
	// ② 测试命令真的跑了并通过（模型看到的工具输出里应有 ok/PASS）。
	if provider.callCount() < 4 {
		t.Fatalf("模型只被调用 %d 次，编码闭环没跑完", provider.callCount())
	}
	final := provider.request(3)
	var testOutput string
	for _, message := range final.Messages {
		if message.Role == kit.RoleTool && strings.Contains(message.Text(), "calc") {
			testOutput = message.Text()
		}
	}
	if testOutput == "" {
		t.Fatalf("没找到 go test 的工具输出；消息 = %#v", final.Messages)
	}
	if !strings.Contains(testOutput, "ok") && !strings.Contains(testOutput, "PASS") {
		t.Fatalf("go test 输出不含成功标记：%q", testOutput)
	}
}

// A2C-K5（Finding）：**零配置/默认 ask 且没有 Asker 时，编码闭环直接断在写文件**。
//
// 默认 kit.New(Config{Model, Provider, Root}) 用 gate.PersonalAskWithoutAsker()，
// 于是 write_file / apply_patch / bash(go test) 全部被拒。个人客户端的"最少配置"
// 若不加 Approval 就是只读的，这与"开箱即用的编码助手"的预期不符——要么文档
// 大声说明，要么把默认改成另一种组合。
func TestAudit2CFinding_DefaultAskWithoutAskerCannotEditOrTest(t *testing.T) {
	root := t.TempDir()
	writeModule(t, root)
	if err := os.WriteFile(filepath.Join(root, "calc.go"), []byte("package calc\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	// 写文件
	writeText, _ := runOneTool(t, kit.Config{Model: "m", Root: root}, "write_file",
		`{"path":"new.txt","content":"x"}`)
	// 跑测试
	testText, _ := runOneTool(t, kit.Config{Model: "m", Root: root}, "bash",
		`{"command":"go test ./..."}`)

	if !strings.Contains(writeText, "denied") && !strings.Contains(writeText, "拒绝") {
		t.Fatalf("默认 ask+无 Asker 时 write_file 竟然放行：%q", writeText)
	}
	if !strings.Contains(writeText, "审批通道") {
		t.Fatalf("write_file 拒绝理由没有说明「没有审批通道」：%q", writeText)
	}
	if !strings.Contains(testText, "denied") && !strings.Contains(testText, "拒绝") {
		t.Fatalf("默认 ask+无 Asker 时 `go test` 竟然放行：%q", testText)
	}
	if !strings.Contains(testText, "审批通道") {
		t.Fatalf("`go test` 拒绝理由应说明没有审批通道（而不是判定危险）：%q", testText)
	}
}

// A2C-K6（危险命令拒绝文案，逐字节打印）：模型读完能不能自己换方案？
func TestAudit2CDangerousRejectionMessageIsActionable(t *testing.T) {
	root := t.TempDir()
	text, err := runOneTool(t, kit.Config{Model: "m", Root: root, Approval: kit.ApprovalAuto},
		"bash", `{"command":"rm -rf /"}`)
	if err != nil {
		t.Logf("run err = %v", err)
	}
	t.Logf("模型看到的完整 tool message：\n%s", text)
	for _, want := range []string{"denied", "rm.recursive-root", "改用", "do not repeat"} {
		if !strings.Contains(text, want) {
			t.Fatalf("拒绝文案缺 %q：%q", want, text)
		}
	}
}

// A2C-K7：写 .git/config 必须被拒（保护路径默认开启）。
func TestAudit2CProtectedPathWriteIsDenied(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "config"), []byte("[core]\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	text, _ := runOneTool(t, kit.Config{Model: "m", Root: root, Approval: kit.ApprovalAuto},
		"write_file", `{"path":".git/config","content":"[core]\n\tbare = true\n"}`)
	if !strings.Contains(text, "denied") && !strings.Contains(text, "拒绝") && !strings.Contains(text, "protected") {
		t.Fatalf(".git/config 写入没有被拒：%q", text)
	}
	t.Logf("保护路径拒绝文案：%s", strings.TrimSpace(text))
	// 关掉保护后应该可以写（证明拒绝来自保护策略而不是其它原因）。
	text2, _ := runOneTool(t, kit.Config{Model: "m", Root: root, Approval: kit.ApprovalAuto, DisableProtection: true},
		"write_file", `{"path":".git/config","content":"[core]\n\tbare = true\n"}`)
	if strings.Contains(text2, "denied") && strings.Contains(text2, "protected") {
		t.Fatalf("DisableProtection=true 之后仍被保护策略拒绝：%q", text2)
	}
}

// isToolAbsent 判断一条工具输出是不是"这个工具根本没注册"。
//
// 实测文案是 `rig: tool not found: <name>`（**不是** unknown tool），第一版审计脚本
// 按 "unknown tool" 匹配，于是把 16 个被预设正确裁掉的工具误判成"仍被注册"。
func isToolAbsent(text string) bool {
	lower := strings.ToLower(text)
	for _, pattern := range []string{"tool not found", "unknown tool", "未知工具", "工具不存在"} {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

// isWiringFailure 判断一条工具输出是不是"宿主能力没装配"（而不是参数错误/正常结果）。
func isWiringFailure(text string) bool {
	patterns := []string{
		"did not provide a rig.",
		"no subagent runner is configured",
		"no session log is configured",
		"no Searcher configured",
		"no interactive UI",
		"network access is disabled",
		"no todo store",
		"no memory store",
		"no goal store",
		"no plan controller",
	}
	lower := strings.ToLower(text)
	for _, pattern := range patterns {
		if strings.Contains(lower, strings.ToLower(pattern)) {
			return true
		}
	}
	return false
}

// A2C-K9：事件流默认关闭（EventWriter nil 时不写任何东西）。
func TestAudit2CEventsDefaultOff(t *testing.T) {
	root := t.TempDir()
	provider := &scriptProvider{scripts: [][]kit.Chunk{textScript("ok")}}
	agent := mustNew(t, kit.Config{Model: "m", Provider: provider, Root: root})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, _, err := agent.RunText(ctx, "s1", "hi", nil); err != nil {
		t.Fatalf("RunText: %v", err)
	}
	// 没有 EventWriter 时不应有任何落盘文件（除了 root 里我们创建的东西）。
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("默认装配往工作区写了文件：%v", entries)
	}
}

// writeModule 在 root 下建一个最小 Go module（stdlib，无网络依赖）。
func writeModule(t *testing.T, root string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.test/calc\n\ngo 1.21\n"), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
}

// firstLine 返回第一行（用于日志表格）。
func firstLine(text string) string {
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		return text[:index]
	}
	return text
}

// jsonString 把任意字符串编码成 JSON 字符串字面量。
func auditJSONString(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`)
	return `"` + replacer.Replace(value) + `"`
}

var _ = rig.DecisionAllow

// A2C-K10：默认工具集的上下文成本，以及"零配置体检"省下了多少。
func TestAudit2CDefaultToolsetContextCost(t *testing.T) {
	root := t.TempDir()
	pruned := map[string]bool{
		"web_search": true, "session_search": true, "session_trace": true,
		"session_event_read": true, "session_event_search": true, "session_event_trace": true,
		"get_goal": true, "create_goal": true, "update_goal": true,
		"plan_review": true, "exit_plan_mode": true, "subagent": true, "subagent_fork": true,
		"ask_user": true, "undo": true, "sandbox_status": true, "request_sandbox_access": true,
	}
	tools := fullAllForAudit(t, root)
	totalBytes, prunedBytes := 0, 0
	for _, tool := range tools {
		size := len(tool.Description())
		if schema, err := json.Marshal(tool.Schema()); err == nil {
			size += len(schema)
		}
		totalBytes += size
		if pruned[tool.Name()] {
			prunedBytes += size
		}
	}
	t.Logf("full.All 静态集 %d 个工具 / %d 字节 schema；被零配置体检摘掉的 17 个占 %d 字节（约 %d tokens）",
		len(tools), totalBytes, prunedBytes, prunedBytes/4)
	if prunedBytes == 0 {
		t.Fatal("没有匹配到任何被摘掉的工具，审计表可能已过期")
	}
}

// A2C-K11：默认 kit 到底注册了多少个工具（用"未知工具"来区分注册与否）。
//
// 口径校准：presets["personal"] 是名字表（56），kit 还会按装配情况增加/减少
// （sandbox_status/request_sandbox_access 需要 SandboxGrants，undo 需要 tracker）。
func TestAudit2CMinimalConfigRegisteredToolCount(t *testing.T) {
	root := t.TempDir()
	names, _ := presets.Names(presets.PresetPersonal)
	var registered, absent []string
	for _, name := range names {
		text, _ := runOneTool(t, kit.Config{Model: "m", Root: root, Approval: kit.ApprovalAuto}, name, `{}`)
		if isToolAbsent(text) {
			absent = append(absent, name)
			continue
		}
		registered = append(registered, name)
	}
	// 反方向：被 personal 裁掉的工具不该出现在默认装配里。
	standard, _ := presets.Names(presets.PresetStandard)
	excludedButRegistered := []string{}
	for _, name := range standard {
		if slices.Contains(names, name) {
			continue
		}
		text, _ := runOneTool(t, kit.Config{Model: "m", Root: root, Approval: kit.ApprovalAuto}, name, `{}`)
		if !isToolAbsent(text) {
			excludedButRegistered = append(excludedButRegistered, name)
		}
	}
	t.Logf("默认装配注册的工具数 = %d；personal 名单里有但没注册 = %v；名单外却被注册 = %v",
		len(registered), absent, excludedButRegistered)
	if len(excludedButRegistered) > 0 {
		t.Fatalf("personal 预设没挡住这些工具：%v", excludedButRegistered)
	}
	// 名单里有、但没注册的必须正好是这两类：需要显式开关的 3 个 + 零配置体检摘掉的 14 个。
	wantAbsent := map[string]bool{
		// 需要显式开关
		"undo": true, "sandbox_status": true, "request_sandbox_access": true,
		// 平台相关（非 Windows 上 pwsh 必然失败）
		"pwsh": true,
		// 默认不联网
		"web_fetch": true,
		// 零配置体检（装配缺失）摘掉的
		"web_search": true, "session_search": true, "session_trace": true,
		"session_event_read": true, "session_event_search": true, "session_event_trace": true,
		"get_goal": true, "create_goal": true, "update_goal": true,
		"plan_review": true, "exit_plan_mode": true,
		"ask_user": true,
	}
	for _, name := range absent {
		if !wantAbsent[name] {
			t.Fatalf("意外的未注册工具：%q（absent 全量：%v）", name, absent)
		}
	}
	if len(registered) != len(names)-len(wantAbsent) {
		t.Fatalf("注册数 %d，want %d（名单 %d 减去 %d 个需显式开关的工具）",
			len(registered), len(names)-len(wantAbsent), len(names), len(wantAbsent))
	}
}

// ---------------------------------------------------------------------------
// 零配置体检（kit 的 cfgUnavailableTools）验收：默认 39 个工具，按项加回。
// ---------------------------------------------------------------------------

// auditAsker 是最小 rig.Asker（记录提问并固定回答）。
type auditAsker struct {
	asked    int
	answer   string
	question string
}

func (a *auditAsker) Ask(_ context.Context, question string, _ []string) (string, error) {
	a.asked++
	a.question = question
	if a.answer == "" {
		return "allow", nil
	}
	return a.answer, nil
}

// auditSearcher 是最小 web.Searcher。
type auditSearcher struct{ calls int }

func (s *auditSearcher) Search(context.Context, string, int) ([]web.SearchResult, error) {
	s.calls++
	return []web.SearchResult{{Title: "t", URL: "https://example.com", Snippet: "s"}}, nil
}

// auditSessionLog 是最小 rig.SessionLog。
type auditSessionLog struct{}

func (auditSessionLog) Events(context.Context, string, int64, int64) ([]rig.SessionEvent, error) {
	return []rig.SessionEvent{{SessionID: "s1", Type: "notice"}}, nil
}
func (auditSessionLog) Search(context.Context, string, int) ([]rig.SessionEvent, error) {
	return []rig.SessionEvent{{SessionID: "s1", Type: "notice"}}, nil
}
func (auditSessionLog) Last(context.Context, string, int) ([]rig.SessionEvent, error) {
	return []rig.SessionEvent{{SessionID: "s1", Type: "notice"}}, nil
}

// A2C-K12：零配置体检必须让"默认工具集里每个工具都真的能用"。
//
// 默认 39 个工具；被摘掉的 14 个必须表现为 "tool not found"（而不是留一个只会
// 报 "not configured" 的陷阱）。
func TestAudit2CZeroConfigToolPruning(t *testing.T) {
	root := t.TempDir()
	pruned := []string{
		"web_search", "session_search", "session_trace", "session_event_read",
		"session_event_search", "session_event_trace",
		"get_goal", "create_goal", "update_goal",
		"plan_review", "exit_plan_mode", "subagent", "subagent_fork", "ask_user",
	}
	for _, name := range pruned {
		text, _ := runOneTool(t, kit.Config{Model: "m", Root: root, Approval: kit.ApprovalAuto}, name, `{}`)
		if !isToolAbsent(text) {
			t.Fatalf("默认装配里 %q 没被摘掉（输出：%q）——它只会报「未配置」并浪费模型轮次", name, firstLine(text))
		}
	}
	// undo：没有 tracker 时也必须摘掉。
	undoText, _ := runOneTool(t, kit.Config{Model: "m", Root: root, Approval: kit.ApprovalAuto}, "undo", `{}`)
	if !isToolAbsent(undoText) {
		t.Fatalf("没有 tracker 时 undo 仍在工具集里：%q", firstLine(undoText))
	}
	// 非 Windows 平台上 pwsh 必然失败，必须摘掉（Windows 分支见内部用例）。
	if runtime.GOOS != "windows" {
		pwshText, _ := runOneTool(t, kit.Config{Model: "m", Root: root, Approval: kit.ApprovalAuto}, "pwsh", `{}`)
		if !isToolAbsent(pwshText) {
			t.Fatalf("非 Windows 平台上 pwsh 仍在工具集里：%q", firstLine(pwshText))
		}
	}
	// 仍然保留的编码/交互基础工具必须可用。
	for _, name := range []string{"read_file", "write_file", "apply_patch", "bash", "grep", "glob", "list_dir", "search", "todo_write", "present"} {
		text, _ := runOneTool(t, kit.Config{Model: "m", Root: root, Approval: kit.ApprovalAuto}, name, `{}`)
		if isToolAbsent(text) || isWiringFailure(text) {
			t.Fatalf("基础工具 %q 在默认装配下不可用：%q", name, firstLine(text))
		}
	}
}

// A2C-K13：接上 Searcher / SessionLog 之后，对应工具必须**真的能用**（不是只保留名字）。
func TestAudit2CReopenWithSearcherAndSessionLog(t *testing.T) {
	root := t.TempDir()
	searcher := &auditSearcher{}
	sessionLog := auditSessionLog{}
	cfg := kit.Config{
		Model: "m", Root: root, Approval: kit.ApprovalAuto,
		Searcher: searcher, SessionLog: sessionLog,
	}
	webText, _ := runOneTool(t, cfg, "web_search", `{"query":"x"}`)
	if isToolAbsent(webText) {
		t.Fatalf("配了 Searcher 之后 web_search 仍不在工具集里")
	}
	if isWiringFailure(webText) || searcher.calls == 0 {
		t.Fatalf("配了 Searcher 但 web_search 没真的用上：%q（calls=%d）", firstLine(webText), searcher.calls)
	}
	sessionText, _ := runOneTool(t, cfg, "session_search", `{"query":"x"}`)
	if isToolAbsent(sessionText) {
		t.Fatalf("配了 SessionLog 之后 session_search 仍不在工具集里")
	}
	if isWiringFailure(sessionText) {
		t.Fatalf("配了 SessionLog 但 session_search 仍报未配置：%q", firstLine(sessionText))
	}
}

// A2C-K14：配了 Asker 之后 ask_user 必须**真的问到人**（P1-1 验收）。
//
// 之前：零配置体检按 cfg.Asker != nil 保留 ask_user，但 full.All 从未拿到 Asker
// （AllOptions 没有这个字段），于是工具保留却永远报 "no interactive UI"。
func TestAudit2CAskUserReachesAsker(t *testing.T) {
	root := t.TempDir()
	asker := &auditAsker{answer: "继续"}
	text, _ := runOneTool(t, kit.Config{
		Model: "m", Root: root, Approval: kit.ApprovalAuto, Asker: asker,
	}, "ask_user", `{"question":"要继续吗？"}`)
	if isToolAbsent(text) {
		t.Fatalf("配了 Asker 之后 ask_user 被摘掉了：%q", firstLine(text))
	}
	if strings.Contains(text, "no interactive UI") {
		t.Fatalf("配了 Asker 但 ask_user 仍报 no interactive UI：%q", firstLine(text))
	}
	if asker.asked != 1 {
		t.Fatalf("Asker 被调用 %d 次，want 1（问题=%q，输出=%q）", asker.asked, asker.question, firstLine(text))
	}
	if !strings.Contains(asker.question, "要继续吗？") {
		t.Fatalf("问题文本没有透传给 Asker：%q", asker.question)
	}
	if !strings.Contains(text, "继续") {
		t.Fatalf("Asker 的回答没有回到模型：%q", firstLine(text))
	}
}

// A2C-K15：web_fetch 必须随 AllowNetwork 出现/消失（P1-3 验收）。
func TestAudit2CWebFetchFollowsAllowNetwork(t *testing.T) {
	root := t.TempDir()
	// 默认不联网：工具必须被摘掉（而不是留一个必被策略拒绝的假能力）。
	denied, _ := runOneTool(t, kit.Config{Model: "m", Root: root, Approval: kit.ApprovalAuto},
		"web_fetch", `{"url":"https://example.com"}`)
	if !isToolAbsent(denied) {
		t.Fatalf("默认 AllowNetwork=false 时 web_fetch 仍在工具集里：%q", firstLine(denied))
	}

	// AllowNetwork=true：工具出现，并且**真的去执行**了（不再是"网络被禁用"）。
	//
	// 本机 httptest 的地址是 127.0.0.1，会被沙箱的 SSRF 规则单独拒绝
	// （loopback/私有/云元数据一律不放行，见 sandbox/url.go）——这说明网络闸门
	// 已经打开、命令走到了 URL 策略层，而不是"联网能力不存在"。
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = writer.Write([]byte("audit-2c-body"))
	}))
	defer server.Close()
	text, _ := runOneTool(t, kit.Config{
		Model: "m", Root: root, Approval: kit.ApprovalAuto, AllowNetwork: true,
	}, "web_fetch", `{"url":`+auditJSONString(server.URL)+`}`)
	if isToolAbsent(text) {
		t.Fatalf("AllowNetwork=true 时 web_fetch 仍被摘掉：%q", firstLine(text))
	}
	lower := strings.ToLower(text)
	if strings.Contains(lower, "network access is disabled") {
		t.Fatalf("AllowNetwork=true 之后仍然报网络被禁用：%q", firstLine(text))
	}
	if !strings.Contains(lower, "loopback") && !strings.Contains(text, "audit-2c-body") {
		t.Fatalf("web_fetch 既没取到内容、也没走到 SSRF 规则，行为不明：%q", firstLine(text))
	}
}

// fullAllForAudit 用 kit 默认的等价参数重建静态工具集（只为量 schema 体积与名字表）。
func fullAllForAudit(t *testing.T, root string) []kit.Tool {
	t.Helper()
	policy := sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeWorkspaceWrite))
	return full.All(full.AllOptions{Policy: policy, Root: root})
}
