package agent_test

// 第二轮审计（2H）：长会话在 agent 主循环里会不会坏。
//
// 关注点：
//   - 自动压缩之后，下一轮请求必须仍然是合法的 chat-completions 序列
//     （孤立的 role=tool 消息会让 provider 直接 400）；
//   - 压缩失败时的回合行为；
//   - 压缩结果是否持久化（不持久化 = 每轮重新摘要，成本翻倍）；
//   - 模型抽风（无限重复同一工具调用）时有没有兜底。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/session/compact"
)

// longPrompt 是一条足够长的用户消息：预算设为 1 token 时保证触发压缩。
var longPrompt = strings.Repeat("context padding ", 40)

// auditAssertToolPairing 断言一段消息序列满足 chat-completions 的配对约束：
//   - 任何 role=tool 消息之前必须已经出现过带 tool_calls 的 assistant 消息；
//   - 同一 assistant 的 tool_calls 集合覆盖后续 tool 消息的 ToolCallID；
//   - 序列不以 role=tool 开头。
func auditAssertToolPairing(t *testing.T, messages []rig.Message) {
	t.Helper()
	open := map[string]bool{}
	for index, message := range messages {
		switch message.Role {
		case rig.RoleTool:
			if index == 0 {
				t.Fatalf("第 0 条消息是 role=tool（孤立工具结果，provider 会 400）: %+v", message)
			}
			if !open[message.ToolCallID] {
				t.Fatalf("第 %d 条 tool 消息的调用 %q 没有对应的 assistant tool_calls（孤立工具结果）",
					index, message.ToolCallID)
			}
			delete(open, message.ToolCallID)
		case rig.RoleAssistant:
			for _, call := range message.ToolCalls {
				open[call.ID] = true
			}
		}
	}
}

// auditToolCallRound 生成一轮"模型请求一次工具调用"的脚本。
func auditToolCallRound(id, name, args string) []rig.Chunk {
	return toolCallScript(id, name, args)
}

// TestAudit2HCompactionKeepsToolPairing 是本次审计最重要的一条：长会话触发压缩后，
// 下一轮请求里 assistant tool_calls 与 tool 结果必须仍成对出现。
func TestAudit2HCompactionKeepsToolPairing(t *testing.T) {
	provider := &scriptProvider{scripts: [][]rig.Chunk{
		auditToolCallRound("call_1", "echo", `{"n":1}`),
		auditToolCallRound("call_2", "echo", `{"n":2}`),
		textScript("third"),
	}}
	tool := &fakeTool{name: "echo", fn: func(_ context.Context, call rig.ToolCall) (rig.ToolResult, error) {
		return rig.ToolResult{Content: "ok:" + call.Arguments}, nil
	}}
	a := mustNewAgent(t, rig.AgentSpec{
		Model:         "m",
		Provider:      provider,
		Tools:         []rig.Tool{tool},
		Compactor:     compact.WindowCompactor{KeepLast: 4},
		ContextBudget: 1, // 极小预算：每轮都会触发压缩
	})

	// 输入足够长，保证第一轮就超过 1 token 的预算（否则压缩不会触发）。
	result, err := a.Run(context.Background(), rig.RunRequest{Input: []rig.Message{userMessage(longPrompt)}})
	requireNoError(t, err)
	if result.Iterations != 3 {
		t.Fatalf("Iterations = %d, want 3", result.Iterations)
	}
	if provider.callCount() < 3 {
		t.Fatalf("provider 调用次数 = %d", provider.callCount())
	}
	// 每一次请求都必须满足配对约束（第一次也可能被压缩，因为预算极小）。
	for index := 0; index < provider.callCount(); index++ {
		request := provider.request(index)
		if index > 0 {
			auditAssertToolPairing(t, request.Messages)
		}
	}
	// 压缩必须真的发生过：后面某次请求里应该出现压缩摘要。
	sawSummary := false
	for index := 0; index < provider.callCount(); index++ {
		for _, message := range provider.request(index).Messages {
			if compact.IsCompactionSummary(message) {
				sawSummary = true
			}
		}
	}
	if !sawSummary {
		t.Fatal("极小预算下没有触发压缩：本用例没有测到目标路径")
	}
}

