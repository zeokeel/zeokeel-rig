package kit_test

// 审批原语（B4）：审批记忆、会话作用域、同一轮去重、prompt 链。
//
// 这一组的命题是"好用"而不是"权限引擎"：记住的是**用户已经回答过的那个问题**
// （同工具同参数不再打扰），但围栏（沙箱/策略）一条都不放宽；并行发出的同一
// 调用在同一轮里只问一次；审批文案可以由宿主按顺序组合。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/gate"
	"github.com/zeokeel/zeokeel-rig/kit"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/tools/builtin"
)

// approvalAsker 记录被问了几次、问了什么；答案可配（默认 allow）。
type approvalAsker struct {
	mu        sync.Mutex
	calls     int
	questions []string
	answer    string
}

func (a *approvalAsker) Ask(ctx context.Context, question string, options []string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	a.questions = append(a.questions, question)
	if a.answer == "" {
		return "allow", nil
	}
	return a.answer, nil
}

func (a *approvalAsker) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

func (a *approvalAsker) asked() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.questions...)
}

// approvalAgent 装配一个"计数 Asker + 脚本 provider"的 kit agent。
func approvalAgent(t *testing.T, cfg kit.Config) (*kit.Agent, *scriptProvider, *approvalAsker) {
	t.Helper()
	asker := &approvalAsker{answer: "allow"}
	if cfg.Asker == nil {
		cfg.Asker = asker
	}
	if cfg.Model == "" {
		cfg.Model = "m"
	}
	if cfg.Root == "" {
		cfg.Root = t.TempDir()
	}
	provider := &scriptProvider{}
	cfg.Provider = provider
	return mustNewKitAgent(t, cfg), provider, asker
}

// writeCall 构造一次 write_file 调用。
func writeCall(id, path, content string) kit.ToolCall {
	return kit.ToolCall{
		ID:        id,
		Name:      "write_file",
		Arguments: `{"path":"` + path + `","content":"` + content + `"}`,
	}
}

// runToolRound 跑一个"模型请求 calls + 收尾文本"的完整 Run，返回工具结果文本。
func runToolRound(t *testing.T, agent *kit.Agent, provider *scriptProvider, sessionID string, calls ...kit.ToolCall) (string, error) {
	t.Helper()
	provider.scripts = [][]kit.Chunk{toolCallScript(calls...), textScript("done")}
	result, err := agent.Run(context.Background(), kit.RunRequest{
		SessionID: sessionID,
		Input:     []kit.Message{kit.NewMessage(kit.RoleUser, "run it")},
	})
	var texts []string
	for _, message := range result.Messages {
		if message.Role == kit.RoleTool {
			texts = append(texts, message.Text())
		}
	}
	return strings.Join(texts, "\n"), err
}

// approvalRule 构造一条预置的审批记忆规则。
func approvalRule(kind, key, scope, sessionID string) rig.ApprovalRule {
	return rig.ApprovalRule{Kind: kind, Key: key, Scope: scope, SessionID: sessionID, DecidedAt: time.Now()}
}

// TestApprovalMemoryRemembersAllowAlways：第一次 allow-always 写进 store；换一个
// 进程内的 Agent（模拟重启）读同一份 store，同工具同参数不再问 Asker。
func TestApprovalMemoryRemembersAllowAlways(t *testing.T) {
	store := kit.NewMemoryApprovals()
	first, provider, asker := approvalAgent(t, kit.Config{Approvals: store})
	asker.answer = "allow-always"

	if _, err := runToolRound(t, first, provider, "s1", writeCall("c1", "a.txt", "v1")); err != nil {
		t.Fatalf("第一次 Run: %v", err)
	}
	if got := asker.count(); got != 1 {
		t.Fatalf("第一次 Run 应该问 1 次，实际 %d 次", got)
	}
	rules := first.SessionApprovals()
	if len(rules) != 1 {
		t.Fatalf("SessionApprovals = %v，want 1 条（allow-always 应写进 store）", rules)
	}
	if rules[0].Kind != gate.ApprovalKindPath || rules[0].Key != "a.txt" {
		t.Fatalf("规则 = %+v，want kind=%q key=a.txt", rules[0], gate.ApprovalKindPath)
	}

	// 新 Agent（模拟重启）+ 同一份 store + 同一会话：记忆命中，不再问人。
	second, provider2, asker2 := approvalAgent(t, kit.Config{Approvals: store})
	if _, err := runToolRound(t, second, provider2, "s1", writeCall("c2", "a.txt", "v1")); err != nil {
		t.Fatalf("第二次 Run: %v", err)
	}
	if got := asker2.count(); got != 0 {
		t.Fatalf("记忆命中时不该再问人，Asker 被调用 %d 次（want 0）", got)
	}
}

