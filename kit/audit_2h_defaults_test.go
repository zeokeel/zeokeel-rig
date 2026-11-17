package kit_test

// 第二轮审计 2H 的默认值修复用例：kit 的"安全网"必须默认打开。
//
//	L1 预算自动推导（模型上下文 × 0.6，下限 16k）
//	L2 默认 WindowCompactor（KeepLast 8 + 工具结果保留 4）
//	G3 轮数护栏默认 200
//	花费护栏 MaxRunCostUSD
//	压缩可见性：事件流里出现 "compacted N message(s)"

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/costmeter"
	"github.com/zeokeel/zeokeel-rig/events"
	"github.com/zeokeel/zeokeel-rig/kit"
	"github.com/zeokeel/zeokeel-rig/session"
)

// TestAudit2HDefaultContextBudgetIsDerived 验证 L1：零配置下预算自动推导，
// 并且长会话真的被压缩（事件流里有 compacted 提示、请求比历史小得多）。
func TestAudit2HDefaultContextBudgetIsDerived(t *testing.T) {
	// 100 条 × 约 2k token 的历史 ≈ 200k token，远超 128k×0.6=76.8k 的默认预算。
	payload := strings.Repeat("历史对话内容 ", 300)
	seeded := make([]rig.Message, 0, 100)
	for i := 0; i < 100; i++ {
		role := rig.RoleUser
		if i%2 == 1 {
			role = rig.RoleAssistant
		}
		seeded = append(seeded, rig.NewMessage(role, payload))
	}
	provider := &scriptProvider{scripts: [][]kit.Chunk{textScript("ok")}}
	history := session.NewMemory()
	if err := history.Replace(context.Background(), "s", seeded); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	var buffer bytes.Buffer
	agent, err := kit.New(kit.Config{
		Model: "m", Provider: provider, Root: t.TempDir(),
		Tools: []kit.Tool{}, Approval: kit.ApprovalAuto,
		History: history, EventWriter: &buffer,
	})
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	if _, err := agent.Run(context.Background(), kit.RunRequest{
		SessionID: "s", Input: []kit.Message{kit.NewMessage(kit.RoleUser, "继续")},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	request := provider.request(0)
	seededTokens := rig.EstimateTokens(seeded)
	after := rig.EstimateTokens(request.Messages)
	if after >= seededTokens/2 {
		t.Fatalf("零配置下没有压缩：%d -> %d token（默认预算应约为 %d）",
			seededTokens, after, kit.DefaultModelContextTokens*6/10)
	}
	if !strings.Contains(buffer.String(), "compacted") {
		t.Fatalf("事件流里没有压缩提示:\n%s", buffer.String())
	}
}

// TestAudit2HExplicitContextBudgetWins：显式预算优先于自动推导。
func TestAudit2HExplicitContextBudgetWins(t *testing.T) {
	seeded := make([]rig.Message, 0, 40)
	for i := 0; i < 40; i++ {
		seeded = append(seeded, rig.NewMessage(rig.RoleUser, strings.Repeat("内容 ", 200)))
	}
	provider := &scriptProvider{scripts: [][]kit.Chunk{textScript("ok")}}
	history := session.NewMemory()
	if err := history.Replace(context.Background(), "s", seeded); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	agent, err := kit.New(kit.Config{
		Model: "m", Provider: provider, Root: t.TempDir(),
		Tools: []kit.Tool{}, Approval: kit.ApprovalAuto,
		History: history, ContextBudget: 500,
	})
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	if _, err := agent.Run(context.Background(), kit.RunRequest{
		SessionID: "s", Input: []kit.Message{kit.NewMessage(kit.RoleUser, "继续")},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := rig.EstimateTokens(provider.request(0).Messages); got > 1500 {
		t.Fatalf("显式预算 500 没有生效：请求 %d token", got)
	}
}

// TestAudit2HDefaultIterationCap 验证 G3：零配置下 200 轮是硬上限。
func TestAudit2HDefaultIterationCap(t *testing.T) {
	const rounds = kit.DefaultSafeMaxIterations + 5
	scripts := make([][]kit.Chunk, 0, rounds)
	for i := 0; i < rounds; i++ {
		// 每轮参数不同：避免触发重复调用守卫，这里要测的是轮数总闸。
		scripts = append(scripts, toolCallScript(kit.ToolCall{
			ID: fmt.Sprintf("call_%d", i), Name: "tick", Arguments: fmt.Sprintf(`{"i":%d}`, i),
		}))
	}
	provider := &scriptProvider{scripts: scripts}
	agent, err := kit.New(kit.Config{
		Model: "m", Provider: provider, Root: t.TempDir(),
		Tools: []kit.Tool{&fakeTool{name: "tick"}}, Approval: kit.ApprovalAuto,
	})
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	result, runErr := agent.Run(context.Background(), kit.RunRequest{
		SessionID: "s", Input: []kit.Message{kit.NewMessage(kit.RoleUser, "loop forever")},
	})
	if !errors.Is(runErr, rig.ErrMaxIterations) {
		t.Fatalf("err = %v, want rig.ErrMaxIterations", runErr)
	}
	if result.Iterations > kit.DefaultSafeMaxIterations+1 {
		t.Fatalf("Iterations = %d，超过默认上限 %d", result.Iterations, kit.DefaultSafeMaxIterations)
	}
}

// TestAudit2HNegativeMaxIterationsMeansUnlimited：显式负数 = 真的不限（老语义保留）。
func TestAudit2HNegativeMaxIterationsMeansUnlimited(t *testing.T) {
	scripts := make([][]kit.Chunk, 0, 30)
	for i := 0; i < 30; i++ {
		scripts = append(scripts, toolCallScript(kit.ToolCall{
			ID: fmt.Sprintf("call_%d", i), Name: "tick", Arguments: fmt.Sprintf(`{"i":%d}`, i),
		}))
	}
	scripts = append(scripts, textScript("done"))
	provider := &scriptProvider{scripts: scripts}
	agent, err := kit.New(kit.Config{
		Model: "m", Provider: provider, Root: t.TempDir(),
		Tools: []kit.Tool{&fakeTool{name: "tick"}}, Approval: kit.ApprovalAuto,
		MaxIterations: -1,
	})
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	result, runErr := agent.Run(context.Background(), kit.RunRequest{
		SessionID: "s", Input: []kit.Message{kit.NewMessage(kit.RoleUser, "go")},
	})
	if runErr != nil {
		t.Fatalf("Run: %v", runErr)
	}
	if result.Iterations != 31 {
		t.Fatalf("Iterations = %d, want 31", result.Iterations)
	}
}

// TestAudit2HRunCostLimitStopsRun 验证花费护栏：超限以可读错误收尾。
func TestAudit2HRunCostLimitStopsRun(t *testing.T) {
	// 每轮消耗 10M prompt + 10M completion token：deepseek-flash 定价下约 $15。
	scripts := [][]kit.Chunk{
		append(toolCallScript(kit.ToolCall{ID: "a", Name: "tick", Arguments: `{"i":1}`}),
			kit.Chunk{Usage: &kit.Usage{PromptTokens: 10_000_000, CompletionTokens: 10_000_000}}),
		append(toolCallScript(kit.ToolCall{ID: "b", Name: "tick", Arguments: `{"i":2}`}),
			kit.Chunk{Usage: &kit.Usage{PromptTokens: 10_000_000, CompletionTokens: 10_000_000}}),
		append(toolCallScript(kit.ToolCall{ID: "c", Name: "tick", Arguments: `{"i":3}`}),
			kit.Chunk{Usage: &kit.Usage{PromptTokens: 10_000_000, CompletionTokens: 10_000_000}}),
		textScript("done"),
	}
	provider := &scriptProvider{scripts: scripts}
	meter := costmeter.New()
	agent, err := kit.New(kit.Config{
		Model: "deepseek-flash", Provider: provider, Root: t.TempDir(),
		Tools: []kit.Tool{&fakeTool{name: "tick"}}, Approval: kit.ApprovalAuto,
		Meter: meter, MaxRunCostUSD: 1,
	})
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	_, runErr := agent.Run(context.Background(), kit.RunRequest{
		SessionID: "s", Input: []kit.Message{kit.NewMessage(kit.RoleUser, "go")},
	})
	if runErr == nil {
		t.Fatal("超过花费上限必须返回错误")
	}
	if !strings.Contains(runErr.Error(), "超过上限") {
		t.Fatalf("错误文案应说明超限: %v", runErr)
	}
	if spent := meter.Session().CostUSD; spent < 1 {
		t.Fatalf("meter 花费 = $%.4f，用例前提（超限）不成立", spent)
	}
}

// TestAudit2HRunEndPayloadFields 验证事件流的 run.end 三个数字。
func TestAudit2HRunEndPayloadFields(t *testing.T) {
	provider := &scriptProvider{scripts: [][]kit.Chunk{
		toolCallScript(kit.ToolCall{ID: "c1", Name: "echoer", Arguments: `{}`}),
		textScript("done"),
	}}
	var buffer bytes.Buffer
	agent, err := kit.New(kit.Config{
		Model: "m", Provider: provider, Root: t.TempDir(),
		Tools: []kit.Tool{&wiringEchoTool{name: "echoer"}}, Approval: kit.ApprovalAuto,
		EventWriter: &buffer,
	})
	if err != nil {
		t.Fatalf("kit.New: %v", err)
	}
	result, err := agent.Run(context.Background(), kit.RunRequest{
		SessionID: "s", Input: []kit.Message{kit.NewMessage(kit.RoleUser, "go")},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var runEnd events.RunEnd
	found := false
	decoder := events.NewDecoder(bytes.NewReader(buffer.Bytes()))
	for {
		envelope, err := decoder.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if envelope.Kind != events.KindRunEnd {
			continue
		}
		found = true
		if err := decodeInto(envelope, &runEnd); err != nil {
			t.Fatalf("decode run.end: %v", err)
		}
	}
	if !found {
		t.Fatal("事件流里没有 run.end")
	}
	if runEnd.Turns != result.Iterations {
		t.Fatalf("Turns = %d, want %d", runEnd.Turns, result.Iterations)
	}
	if runEnd.Messages != len(result.Messages) {
		t.Fatalf("Messages = %d, want %d", runEnd.Messages, len(result.Messages))
	}
	if runEnd.ToolCalls != 1 {
		t.Fatalf("ToolCalls = %d, want 1", runEnd.ToolCalls)
	}
}

// decodeInto 把事件 payload 解到 target。
func decodeInto(envelope events.Envelope, target any) error {
	return jsonUnmarshal(envelope.Payload, target)
}

var _ = fmt.Sprintf
var _ = io.EOF