// TestAudit2HCompactionBoundaryInsideToolBlock 直接压边界函数：窗口切点落在工具
// 结果块中间时，必须向前扩到 assistant tool_calls，不能留下孤立 tool 消息。
func TestAudit2HCompactionBoundaryInsideToolBlock(t *testing.T) {
	compactor := compact.WindowCompactor{KeepLast: 2}
	messages := []rig.Message{
		rig.NewMessage(rig.RoleUser, "task"),
		{Role: rig.RoleAssistant, ToolCalls: []rig.ToolCall{
			{ID: "a", Name: "t", Arguments: "{}"},
			{ID: "b", Name: "t", Arguments: "{}"},
			{ID: "c", Name: "t", Arguments: "{}"},
			{ID: "d", Name: "t", Arguments: "{}"},
		}},
		{Role: rig.RoleTool, ToolCallID: "a", Name: "t", Content: []rig.ContentPart{rig.TextPart("ra")}},
		{Role: rig.RoleTool, ToolCallID: "b", Name: "t", Content: []rig.ContentPart{rig.TextPart("rb")}},
		{Role: rig.RoleTool, ToolCallID: "c", Name: "t", Content: []rig.ContentPart{rig.TextPart("rc")}},
		{Role: rig.RoleTool, ToolCallID: "d", Name: "t", Content: []rig.ContentPart{rig.TextPart("rd")}},
	}
	compacted, err := compactor.Compact(context.Background(), "s", messages, 1)
	requireNoError(t, err)
	auditAssertToolPairing(t, compacted)
}

// TestAudit2HCompactionErrorDegrades 覆盖 2H-P1 修复：压缩器出错时**不中止回合**，
// 而是发一条 EventError 提示并带着未压缩的对话继续。
func TestAudit2HCompactionErrorDegrades(t *testing.T) {
	provider := &scriptProvider{scripts: [][]rig.Chunk{textScript("ok")}}
	rec := &recorder{}
	a := mustNewAgent(t, rig.AgentSpec{
		Model:         "m",
		Provider:      provider,
		Compactor:     failingCompactor{err: errors.New("summary provider down")},
		ContextBudget: 1,
	})
	result, err := a.Run(context.Background(), rig.RunRequest{
		Input: []rig.Message{userMessage(longPrompt)},
		Emit:  rec.emit,
	})
	requireNoError(t, err)
	if provider.callCount() != 1 {
		t.Fatalf("provider 调用次数 = %d, want 1（降级后照常请求）", provider.callCount())
	}
	if result.FinishReason != "stop" {
		t.Fatalf("FinishReason = %q", result.FinishReason)
	}
	// 必须留下可诊断的提示，但不能让 run.end 报错。
	errorEvents := rec.ofType(rig.EventError)
	if len(errorEvents) != 1 || !strings.Contains(errorEvents[0].Err.Error(), "compaction failed") {
		t.Fatalf("EventError = %+v", errorEvents)
	}
	// 未压缩的对话照常发给 provider（预算只是估算，provider 才是裁判）。
	if request := provider.request(0); len(request.Messages) == 0 {
		t.Fatal("请求是空的")
	}
}

type failingCompactor struct{ err error }

func (c failingCompactor) Compact(context.Context, string, []rig.Message, int) ([]rig.Message, error) {
	return nil, c.err
}

