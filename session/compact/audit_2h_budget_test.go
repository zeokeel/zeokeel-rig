package compact

// 第二轮审计 2H 的 P2 修复用例："压缩必须真的进预算"。
//
// 审计发现 `Compact(..., budget)` 的 budget 参数被完全忽略：预算 8000、窗口里
// 有一条 18 万 token 的工具结果时，压完仍然 18 万 token，provider 照样 400。
// 修复后必须满足：① 估算 token 落回预算；② **绝不拆 tool 块**（孤立 tool 消息
// 会让 provider 直接 400）；③ 当前用户输入不被改写。

import (
	"context"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
)

// auditHugeToolResult 构造"一条巨大的工具结果"场景。
func auditHugeToolResult() []rig.Message {
	huge := strings.Repeat("数据", 90_000) // 约 18 万 token（CJK 1 字/token）
	return []rig.Message{
		rig.NewMessage(rig.RoleSystem, "sys"),
		rig.NewMessage(rig.RoleUser, "请分析这个文件"),
		{Role: rig.RoleAssistant, ToolCalls: []rig.ToolCall{
			{ID: "call_big", Name: "read_file", Arguments: `{"path":"big.log"}`},
		}},
		{Role: rig.RoleTool, ToolCallID: "call_big", Name: "read_file",
			Content: []rig.ContentPart{rig.TextPart(huge)}},
		rig.NewMessage(rig.RoleUser, "结论是什么？"),
	}
}

// auditAssertPairing 断言工具结果没有变成"孤儿"：每条 tool 消息之前都必须出现
// 过带同名 ID 的 assistant tool_calls。
func auditAssertPairing(t *testing.T, messages []rig.Message) {
	t.Helper()
	open := make(map[string]bool)
	for index, message := range messages {
		switch message.Role {
		case rig.RoleTool:
			if index == 0 {
				t.Fatalf("第 0 条是孤立 tool 消息")
			}
			if !open[message.ToolCallID] {
				t.Fatalf("第 %d 条 tool 消息（%s）没有对应的 assistant tool_calls",
					index, message.ToolCallID)
			}
		case rig.RoleAssistant:
			for _, call := range message.ToolCalls {
				open[call.ID] = true
			}
		}
	}
}

func TestAudit2HWindowCompactFitsBudgetWithHugeToolResult(t *testing.T) {
	const budget = 8000
	source := auditHugeToolResult()
	before := rig.EstimateTokens(source)
	if before < 150_000 {
		t.Fatalf("装配体只有 %d token，不足以触发预算逻辑", before)
	}
	compacted, err := WindowCompactor{KeepLast: 8}.Compact(context.Background(), "s", source, budget)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if got := rig.EstimateTokens(compacted); got > budget {
		t.Fatalf("压缩后仍估算 %d token，超过预算 %d", got, budget)
	}
	auditAssertPairing(t, compacted)
	// 当前用户输入必须原样保留。
	last := compacted[len(compacted)-1]
	if last.Role != rig.RoleUser || last.Text() != "结论是什么？" {
		t.Fatalf("最后一条消息被改坏: %+v", last)
	}
}

func TestAudit2HWindowCompactPrunesToolResults(t *testing.T) {
	source := []rig.Message{
		rig.NewMessage(rig.RoleSystem, "sys"),
		{Role: rig.RoleAssistant, ToolCalls: []rig.ToolCall{{ID: "a", Name: "t", Arguments: "{}"}}},
		{Role: rig.RoleTool, ToolCallID: "a", Name: "t", Content: []rig.ContentPart{rig.TextPart("old result")}},
		{Role: rig.RoleAssistant, ToolCalls: []rig.ToolCall{{ID: "b", Name: "t", Arguments: "{}"}}},
		{Role: rig.RoleTool, ToolCallID: "b", Name: "t", Content: []rig.ContentPart{rig.TextPart("new result")}},
		rig.NewMessage(rig.RoleUser, "go on"),
	}
	compacted, err := WindowCompactor{KeepLast: 5, ToolResultKeepLast: 1}.
		Compact(context.Background(), "s", source, 0)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	auditAssertPairing(t, compacted)
	var pruned, kept int
	for _, message := range compacted {
		if message.Role != rig.RoleTool {
			continue
		}
		if message.Text() == ToolResultPrunedMarker {
			pruned++
			continue
		}
		kept++
	}
	if pruned != 1 || kept != 1 {
		t.Fatalf("pruned=%d kept=%d，want 1/1（只保留最新一条工具结果）", pruned, kept)
	}
}

// TestAudit2HCompactNeverSplitsToolBlock：切点落在工具块中间时，整块保留或整块丢。
func TestAudit2HCompactNeverSplitsToolBlock(t *testing.T) {
	source := []rig.Message{
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
	for _, keepLast := range []int{1, 2, 3, 4} {
		compacted, err := WindowCompactor{KeepLast: keepLast}.Compact(context.Background(), "s", source, 0)
		if err != nil {
			t.Fatalf("Compact(%d): %v", keepLast, err)
		}
		auditAssertPairing(t, compacted)
	}
}

func TestAudit2HLLMCompactAdaptivelyPrunesToBudget(t *testing.T) {
	const budget = 6000
	source := auditHugeToolResult()
	compactor := LLMCompactor{
		Provider: &fakeProvider{stream: &fakeStream{chunks: []rig.Chunk{{Delta: "摘要"}}}},
		Model:    "m",
		KeepLast: 8,
	}
	compacted, err := compactor.Compact(context.Background(), "s", source, budget)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if got := rig.EstimateTokens(compacted); got > budget {
		t.Fatalf("LLMCompactor 压缩后估算 %d token，超过预算 %d", got, budget)
	}
	auditAssertPairing(t, compacted)
	// 摘要必须在。
	sawSummary := false
	for _, message := range compacted {
		if IsCompactionSummary(message) {
			sawSummary = true
		}
	}
	if !sawSummary {
		t.Fatal("压缩结果里没有摘要")
	}
}

// TestAudit2HFitBudgetKeepsAtLeastOneMessage：极端预算下也不能返回空对话。
func TestAudit2HFitBudgetKeepsAtLeastOneMessage(t *testing.T) {
	source := []rig.Message{
		rig.NewMessage(rig.RoleSystem, "sys"),
		rig.NewMessage(rig.RoleUser, strings.Repeat("长", 10_000)),
		rig.NewMessage(rig.RoleAssistant, strings.Repeat("回", 10_000)),
		rig.NewMessage(rig.RoleUser, "最后的问题"),
	}
	compacted, err := WindowCompactor{KeepLast: 3}.Compact(context.Background(), "s", source, 1)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if len(compacted) == 0 {
		t.Fatal("压缩结果为空")
	}
	last := compacted[len(compacted)-1]
	if last.Text() != "最后的问题" {
		t.Fatalf("最后一条消息被截断: %q", last.Text())
	}
}