// TestApprovalMemorySessionScopeDoesNotLeak：session 作用域下换会话要重问；
// ForgetApprovals 清掉会话规则后同样重问。
func TestApprovalMemorySessionScopeDoesNotLeak(t *testing.T) {
	store := kit.NewMemoryApprovals()
	if err := store.Save(context.Background(), []rig.ApprovalRule{
		approvalRule(gate.ApprovalKindPath, "a.txt", rig.ApprovalScopeSession, "s1"),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// s1 命中已记住的规则：不问人。
	same, provider, asker := approvalAgent(t, kit.Config{Approvals: store})
	if _, err := runToolRound(t, same, provider, "s1", writeCall("c1", "a.txt", "v1")); err != nil {
		t.Fatalf("会话 s1 的 Run: %v", err)
	}
	if got := asker.count(); got != 0 {
		t.Fatalf("s1 的记忆应命中，Asker 被调用 %d 次（want 0）", got)
	}

	// 换 SessionID：s1 的规则不生效，必须重问。
	other, provider2, asker2 := approvalAgent(t, kit.Config{Approvals: store})
	if _, err := runToolRound(t, other, provider2, "s2", writeCall("c2", "a.txt", "v1")); err != nil {
		t.Fatalf("会话 s2 的 Run: %v", err)
	}
	if got := asker2.count(); got != 1 {
		t.Fatalf("换会话后应该重问（Asker 1 次），实际 %d 次", got)
	}

	// 清掉会话规则：再跑又要问。
	if err := other.ForgetApprovals(context.Background()); err != nil {
		t.Fatalf("ForgetApprovals: %v", err)
	}
	if rules := other.SessionApprovals(); len(rules) != 0 {
		t.Fatalf("ForgetApprovals 之后 SessionApprovals = %v，want 空", rules)
	}
	after, provider3, asker3 := approvalAgent(t, kit.Config{Approvals: store})
	if _, err := runToolRound(t, after, provider3, "s1", writeCall("c3", "a.txt", "v1")); err != nil {
		t.Fatalf("清空后的 Run: %v", err)
	}
	if got := asker3.count(); got != 1 {
		t.Fatalf("清空后应该重问（Asker 1 次），实际 %d 次", got)
	}
}

// TestApprovalMemoryGlobalScopeLeaksAcrossSessions：global 作用域下写入不带
// SessionID，换会话（甚至换 Agent）照样生效。
func TestApprovalMemoryGlobalScopeLeaksAcrossSessions(t *testing.T) {
	store := kit.NewMemoryApprovals()
	first, provider, asker := approvalAgent(t, kit.Config{Approvals: store, ApprovalScope: "global"})
	asker.answer = "allow-always"

	if _, err := runToolRound(t, first, provider, "s1", writeCall("c1", "a.txt", "v1")); err != nil {
		t.Fatalf("会话 s1 的 Run: %v", err)
	}
	rules, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(rules) != 1 || rules[0].Scope != rig.ApprovalScopeGlobal || rules[0].SessionID != "" {
		t.Fatalf("store 里的规则 = %+v，want 一条 Scope=global 且不带 SessionID", rules)
	}

	// 另一个会话、另一个 Agent：global 规则仍然生效。
	second, provider2, asker2 := approvalAgent(t, kit.Config{Approvals: store, ApprovalScope: "global"})
	if _, err := runToolRound(t, second, provider2, "s2", writeCall("c2", "a.txt", "v1")); err != nil {
		t.Fatalf("会话 s2 的 Run: %v", err)
	}
	if got := asker2.count(); got != 0 {
		t.Fatalf("global 作用域下跨会话不该重问，Asker 被调用 %d 次（want 0）", got)
	}
}

// TestApprovalMemoryDoesNotBypassPolicy：记忆只跳过"问人"，不放宽围栏。
// 即使 store 里已有这条命令的 allow-always 规则，read-only 策略仍然拒绝执行。
func TestApprovalMemoryDoesNotBypassPolicy(t *testing.T) {
	dir := t.TempDir()
	policy := sandbox.New(sandbox.WithRoots(dir), sandbox.WithMode(sandbox.ModeReadOnly))
	store := kit.NewMemoryApprovals()
	err := store.Save(context.Background(), []rig.ApprovalRule{{
		Kind:      gate.ApprovalKindCommand,
		Key:       "touch pwned.txt",
		Scope:     rig.ApprovalScopeSession,
		SessionID: "s1",
		DecidedAt: time.Now(),
	}})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	agent, provider, asker := approvalAgent(t, kit.Config{
		Root:               dir,
		Policy:             policy,
		Tools:              builtin.NewWithPolicy(policy),
		InitialActiveTools: []string{"bash"},
		Approvals:          store,
	})
	asker.answer = "deny" // 一旦问人就说明记忆失效；答案本身也不该被采纳

	text, err := runToolRound(t, agent, provider, "s1", kit.ToolCall{
		ID: "c1", Name: "bash",
		Arguments: `{"command":"touch pwned.txt","description":"probe"}`,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := asker.count(); got != 0 {
		t.Fatalf("记忆命中时不该问人，Asker 被调用 %d 次", got)
	}
	if !strings.Contains(text, "read-only") {
		t.Fatalf("策略拒绝文案应说明 read-only，实际：%q", text)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "pwned.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("read-only 下文件被创建了（记忆放大了围栏）：stat err = %v", statErr)
	}
}

// countingAlwaysGate 是宿主自建门禁：每次调用都记一笔并返回 allow-always。
type countingAlwaysGate struct {
	mu    sync.Mutex
	calls int
}

func (g *countingAlwaysGate) Approve(ctx context.Context, tool rig.Tool, call rig.ToolCall) (rig.Decision, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	return rig.DecisionAllowAlways, nil
}

func (g *countingAlwaysGate) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

// TestApprovalMemoryDoesNotWrapCustomGate：显式 Config.Gate 时不自动挂记忆——
// 宿主门禁里可能叠了硬策略（CommandRiskGate 之类），记忆命中会绕过它。
func TestApprovalMemoryDoesNotWrapCustomGate(t *testing.T) {
	store := kit.NewMemoryApprovals()
	custom := &countingAlwaysGate{}
	agent, provider, _ := approvalAgent(t, kit.Config{Gate: custom, Approvals: store})

	for _, callID := range []string{"c1", "c2"} {
		if _, err := runToolRound(t, agent, provider, "s1", writeCall(callID, "a.txt", "v1")); err != nil {
			t.Fatalf("Run %s: %v", callID, err)
		}
	}
	if got := custom.count(); got != 2 {
		t.Fatalf("自定义门禁应被调用 2 次（不被记忆短路），实际 %d 次", got)
	}
	if rules := agent.SessionApprovals(); len(rules) != 0 {
		t.Fatalf("自定义 Gate 不该被 kit 自动挂记忆：%v", rules)
	}
	rules, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(rules) != 0 {
		t.Fatalf("store 不该被 kit 写入：%+v", rules)
	}
}

// TestApprovalMemoryDoesNotBypassNever：approval=never 是"拒绝一切"，记忆不能把
// 它悄悄变成"记住的放行"。
func TestApprovalMemoryDoesNotBypassNever(t *testing.T) {
	dir := t.TempDir()
	store := kit.NewMemoryApprovals()
	if err := store.Save(context.Background(), []rig.ApprovalRule{
		approvalRule(gate.ApprovalKindPath, "a.txt", rig.ApprovalScopeSession, "s1"),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	agent, provider, asker := approvalAgent(t, kit.Config{
		Root:      dir,
		Approval:  kit.ApprovalNever,
		Approvals: store,
	})

	text, err := runToolRound(t, agent, provider, "s1", writeCall("c1", "a.txt", "v1"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := asker.count(); got != 0 {
		t.Fatalf("never 档不该问人，Asker 被调用 %d 次", got)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "a.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("never 档下文件被创建了（记忆绕过了 never）：stat err = %v，工具结果：%q", statErr, text)
	}
	if agent.SessionApprovals() != nil {
		t.Fatalf("never 档下不该挂审批记忆：%v", agent.SessionApprovals())
	}
}

// TestParallelToolCallsAskOncePerRound：同一轮里两个"同工具同参数"的并行调用
// 只问一次；memo 只在单次 Run 内有效，下一次 Run 仍按记忆规则判断。
func TestParallelToolCallsAskOncePerRound(t *testing.T) {
	dir := t.TempDir()
	agent, provider, asker := approvalAgent(t, kit.Config{Root: dir})

	text, err := runToolRound(t, agent, provider, "s1",
		writeCall("c1", "a.txt", "v1"),
		writeCall("c2", "a.txt", "v1"),
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := asker.count(); got != 1 {
		t.Fatalf("同一轮两个同参数调用只该问 1 次，Asker 被调用 %d 次（工具结果：%q）", got, text)
	}
	if content, readErr := os.ReadFile(filepath.Join(dir, "a.txt")); readErr != nil || string(content) != "v1" {
		t.Fatalf("两次调用都应该真的执行了，读回 = %q, err = %v", content, readErr)
	}

	// 换一次 Run（答案只是 allow，没有写进记忆）：按记忆规则判断 → 重新问人。
	if _, err := runToolRound(t, agent, provider, "s1", writeCall("c3", "a.txt", "v1")); err != nil {
		t.Fatalf("第二次 Run: %v", err)
	}
	if got := asker.count(); got != 2 {
		t.Fatalf("memo 只在单次 Run 内有效，第二次 Run 应该重问（累计 2 次），实际 %d 次", got)
	}
}

// TestApprovalPromptChainOrder：Config.ApprovalPromptChain 里的 prompt 按顺序
// 组合成模型可见的提问；Preflight 如实报告当前用的是哪条链。
func TestApprovalPromptChainOrder(t *testing.T) {
	agent, provider, asker := approvalAgent(t, kit.Config{
		ApprovalPromptChain: []gate.Prompt{
			func(rig.Tool, rig.ToolCall) string { return "FIRST-SEGMENT: diff preview" },
			func(rig.Tool, rig.ToolCall) string { return "" }, // 空段跳过
			func(rig.Tool, rig.ToolCall) string { return "SECOND-SEGMENT: confirm write" },
		},
	})
	if _, err := runToolRound(t, agent, provider, "s1", writeCall("c1", "a.txt", "v1")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	questions := asker.asked()
	if len(questions) != 1 {
		t.Fatalf("Asker 被调用 %d 次，want 1", len(questions))
	}
	question := questions[0]
	first := strings.Index(question, "FIRST-SEGMENT")
	second := strings.Index(question, "SECOND-SEGMENT")
	if first < 0 || second < 0 {
		t.Fatalf("提问里应包含两段 prompt，实际：%q", question)
	}
	if first > second {
		t.Fatalf("prompt 顺序不对（SECOND 出现在 FIRST 之前）：%q", question)
	}
	report := agent.Preflight()
	joined := strings.Join(report.Warnings, " ")
	if !strings.Contains(joined, "审批提示链") || !strings.Contains(joined, "3") {
		t.Fatalf("Preflight 应报告自定义 prompt 链（3 段），实际 Warnings：%v", report.Warnings)
	}
}

// TestApprovalMemoryGranularityBeatsToolWideAllowAlways：allow-always 只记"用户批准
// 的那一个目标"，不能让内层 AskGate 按工具名把同工具的其它参数、其它会话一起放行。
//
// 这是 B4 的中心承诺：store 里只有 path:a.txt，那么 b.txt 必须重问，换会话的 a.txt
// 也必须重问。
func TestApprovalMemoryGranularityBeatsToolWideAllowAlways(t *testing.T) {
	store := kit.NewMemoryApprovals()
	agent, provider, asker := approvalAgent(t, kit.Config{Approvals: store})
	asker.answer = "allow-always"

	if _, err := runToolRound(t, agent, provider, "s1", writeCall("c1", "a.txt", "v1")); err != nil {
		t.Fatalf("第一次 Run: %v", err)
	}
	if got := asker.count(); got != 1 {
		t.Fatalf("第一次 Run 应该问 1 次，实际 %d 次", got)
	}

	// 同会话、同工具、不同路径：批准的是 a.txt，不是 write_file。
	asker.answer = "allow"
	text, err := runToolRound(t, agent, provider, "s1", writeCall("c2", "b.txt", "v2"))
	if err != nil {
		t.Fatalf("第二次 Run（b.txt）: %v", err)
	}
	if got := asker.count(); got != 2 {
		t.Fatalf("批准 a.txt 后写 b.txt 仍必须问人（Asker 应为 2 次），实际 %d 次（工具结果：%q）", got, text)
	}

	// 换会话、同一个路径：session 作用域的规则不生效，必须重问。
	text, err = runToolRound(t, agent, provider, "s2", writeCall("c3", "a.txt", "v3"))
	if err != nil {
		t.Fatalf("第三次 Run（s2）: %v", err)
	}
	if got := asker.count(); got != 3 {
		t.Fatalf("换会话后写 a.txt 仍必须问人（Asker 应为 3 次），实际 %d 次（工具结果：%q）", got, text)
	}

	rules, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(rules) != 1 || rules[0].Kind != gate.ApprovalKindPath || rules[0].Key != "a.txt" {
		t.Fatalf("store 里应只有 path:a.txt 一条规则，实际 %+v", rules)
	}
}

// TestRunRequestGateOverridesApprovalMemory：本轮显式 RunRequest.Gate 是宿主的最终
// 决定，审批记忆不能把它包在里面（否则已记住的规则会穿透 PersonalNever 真的写盘）。
func TestRunRequestGateOverridesApprovalMemory(t *testing.T) {
	dir := t.TempDir()
	store := kit.NewMemoryApprovals()
	if err := store.Save(context.Background(), []rig.ApprovalRule{
		approvalRule(gate.ApprovalKindPath, "a.txt", rig.ApprovalScopeSession, "s1"),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	agent, provider, asker := approvalAgent(t, kit.Config{Root: dir, Approvals: store})
	provider.scripts = [][]kit.Chunk{
		toolCallScript(writeCall("c1", "a.txt", "v1")),
		textScript("done"),
	}

	result, err := agent.Run(context.Background(), kit.RunRequest{
		SessionID: "s1",
		Gate:      gate.PersonalNever(),
		Input:     []kit.Message{kit.NewMessage(kit.RoleUser, "run it")},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := asker.count(); got != 0 {
		t.Fatalf("PersonalNever 是终局拒绝，不该问人（Asker %d 次）", got)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "a.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("本轮显式 PersonalNever 被审批记忆穿透：文件被创建（stat err = %v）", statErr)
	}
	texts := make([]string, 0, len(result.Messages))
	for _, message := range result.Messages {
		if message.Role == kit.RoleTool {
			texts = append(texts, message.Text())
		}
	}
	if joined := strings.Join(texts, "\n"); !strings.Contains(joined, "approval=never") {
		t.Fatalf("工具结果应说明 approval=never，实际：%q", joined)
	}
}

// TestApprovalMemoryEmptySessionDoesNotShareBucket：SessionID 为空时 session 作用域
// 会退化成"所有空会话共用一个桶"，等于把 session 语义悄悄变成 global —— 不写。
func TestApprovalMemoryEmptySessionDoesNotShareBucket(t *testing.T) {
	store := kit.NewMemoryApprovals()
	agent, provider, asker := approvalAgent(t, kit.Config{Approvals: store})
	asker.answer = "allow-always"

	if _, err := runToolRound(t, agent, provider, "", writeCall("c1", "a.txt", "v1")); err != nil {
		t.Fatalf("空 SessionID 的 Run: %v", err)
	}
	rules, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(rules) != 0 {
		t.Fatalf("SessionID 为空时不该写 session 规则（否则退化成共享桶）：%+v", rules)
	}

	// 另一个同样没有 SessionID 的临时会话：不能吃上一条的 allow-always。
	asker.answer = "allow"
	if _, err := runToolRound(t, agent, provider, "", writeCall("c2", "a.txt", "v1")); err != nil {
		t.Fatalf("第二个空 SessionID 的 Run: %v", err)
	}
	if got := asker.count(); got != 2 {
		t.Fatalf("空 SessionID 之间不共享记忆：第二次仍要问人（Asker 2 次），实际 %d 次", got)
	}
}

// barrierAsker 让 n 个并发 Run 都问过人之后才一起回答：把"两个 Run 都读过空的
// 规则快照、再各自整体保存"这个交错固定下来（否则测试会飘）。
type barrierAsker struct {
	n     int
	mu    sync.Mutex
	count int
	ready chan struct{}
}

func newBarrierAsker(n int) *barrierAsker {
	return &barrierAsker{n: n, ready: make(chan struct{})}
}

func (a *barrierAsker) Ask(ctx context.Context, _ string, _ []string) (string, error) {
	a.mu.Lock()
	a.count++
	if a.count == a.n {
		close(a.ready)
	}
	ready := a.ready
	a.mu.Unlock()
	select {
	case <-ready:
		return "allow-always", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// TestConcurrentRunsMergeApprovalRules：两个 Run 并发各记一条不同目标的规则时，
// Save 必须合并写（先 Load 再并集），不能用本次 Run 的旧快照整体覆盖。
func TestConcurrentRunsMergeApprovalRules(t *testing.T) {
	dir := t.TempDir()
	store := kit.NewMemoryApprovals()
	asker := newBarrierAsker(2)

	newAgent := func(path string) (*kit.Agent, *scriptProvider) {
		provider := &scriptProvider{scripts: [][]kit.Chunk{
			toolCallScript(writeCall("c1", path, "v1")),
			textScript("done"),
		}}
		cfg := kit.Config{
			Model:     "m",
			Root:      dir,
			Asker:     asker,
			Approvals: store,
			Provider:  provider,
		}
		return mustNewKitAgent(t, cfg), provider
	}
	first, _ := newAgent("a.txt")
	second, _ := newAgent("b.txt")

	errs := make(chan error, 2)
	run := func(agent *kit.Agent, sessionID string) {
		_, err := agent.Run(context.Background(), kit.RunRequest{
			SessionID: sessionID,
			Input:     []kit.Message{kit.NewMessage(kit.RoleUser, "run it")},
		})
		errs <- err
	}
	go run(first, "s1")
	go run(second, "s2")
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("并发 Run: %v", err)
		}
	}

	rules, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("并发 Run 各记一条规则，store 应剩 2 条，实际 %d 条：%+v", len(rules), rules)
	}
}

// TestApprovalMemoryKeepsToolReviewerInTheLoop：auto 档的预审在最外层，审批记忆在
// 最内层——已记住的规则不能让 reviewer 整层被绕过（那是"记忆绕过硬策略"的翻版，
// 与 RunRequest.Gate 被穿透同一类问题）。
func TestApprovalMemoryKeepsToolReviewerInTheLoop(t *testing.T) {
	store := kit.NewMemoryApprovals()
	if err := store.Save(context.Background(), []rig.ApprovalRule{
		approvalRule(gate.ApprovalKindPath, "x", rig.ApprovalScopeSession, "s1"),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	tool := presetProbeTool()
	reviewer := &allowReviewer{}
	asker := &asleepAsker{answer: "allow"}
	cfg := kit.Config{
		Model:            "m",
		Root:             t.TempDir(),
		Tools:            []kit.Tool{tool},
		PermissionPreset: kit.PermissionPresetAuto,
		ToolReviewer:     reviewer,
		Asker:            asker,
		Approvals:        store,
	}
	if _, err := runOneTool(t, cfg, "write_file", `{"path":"x"}`); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(reviewer.requests) != 1 {
		t.Fatalf("auto 档 + 审批记忆时预审仍必须被调用（记忆只在最内层），reviewer 收到 %d 次", len(reviewer.requests))
	}
	if asker.asked != 0 {
		t.Fatalf("reviewer 放行后不该再问人，Asker 被调用 %d 次", asker.asked)
	}
	if probeExecutions(tool) != 1 {
		t.Fatalf("reviewer 放行后工具应执行 1 次，实际 %d 次", probeExecutions(tool))
	}
}