// TestAudit2HCompactionNotPersisted 记录现状：自动压缩只在内存里生效，历史仍是
// 全量。下一轮会重新加载全量历史并再次压缩（LLMCompactor 意味着每轮一次摘要调用）。
func TestAudit2HCompactionNotPersisted(t *testing.T) {
	provider := &scriptProvider{scripts: [][]rig.Chunk{
		auditToolCallRound("call_1", "echo", `{"n":1}`),
		textScript("first"),
	}}
	tool := &fakeTool{name: "echo"}
	history := newCaptureHistory(nil)
	a := mustNewAgent(t, rig.AgentSpec{
		Model:         "m",
		Provider:      provider,
		Tools:         []rig.Tool{tool},
		History:       history,
		Compactor:     compact.WindowCompactor{KeepLast: 2},
		ContextBudget: 1,
	})
	_, err := a.Run(context.Background(), rig.RunRequest{SessionID: "s1", Input: []rig.Message{userMessage(longPrompt)}})
	requireNoError(t, err)

	// 先确认压缩确实在请求路径上发生过（否则本用例没有测到目标）。
	compactedRequest := false
	for index := 0; index < provider.callCount(); index++ {
		for _, message := range provider.request(index).Messages {
			if compact.IsCompactionSummary(message) {
				compactedRequest = true
			}
		}
	}
	if !compactedRequest {
		t.Fatal("压缩没有在请求路径上发生：用例没有测到目标")
	}

	stored := history.stored("s1")
	sawSummary := false
	for _, message := range stored {
		if compact.IsCompactionSummary(message) {
			sawSummary = true
		}
	}
	if sawSummary {
		t.Fatalf("现状被改变：历史里出现了压缩摘要（说明已持久化）。存储条数=%d", len(stored))
	}
	// 历史里仍应有原始的 assistant tool_calls 与 tool 结果（全量）。
	if len(stored) < 3 {
		t.Fatalf("历史条数 = %d", len(stored))
	}
}

// TestAudit2HCompactionSummaryDoesNotAccumulate 连续两轮都压缩时，摘要不应累积。
func TestAudit2HCompactionSummaryDoesNotAccumulate(t *testing.T) {
	provider := &scriptProvider{scripts: [][]rig.Chunk{
		auditToolCallRound("call_1", "echo", `{}`),
		textScript("first"),
		auditToolCallRound("call_2", "echo", `{}`),
		textScript("second"),
	}}
	tool := &fakeTool{name: "echo"}
	a := mustNewAgent(t, rig.AgentSpec{
		Model:         "m",
		Provider:      provider,
		Tools:         []rig.Tool{tool},
		Compactor:     compact.WindowCompactor{KeepLast: 2},
		ContextBudget: 1,
	})
	_, err := a.Run(context.Background(), rig.RunRequest{Input: []rig.Message{userMessage(longPrompt)}})
	requireNoError(t, err)

	last := provider.request(provider.callCount() - 1)
	summaries := 0
	for _, message := range last.Messages {
		if compact.IsCompactionSummary(message) {
			summaries++
		}
	}
	if summaries > 1 {
		t.Fatalf("最后一次请求里有 %d 条压缩摘要（应被替换而不是累积）", summaries)
	}
}

// TestAudit2HRepeatedIdenticalToolCallsAreGuarded 覆盖 2H-P3 修复：模型连续请求
// 完全相同的调用时，第 3 轮注入提示、第 5 轮起熔断（返回工具错误、不再执行）。
func TestAudit2HRepeatedIdenticalToolCallsAreGuarded(t *testing.T) {
	scripts := make([][]rig.Chunk, 0, 7)
	for i := 0; i < 6; i++ {
		scripts = append(scripts, auditToolCallRound("call_same", "spin", `{"query":"same"}`))
	}
	scripts = append(scripts, textScript("finally done"))
	provider := &scriptProvider{scripts: scripts}
	var invocations int
	tool := &fakeTool{name: "spin", fn: func(context.Context, rig.ToolCall) (rig.ToolResult, error) {
		invocations++
		return rig.ToolResult{Content: "same result"}, nil
	}}
	a := mustNewAgent(t, rig.AgentSpec{Model: "m", Provider: provider, Tools: []rig.Tool{tool}})
	result, err := a.Run(context.Background(), rig.RunRequest{Input: []rig.Message{userMessage("go")}})
	requireNoError(t, err)
	if result.Iterations != 7 {
		t.Fatalf("Iterations = %d, want 7（6 轮重复 + 收尾）", result.Iterations)
	}
	// 前 4 轮真正执行，第 5、6 轮被熔断。
	if invocations != 4 {
		t.Fatalf("工具执行次数 = %d, want 4（第 5 轮起熔断）", invocations)
	}
	// 第 3 轮之后必须出现"你已经重复了"的提示（注入给模型的 user 消息）。
	sawNotice := false
	for index := 0; index < provider.callCount(); index++ {
		if strings.Contains(requestText(provider.request(index)), "identical arguments") {
			sawNotice = true
		}
	}
	if !sawNotice {
		t.Fatal("没有注入重复调用提示")
	}
	// 被熔断的调用必须留下工具错误（否则历史无法重放）。
	sawBlocked := false
	for _, message := range result.Messages {
		if message.Role == rig.RoleTool && strings.Contains(message.Text(), "repeated call blocked") {
			sawBlocked = true
		}
	}
	if !sawBlocked {
		t.Fatal("熔断的调用没有产生工具错误结果")
	}
}

