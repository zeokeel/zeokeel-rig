package compact

// 本文件钉住"压缩摘要链"的语义：旧摘要既不能凭空消失，计数也要累计。
//
// 修复前的两个漏洞（同一处根因）：
//   - splitConversation 无条件丢弃旧摘要，而新摘要只在 dropped>0 时插入，于是
//     dropped==0（窗口没超但整段超预算）时旧摘要正文凭空消失；
//   - dropped>0 时旧摘要既不保留也不进摘要请求，上一轮的语义内容全丢，计数也
//     只统计本轮丢弃数（40 条之后又压 6 条报 "6 messages" 而不是 "46"）。
//
// 这里的两条主用例都是"连续两次压缩"，因为只有第二次压缩才能暴露第一次写下的
// 摘要是否被保留。

import (
	"context"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
)

// summaryMessages 返回结果里所有压缩摘要消息。
func summaryMessages(messages []rig.Message) []rig.Message {
	out := make([]rig.Message, 0, 1)
	for _, message := range messages {
		if IsCompactionSummary(message) {
			out = append(out, message)
		}
	}
	return out
}

// onlySummary 断言结果里恰好有一条摘要并返回它。
func onlySummary(t *testing.T, messages []rig.Message) rig.Message {
	t.Helper()
	summaries := summaryMessages(messages)
	if len(summaries) != 1 {
		t.Fatalf("摘要条数 = %d，want 1：%#v", len(summaries), messages)
	}
	return summaries[0]
}