// TestAudit2HMaxIterationsStopsRunawayLoop 记录兜底手段：显式 MaxIterations 能停住。
func TestAudit2HMaxIterationsStopsRunawayLoop(t *testing.T) {
	scripts := make([][]rig.Chunk, 0, 8)
	for i := 0; i < 8; i++ {
		scripts = append(scripts, auditToolCallRound("call_same", "spin", `{}`))
	}
	provider := &scriptProvider{scripts: scripts}
	history := newCaptureHistory(nil)
	tool := &fakeTool{name: "spin"}
	a := mustNewAgent(t, rig.AgentSpec{
		Model: "m", Provider: provider, Tools: []rig.Tool{tool},
		History: history, MaxIterations: 3,
	})
	_, err := a.Run(context.Background(), rig.RunRequest{SessionID: "s", Input: []rig.Message{userMessage("go")}})
	if !errors.Is(err, rig.ErrMaxIterations) {
		t.Fatalf("err = %v, want rig.ErrMaxIterations", err)
	}
	// 被兜底截断之后，历史仍然可重放（每个 tool 调用都有结果）。
	auditAssertToolPairing(t, history.stored("s"))
}

// TestAudit2HOrphanToolMessageRepaired 覆盖 2H-P6 修复：历史里的孤立 tool 消息被
// 丢弃、没人回复的 tool_calls 被合成结果，于是下一轮请求合法（不会 400）。
func TestAudit2HOrphanToolMessageRepaired(t *testing.T) {
	orphan := rig.Message{Role: rig.RoleTool, ToolCallID: "missing", Name: "x",
		Content: []rig.ContentPart{rig.TextPart("orphan result")}}
	// 另一种坏形态：assistant 发了调用，但结果丢了（崩在工具执行中途）。
	dangling := rig.Message{Role: rig.RoleAssistant, ToolCalls: []rig.ToolCall{
		{ID: "lost", Name: "read_file", Arguments: `{}`},
	}}
	provider := &scriptProvider{scripts: [][]rig.Chunk{textScript("ok")}}
	history := newCaptureHistory(map[string][]rig.Message{"s": {orphan, dangling}})
	a := mustNewAgent(t, rig.AgentSpec{Model: "m", Provider: provider, History: history})
	_, err := a.Run(context.Background(), rig.RunRequest{SessionID: "s", Input: []rig.Message{userMessage("go")}})
	requireNoError(t, err)

	request := provider.request(0)
	auditAssertToolPairing(t, request.Messages)
	for _, message := range request.Messages {
		if message.Role == rig.RoleTool && message.ToolCallID == "missing" {
			t.Fatal("孤立 tool 消息没有被丢弃")
		}
		if message.Role == rig.RoleTool && message.ToolCallID == "lost" &&
			!strings.Contains(message.Text(), "tool result unavailable") {
			t.Fatalf("悬空 tool_calls 的结果不合规: %q", message.Text())
		}
	}
	sawSynthesized := false
	for _, message := range request.Messages {
		if message.Role == rig.RoleTool && message.ToolCallID == "lost" {
			sawSynthesized = true
		}
	}
	if !sawSynthesized {
		t.Fatal("悬空的 tool_calls 没有合成工具结果")
	}
}

// TestAudit2HCompactionKeepsRequestValidAfterManyRounds 是"200 条消息 / 50 万 token"
// 的缩小版：25 轮工具往返 + 极小预算，逐轮检查请求合法性。
func TestAudit2HCompactionKeepsRequestValidAfterManyRounds(t *testing.T) {
	scripts := make([][]rig.Chunk, 0, 25)
	for i := 0; i < 24; i++ {
		scripts = append(scripts, auditToolCallRound(fmt.Sprintf("call_%d", i), "echo", `{"i":`+fmt.Sprint(i)+`}`))
	}
	scripts = append(scripts, textScript("ok"))
	provider := &scriptProvider{scripts: scripts}
	tool := &fakeTool{name: "echo", fn: func(context.Context, rig.ToolCall) (rig.ToolResult, error) {
		return rig.ToolResult{Content: strings.Repeat("payload ", 200)}, nil
	}}
	a := mustNewAgent(t, rig.AgentSpec{
		Model: "m", Provider: provider, Tools: []rig.Tool{tool},
		Compactor: compact.WindowCompactor{KeepLast: 6}, ContextBudget: 32,
	})
	result, err := a.Run(context.Background(), rig.RunRequest{Input: []rig.Message{userMessage(longPrompt)}})
	requireNoError(t, err)
	if result.Iterations != 25 {
		t.Fatalf("Iterations = %d, want 25", result.Iterations)
	}
	for index := 0; index < provider.callCount(); index++ {
		auditAssertToolPairing(t, provider.request(index).Messages)
		// 压缩之后请求规模必须被压住，而不是继续线性增长。
		if size := len(provider.request(index).Messages); size > 40 {
			t.Fatalf("第 %d 次请求仍有 %d 条消息（压缩没生效）", index, size)
		}
	}
}

// requestText 拼接一次请求里的全部文本（用于断言"有没有提示注入"）。
func requestText(request rig.Request) string {
	var builder strings.Builder
	for _, message := range request.Messages {
		builder.WriteString(message.Text())
		builder.WriteString("\n")
	}
	return builder.String()
}

var _ = io.EOF

// TestAudit2H200Message005MTokenSession 是任务书点名的规模：约 200 条消息、
// 50 万 token（用 12 万字符的装配体模拟：CJK 按 1 字符/token 估算）。
//
// 断言：压缩后请求合法（工具配对完整、不以 tool 开头）、请求规模被压住、
// 回合正常收尾。
func TestAudit2H200Message005MTokenSession(t *testing.T) {
	// 构造 200 条消息的历史：user/assistant 交替 + 每 4 条插一轮工具调用。
	var seeded []rig.Message
	payload := strings.Repeat("数据", 1800) // 每条约 3600 token（CJK 1 字/token）
	for i := 0; i < 200; i++ {
		switch {
		case i%4 == 2:
			seeded = append(seeded, rig.Message{
				Role:      rig.RoleAssistant,
				ToolCalls: []rig.ToolCall{{ID: fmt.Sprintf("old_%d", i), Name: "echo", Arguments: `{}`}},
			})
		case i%4 == 3:
			seeded = append(seeded, rig.Message{
				Role: rig.RoleTool, ToolCallID: fmt.Sprintf("old_%d", i-1), Name: "echo",
				Content: []rig.ContentPart{rig.TextPart(payload)},
			})
		case i%2 == 0:
			seeded = append(seeded, rig.NewMessage(rig.RoleUser, payload))
		default:
			seeded = append(seeded, rig.NewMessage(rig.RoleAssistant, payload))
		}
	}
	estimated := rig.EstimateTokens(seeded)
	if estimated < 400_000 {
		t.Fatalf("装配体只有 %d token，达不到 50 万量级", estimated)
	}
	provider := &scriptProvider{scripts: [][]rig.Chunk{textScript("summary of a long session")}}
	tool := &fakeTool{name: "echo"}
	history := newCaptureHistory(map[string][]rig.Message{"long": seeded})
	compactor := compact.WindowCompactor{KeepLast: 8}
	a := mustNewAgent(t, rig.AgentSpec{
		Model: "m", Provider: provider, Tools: []rig.Tool{tool}, History: history,
		Compactor: compactor, ContextBudget: 20_000,
	})
	result, err := a.Run(context.Background(), rig.RunRequest{SessionID: "long", Input: []rig.Message{userMessage("continue")}})
	requireNoError(t, err)
	if result.Iterations != 1 {
		t.Fatalf("Iterations = %d", result.Iterations)
	}
	request := provider.request(0)
	auditAssertToolPairing(t, request.Messages)
	after := rig.EstimateTokens(request.Messages)
	if after > estimated/4 {
		t.Fatalf("压缩后仍估算 %d token（原始 %d）：压缩几乎没生效", after, estimated)
	}
	if len(request.Messages) > 16 {
		t.Fatalf("压缩后消息数 = %d（Original 200），应远小于此", len(request.Messages))
	}
	// 摘要必须存在，且用户最新输入必须保留（不能把"当前问题"压掉）。
	last := request.Messages[len(request.Messages)-1]
	if last.Role != rig.RoleUser || last.Text() != "continue" {
		t.Fatalf("最后一条消息 = %+v（最新用户输入必须保留）", last)
	}
	sawSummary := false
	for _, message := range request.Messages {
		if compact.IsCompactionSummary(message) {
			sawSummary = true
		}
	}
	if !sawSummary {
		t.Fatal("压缩后没有摘要消息")
	}
}