// chainMessages 构造 n 条交替的 user/assistant 消息（非 system）。
func chainMessages(n int) []rig.Message {
	out := make([]rig.Message, 0, n)
	for index := 0; index < n; index++ {
		if index%2 == 0 {
			out = append(out, textMessage(rig.RoleUser, "u"+itoa(index)))
			continue
		}
		out = append(out, textMessage(rig.RoleAssistant, "a"+itoa(index)))
	}
	return out
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

// TestSplitSummaryParsesCountAndBody 钉住解析函数：计数帧 + 正文、只有帧、以及
// 不带前缀的"手工摘要"。
func TestSplitSummaryParsesCountAndBody(t *testing.T) {
	count, body, ok := splitSummary(SummaryMessage(40, "phase one: chose plan A"))
	if !ok || count != 40 || body != "phase one: chose plan A" {
		t.Fatalf("splitSummary = (%d, %q, %v)", count, body, ok)
	}
	// 只有计数帧（WindowCompactor 产出的形态）。
	count, body, ok = splitSummary(SummaryMessage(7, ""))
	if !ok || count != 7 || body != "" {
		t.Fatalf("frame-only splitSummary = (%d, %q, %v)", count, body, ok)
	}
	// assistant 角色的摘要同样可解析（手动压缩持久化后的形态）。
	assistant := rig.NewMessage(rig.RoleAssistant, CompactionSummaryContent(3, "third"))
	count, body, ok = splitSummary(assistant)
	if !ok || count != 3 || body != "third" {
		t.Fatalf("assistant splitSummary = (%d, %q, %v)", count, body, ok)
	}
	// 普通 system 消息不是摘要。
	if _, _, ok := splitSummary(textMessage(rig.RoleSystem, "system")); ok {
		t.Fatal("普通 system 消息被当成摘要解析")
	}
	// 计数不是数字时不可信。
	malformed := textMessage(rig.RoleSystem, SummaryPrefix+"many messages)\n\nbody")
	if _, _, ok := splitSummary(malformed); ok {
		t.Fatal("非数字计数被当成可信摘要")
	}
}

// TestWindowCompactKeepsPriorSummaryWhenNothingDropped：窗口没超（dropped==0）但
// 整段超预算时，旧摘要必须原样保留在普通 system 消息之后。
func TestWindowCompactKeepsPriorSummaryWhenNothingDropped(t *testing.T) {
	previous := SummaryMessage(40, "phase one: chose plan A")
	messages := append([]rig.Message{
		textMessage(rig.RoleSystem, "sys"),
		previous,
	}, chainMessages(3)...)

	out, err := WindowCompactor{KeepLast: 8}.Compact(context.Background(), "s", messages, 100)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	summary := onlySummary(t, out)
	if count, body, ok := splitSummary(summary); !ok || count != 40 || body != "phase one: chose plan A" {
		t.Fatalf("旧摘要被改写: (%d, %q, %v), 原文 %q", count, body, ok, summary.Text())
	}
	if out[0].Role != rig.RoleSystem || out[0].Text() != "sys" {
		t.Fatalf("普通 system 消息顺序被破坏: %#v", out)
	}
	if out[1].Text() != previous.Text() {
		t.Fatalf("旧摘要没有紧跟普通 system 消息: %#v", out)
	}
	if got := len(out); got != 5 {
		t.Fatalf("压缩后消息数 = %d，want 5: %#v", got, out)
	}
}

// TestWindowCompactCarriesSummaryBodyAcrossRounds：连续两次压缩后，第一次写下的
// 摘要正文仍然在，且计数累计。
func TestWindowCompactCarriesSummaryBodyAcrossRounds(t *testing.T) {
	// 第一轮：用 LLMCompactor 写一条带正文的摘要（WindowCompactor 自己只写计数帧）。
	provider := &fakeProvider{stream: &fakeStream{chunks: []rig.Chunk{{Delta: "phase one: chose plan A"}}}}
	first, err := LLMCompactor{Provider: provider, Model: "m", KeepLast: 40}.
		Compact(context.Background(), "s", append([]rig.Message{textMessage(rig.RoleSystem, "sys")}, chainMessages(80)...), 0)
	if err != nil {
		t.Fatalf("首次 Compact: %v", err)
	}
	if count, body, _ := splitSummary(onlySummary(t, first)); count != 40 || body != "phase one: chose plan A" {
		t.Fatalf("第一轮摘要 = (%d, %q)，want (40, phase one)", count, body)
	}

	// 第二轮：WindowCompactor 再压一次（+6 条消息）。
	second, err := WindowCompactor{KeepLast: 40}.Compact(context.Background(), "s", append(first, chainMessages(6)...), 0)
	if err != nil {
		t.Fatalf("第二次 Compact: %v", err)
	}
	count, body, ok := splitSummary(onlySummary(t, second))
	if !ok {
		t.Fatalf("第二次压缩后没有摘要: %#v", second)
	}
	if count != 46 {
		t.Fatalf("累计计数 = %d，want 46（40 + 本轮 6，而不是 6）", count)
	}
	if !strings.Contains(body, "phase one: chose plan A") {
		t.Fatalf("旧摘要正文丢失: %q", body)
	}
}

// TestWindowCompactSummaryChainAccumulates 纯 WindowCompactor 两轮：计数逐轮累计。
func TestWindowCompactSummaryChainAccumulates(t *testing.T) {
	first, err := WindowCompactor{KeepLast: 40}.
		Compact(context.Background(), "s", append([]rig.Message{textMessage(rig.RoleSystem, "sys")}, chainMessages(80)...), 0)
	if err != nil {
		t.Fatalf("首次 Compact: %v", err)
	}
	if count, _, _ := splitSummary(onlySummary(t, first)); count != 40 {
		t.Fatalf("第一轮计数 = %d，want 40", count)
	}
	second, err := WindowCompactor{KeepLast: 40}.Compact(context.Background(), "s", append(first, chainMessages(6)...), 0)
	if err != nil {
		t.Fatalf("第二次 Compact: %v", err)
	}
	if count, _, _ := splitSummary(onlySummary(t, second)); count != 46 {
		t.Fatalf("第二轮计数 = %d，want 46", count)
	}
}

// TestLLMCompactKeepsPriorSummaryAcrossRounds：连续两次 LLM 压缩——摘要请求里必须
// 能看到旧摘要正文，最终摘要也必须保留它，计数累计。
func TestLLMCompactKeepsPriorSummaryAcrossRounds(t *testing.T) {
	firstProvider := &fakeProvider{stream: &fakeStream{chunks: []rig.Chunk{{Delta: "phase one: chose plan A"}}}}
	first, err := LLMCompactor{Provider: firstProvider, Model: "m", KeepLast: 40}.
		Compact(context.Background(), "s", append([]rig.Message{textMessage(rig.RoleSystem, "sys")}, chainMessages(80)...), 0)
	if err != nil {
		t.Fatalf("首次 Compact: %v", err)
	}
	if count, body, _ := splitSummary(onlySummary(t, first)); count != 40 || body != "phase one: chose plan A" {
		t.Fatalf("第一轮摘要 = (%d, %q)", count, body)
	}

	secondProvider := &fakeProvider{stream: &fakeStream{chunks: []rig.Chunk{{Delta: "phase two: wrote the fix"}}}}
	second, err := LLMCompactor{Provider: secondProvider, Model: "m", KeepLast: 40}.
		Compact(context.Background(), "s", append(first, chainMessages(6)...), 0)
	if err != nil {
		t.Fatalf("第二次 Compact: %v", err)
	}

	// 摘要请求里必须包含旧摘要正文。
	if len(secondProvider.requests) != 1 {
		t.Fatalf("摘要请求次数 = %d，want 1", len(secondProvider.requests))
	}
	sawPrior := false
	for _, message := range secondProvider.requests[0].Messages {
		if strings.Contains(message.Text(), "phase one: chose plan A") {
			sawPrior = true
		}
	}
	if !sawPrior {
		t.Fatalf("摘要请求里看不到旧摘要正文: %#v", secondProvider.requests[0].Messages)
	}

	count, body, ok := splitSummary(onlySummary(t, second))
	if !ok {
		t.Fatalf("第二次压缩后没有摘要: %#v", second)
	}
	if count != 46 {
		t.Fatalf("累计计数 = %d，want 46", count)
	}
	if !strings.Contains(body, "phase one: chose plan A") || !strings.Contains(body, "phase two: wrote the fix") {
		t.Fatalf("摘要正文没有合并新旧内容: %q", body)
	}
	// 摘要链仍然只有一条消息。
	if summaries := summaryMessages(second); len(summaries) != 1 {
		t.Fatalf("摘要条数 = %d，want 1（替换而不是累积）", len(summaries))
	}
}

// TestLLMCompactKeepsPriorSummaryWhenNothingDropped：LLM 路径的 dropped==0 分支。
func TestLLMCompactKeepsPriorSummaryWhenNothingDropped(t *testing.T) {
	provider := &fakeProvider{stream: &fakeStream{chunks: []rig.Chunk{{Delta: "unused"}}}}
	messages := append([]rig.Message{
		textMessage(rig.RoleSystem, "sys"),
		SummaryMessage(40, "phase one: chose plan A"),
	}, chainMessages(3)...)
	out, err := LLMCompactor{Provider: provider, Model: "m", KeepLast: 8}.
		Compact(context.Background(), "s", messages, 100)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if len(provider.requests) != 0 {
		t.Fatalf("窗口未超时不应调用摘要 provider（调用 %d 次）", len(provider.requests))
	}
	count, body, ok := splitSummary(onlySummary(t, out))
	if !ok || count != 40 || body != "phase one: chose plan A" {
		t.Fatalf("LLM 路径旧摘要丢失/被改写: (%d, %q, %v)", count, body, ok)
	}
}

// assertToolPairing 断言 assistant tool_calls 与 tool 结果仍然配对（孤儿 tool 消息
// 会让 provider 直接 400）。
func assertToolPairing(t *testing.T, messages []rig.Message) {
	t.Helper()
	open := make(map[string]bool)
	for index, message := range messages {
		switch message.Role {
		case rig.RoleTool:
			if index == 0 {
				t.Fatalf("第 0 条是孤立 tool 消息: %#v", messages)
			}
			if !open[message.ToolCallID] {
				t.Fatalf("第 %d 条 tool 消息（%s）没有对应的 assistant tool_calls: %#v", index, message.ToolCallID, messages)
			}
		case rig.RoleAssistant:
			for _, call := range message.ToolCalls {
				open[call.ID] = true
			}
		}
	}
}

// TestCompactRoundsKeepToolPairing：连续两次压缩后，assistant tool_calls 与 tool
// 结果仍成对（摘要消息是 system，不参与配对）。
func TestCompactRoundsKeepToolPairing(t *testing.T) {
	toolCall := func(id string) rig.Message {
		return rig.Message{Role: rig.RoleAssistant, ToolCalls: []rig.ToolCall{{ID: id, Name: "t", Arguments: `{}`}}}
	}
	messages := []rig.Message{
		textMessage(rig.RoleSystem, "sys"),
		textMessage(rig.RoleUser, "task 1"),
		toolCall("a"),
		toolMessage("a", "ra"),
		textMessage(rig.RoleAssistant, "done a"),
		textMessage(rig.RoleUser, "task 2"),
		toolCall("b"),
		toolMessage("b", "rb"),
		textMessage(rig.RoleAssistant, "done b"),
		textMessage(rig.RoleUser, "task 3"),
		toolCall("c"),
		toolMessage("c", "rc"),
		textMessage(rig.RoleAssistant, "done c"),
		textMessage(rig.RoleUser, "task 4"),
	}
	for _, compactor := range []rig.Compactor{
		WindowCompactor{KeepLast: 3},
		LLMCompactor{Provider: &fakeProvider{stream: &fakeStream{chunks: []rig.Chunk{{Delta: "phase one"}}}}, KeepLast: 3},
	} {
		first, err := compactor.Compact(context.Background(), "s", messages, 0)
		if err != nil {
			t.Fatalf("Compact: %v", err)
		}
		assertToolPairing(t, first)
		second, err := compactor.Compact(context.Background(), "s", append(first, textMessage(rig.RoleUser, "task 5")), 0)
		if err != nil {
			t.Fatalf("第二次 Compact: %v", err)
		}
		assertToolPairing(t, second)
	}
}