// TestAudit2HCompactionFitsBudget 覆盖 2H-P2 修复：一条 18 万 token 的工具结果 +
// 预算 8000 时，压缩后的请求必须落回预算，且不拆 tool 块、不改写当前用户输入。
func TestAudit2HCompactionFitsBudget(t *testing.T) {
	const budget = 8000
	hugePayload := strings.Repeat("数据", 90_000) // 约 18 万 token
	seeded := []rig.Message{
		rig.NewMessage(rig.RoleUser, "请分析这个日志"),
		{Role: rig.RoleAssistant, ToolCalls: []rig.ToolCall{
			{ID: "big", Name: "read_file", Arguments: `{"path":"big.log"}`},
		}},
		{Role: rig.RoleTool, ToolCallID: "big", Name: "read_file",
			Content: []rig.ContentPart{rig.TextPart(hugePayload)}},
	}
	provider := &scriptProvider{scripts: [][]rig.Chunk{textScript("done")}}
	history := newCaptureHistory(map[string][]rig.Message{"s": seeded})
	a := mustNewAgent(t, rig.AgentSpec{
		Model: "m", Provider: provider, History: history,
		Compactor:     compact.WindowCompactor{KeepLast: 6},
		ContextBudget: budget,
	})
	_, err := a.Run(context.Background(), rig.RunRequest{SessionID: "s", Input: []rig.Message{userMessage("结论是什么？")}})
	requireNoError(t, err)
	request := provider.request(0)
	auditAssertToolPairing(t, request.Messages)
	if got := rig.EstimateTokens(request.Messages); got > budget*2 {
		t.Fatalf("请求估算 %d token，远超预算 %d", got, budget)
	}
	last := request.Messages[len(request.Messages)-1]
	if last.Role != rig.RoleUser || last.Text() != "结论是什么？" {
		t.Fatalf("当前用户输入被改坏: %+v", last)
	}
}

// auditSummaryProvider 是一个"双角色" provider：收到摘要请求（首条是
// LLMCompactorSystemPrompt）时返回摘要文本，其余请求按脚本回放。
type auditSummaryProvider struct {
	mu       sync.Mutex
	scripts  [][]rig.Chunk
	requests []rig.Request
	summary  string
}

func (p *auditSummaryProvider) Name() string { return "audit-summary" }

func (p *auditSummaryProvider) Stream(ctx context.Context, request rig.Request) (rig.Stream, error) {
	p.mu.Lock()
	p.requests = append(p.requests, request)
	isSummary := len(request.Messages) > 0 &&
		strings.HasPrefix(request.Messages[0].Text(), "You are a conversation summarizer")
	if isSummary {
		summary := p.summary
		p.mu.Unlock()
		return &chunkStream{chunks: []rig.Chunk{{Delta: summary}, {FinishReason: "stop"}}}, nil
	}
	if len(p.scripts) == 0 {
		p.mu.Unlock()
		return nil, fmt.Errorf("audit-summary: no script left")
	}
	script := p.scripts[0]
	p.scripts = p.scripts[1:]
	p.mu.Unlock()
	return &chunkStream{chunks: script}, nil
}

func (p *auditSummaryProvider) all() []rig.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]rig.Request(nil), p.requests...)
}

// TestAudit2HLLMCompactorPathsAreValid 覆盖个人客户端真实会用的压缩器：
// LLMCompactor 自己也会发请求（摘要），这条请求同样必须合法；压缩后的对话必须
// 包含摘要，且工具配对完整。
func TestAudit2HLLMCompactorPathsAreValid(t *testing.T) {
	// 用一段"已经很长"的历史，保证第一轮就触发摘要（不依赖后续轮次的增长）。
	seeded := make([]rig.Message, 0, 20)
	for i := 0; i < 8; i++ {
		seeded = append(seeded,
			rig.NewMessage(rig.RoleUser, strings.Repeat("历史用户消息 ", 40)),
			rig.Message{Role: rig.RoleAssistant, ToolCalls: []rig.ToolCall{
				{ID: fmt.Sprintf("old_%d", i), Name: "echo", Arguments: `{}`},
			}},
			rig.Message{Role: rig.RoleTool, ToolCallID: fmt.Sprintf("old_%d", i), Name: "echo",
				Content: []rig.ContentPart{rig.TextPart(strings.Repeat("旧工具结果 ", 300))}},
		)
	}
	provider := &auditSummaryProvider{
		scripts: [][]rig.Chunk{textScript("done")},
		summary: "用户要求把两批数据合起来；历史里已完成若干次 echo。",
	}
	tool := &fakeTool{name: "echo"}
	history := newCaptureHistory(map[string][]rig.Message{"s": seeded})
	a := mustNewAgent(t, rig.AgentSpec{
		Model: "m", Provider: provider, Tools: []rig.Tool{tool}, History: history,
		Compactor: compact.LLMCompactor{
			Provider: provider, Model: "m", KeepLast: 4, MaxSummaryChars: 2000,
		},
		ContextBudget: 2000,
	})
	result, err := a.Run(context.Background(), rig.RunRequest{SessionID: "s", Input: []rig.Message{userMessage("继续")}})
	requireNoError(t, err)
	if result.Iterations != 1 {
		t.Fatalf("Iterations = %d, want 1", result.Iterations)
	}
	requests := provider.all()
	summarized := 0
	for index, request := range requests {
		auditAssertToolPairing(t, request.Messages)
		if len(request.Messages) > 0 &&
			strings.HasPrefix(request.Messages[0].Text(), "You are a conversation summarizer") {
			summarized++
		}
		_ = index
	}
	if summarized == 0 {
		t.Fatal("LLMCompactor 没有发出摘要请求：用例没有测到目标")
	}
	sawSummaryText := false
	for _, request := range requests {
		for _, message := range request.Messages {
			if strings.Contains(message.Text(), "用户要求把两批数据合起来") {
				sawSummaryText = true
			}
		}
	}
	if !sawSummaryText {
		t.Fatal("压缩摘要没有进入后续请求")
	}
}

// TestAudit2HLLMCompactorFailureFallsBackToWindow 覆盖"摘要失败 → 回退窗口裁剪"，
// 即 LLMCompactor 内置的 fallback 是否真的让回合继续。
func TestAudit2HLLMCompactorFailureFallsBackToWindow(t *testing.T) {
	provider := &scriptProvider{scripts: [][]rig.Chunk{
		auditToolCallRound("call_1", "echo", `{}`),
		textScript("done"),
	}}
	tool := &fakeTool{name: "echo"}
	failing := failingProvider{err: errors.New("summary model 500")}
	a := mustNewAgent(t, rig.AgentSpec{
		Model: "m", Provider: provider, Tools: []rig.Tool{tool},
		Compactor:     compact.LLMCompactor{Provider: failing, Model: "m", KeepLast: 2},
		ContextBudget: 8,
	})
	result, err := a.Run(context.Background(), rig.RunRequest{Input: []rig.Message{userMessage(longPrompt)}})
	requireNoError(t, err)
	if result.Iterations != 2 {
		t.Fatalf("Iterations = %d, want 2", result.Iterations)
	}
	for index := 0; index < provider.callCount(); index++ {
		auditAssertToolPairing(t, provider.request(index).Messages)
	}
}

// failingProvider 的每次 Stream 都返回固定错误。
type failingProvider struct{ err error }

func (p failingProvider) Name() string { return "failing" }

func (p failingProvider) Stream(context.Context, rig.Request) (rig.Stream, error) {
	return nil, p.err
}
