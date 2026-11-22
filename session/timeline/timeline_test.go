package timeline_test

// 耐久工具时间线的恢复测试（对应产品侧 tool_timeline 的恢复用例）：
// Rig 历史只记录某轮发起了哪些调用，不记录调用出现在回复正文的哪个位置；
// 耐久 tool_call/tool_result 事件携带 text_offset 与结果状态，恢复时必须
// 回放这些元数据，让"先调工具再写正文"的轮次在重启后保持原顺序。旧版本
// 事件没有 text_offset 时，消息必须保持"时间线未知"的旧回退。

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	rig "github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/session/timeline"
)

// memoryLog 是进程内的事件日志。它只实现 timeline.EventLog，事件按 seq 顺序
// 保存，并可按 session 过滤。
type memoryLog struct {
	mu     sync.Mutex
	events map[string][]rig.SessionEvent
	err    error
	reads  int
}

func newMemoryLog(sessionID string, events ...rig.SessionEvent) *memoryLog {
	log := &memoryLog{events: make(map[string][]rig.SessionEvent)}
	for _, event := range events {
		event.SessionID = sessionID
		log.events[sessionID] = append(log.events[sessionID], event)
	}
	return log
}

func (l *memoryLog) Events(ctx context.Context, sessionID string, from, to int64) ([]rig.SessionEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reads++
	if l.err != nil {
		return nil, l.err
	}
	out := make([]rig.SessionEvent, 0, len(l.events[sessionID]))
	for _, event := range l.events[sessionID] {
		if from > 0 && event.Seq < from {
			continue
		}
		if to > 0 && event.Seq > to {
			continue
		}
		out = append(out, event)
	}
	return out, nil
}

func (l *memoryLog) readCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reads
}

// callEvent 构造一个 tool_call 事件；offset 为 nil 表示旧版本事件。
func callEvent(t *testing.T, id string, offset *int, at time.Time) rig.SessionEvent {
	t.Helper()
	payload := map[string]any{"id": id}
	if offset != nil {
		payload["text_offset"] = *offset
	}
	return rig.SessionEvent{Type: timeline.EventToolCall, Time: at, Payload: mustJSON(t, payload)}
}

// resultEvent 构造一个 tool_result 事件。
func resultEvent(t *testing.T, id, status, content string, at time.Time) rig.SessionEvent {
	t.Helper()
	return rig.SessionEvent{
		Type: timeline.EventToolResult,
		Time: at,
		Payload: mustJSON(t, map[string]any{
			"id":      id,
			"status":  status,
			"content": content,
		}),
	}
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return raw
}

func intPtr(value int) *int { return &value }

func assistantWithCalls(calls ...rig.ToolCall) rig.Message {
	return rig.Message{Role: rig.RoleAssistant, ToolCalls: calls}
}

// TestHydrateFoldsLiveToolTimeline 覆盖重启后的主场景：调用发生在正文之前，
// 记录的偏移是合法的 0；结果事件恢复状态与摘要。
func TestHydrateFoldsLiveToolTimeline(t *testing.T) {
	start := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	end1 := start.Add(2 * time.Second)
	end2 := end1.Add(time.Second)
	log := newMemoryLog("sess-tools",
		callEvent(t, "call-1", intPtr(0), start),
		callEvent(t, "call-2", intPtr(0), start.Add(500*time.Millisecond)),
		resultEvent(t, "call-1", "ok", "build ok", end1),
		resultEvent(t, "call-2", "error", "open go.mod: no such file", end2),
	)

	placements, err := timeline.Hydrate(context.Background(), log, "sess-tools")
	if err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	if len(placements) != 2 {
		t.Fatalf("placements = %+v, want two", placements)
	}
	first, second := placements[0], placements[1]
	if first.CallID != "call-1" || second.CallID != "call-2" {
		t.Fatalf("call ids = %q/%q, want call-1/call-2", first.CallID, second.CallID)
	}
	if first.TextOffset == nil || *first.TextOffset != 0 {
		t.Fatalf("first TextOffset = %v, want the recorded 0", first.TextOffset)
	}
	if second.TextOffset == nil || *second.TextOffset != 0 {
		t.Fatalf("second TextOffset = %v, want the recorded 0", second.TextOffset)
	}
	if first.Status != "ok" || first.Summary != "build ok" {
		t.Fatalf("first placement = %+v, want ok/build ok", first)
	}
	if second.Status != "error" || second.Summary != "open go.mod: no such file" {
		t.Fatalf("second placement = %+v, want error/summary", second)
	}
	if !first.StartedAt.Equal(start) || !first.EndedAt.Equal(end1) {
		t.Fatalf("first times = %v..%v, want %v..%v", first.StartedAt, first.EndedAt, start, end1)
	}
	if !second.StartedAt.Equal(start.Add(500*time.Millisecond)) || !second.EndedAt.Equal(end2) {
		t.Fatalf("second times = %v..%v, want its own clocks", second.StartedAt, second.EndedAt)
	}
}

// TestHydrateKeepsLegacyOffsetUnknown 覆盖旧版本事件：缺失 text_offset 必须
// 保持 nil（未知），不能被误读成 0；结果状态仍要恢复。
func TestHydrateKeepsLegacyOffsetUnknown(t *testing.T) {
	log := newMemoryLog("sess-old",
		callEvent(t, "call-1", nil, time.Time{}),
		resultEvent(t, "call-1", "ok", "done", time.Time{}),
	)
	placements, err := timeline.Hydrate(context.Background(), log, "sess-old")
	if err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	if len(placements) != 1 {
		t.Fatalf("placements = %+v, want one", placements)
	}
	if placements[0].TextOffset != nil {
		t.Fatalf("legacy TextOffset = %v, want nil (unknown)", *placements[0].TextOffset)
	}
	if placements[0].Status != "ok" || placements[0].Summary != "done" {
		t.Fatalf("legacy placement = %+v, want result metadata", placements[0])
	}
	if !placements[0].StartedAt.IsZero() || !placements[0].EndedAt.IsZero() {
		t.Fatalf("legacy times = %v..%v, want unknown (zero)",
			placements[0].StartedAt, placements[0].EndedAt)
	}
}

// TestHydrateRecordsFileMetadataAndErrorKind 覆盖 tool_call 的 path/generated
// 与 tool_result 的持久化 error_kind（不做内容推断）。
func TestHydrateRecordsFileMetadataAndErrorKind(t *testing.T) {
	start := time.Date(2026, 11, 22, 11, 0, 0, 0, time.UTC)
	log := newMemoryLog("sess-files", rig.SessionEvent{
		Type: timeline.EventToolCall,
		Time: start,
		Payload: mustJSON(t, map[string]any{
			"id":          "call-patch",
			"path":        "src/a.go",
			"text_offset": 5,
			"generated": []map[string]any{
				{"path": "src/a.go", "added": true},
				{"path": "src/old.go", "deleted": true},
			},
		}),
	}, rig.SessionEvent{
		Type: timeline.EventToolResult,
		Time: start.Add(time.Second),
		Payload: mustJSON(t, map[string]any{
			"id":         "call-patch",
			"status":     "error",
			"content":    "sandbox denied",
			"error_kind": "sandbox_denied",
		}),
	})

	placements, err := timeline.Hydrate(context.Background(), log, "sess-files")
	if err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	if len(placements) != 1 {
		t.Fatalf("placements = %+v, want one", placements)
	}
	placement := placements[0]
	if placement.Path != "src/a.go" {
		t.Fatalf("Path = %q, want src/a.go", placement.Path)
	}
	if len(placement.Generated) != 2 {
		t.Fatalf("Generated = %+v, want two files", placement.Generated)
	}
	if !placement.Generated[0].Added || placement.Generated[0].Deleted {
		t.Fatalf("first generated file = %+v, want added", placement.Generated[0])
	}
	if placement.Generated[1].Added || !placement.Generated[1].Deleted {
		t.Fatalf("second generated file = %+v, want deleted", placement.Generated[1])
	}
	if placement.TextOffset == nil || *placement.TextOffset != 5 {
		t.Fatalf("TextOffset = %v, want 5", placement.TextOffset)
	}
	if placement.ErrorKind != "sandbox_denied" {
		t.Fatalf("ErrorKind = %q, want the persisted kind", placement.ErrorKind)
	}
}

// TestHydrateSkipsOrphansMalformedAndEmptyIDs 覆盖坏数据：孤儿结果、畸形载荷
// 和空 id 都不能中断其余调用的回放。
func TestHydrateSkipsOrphansMalformedAndEmptyIDs(t *testing.T) {
	log := newMemoryLog("sess-noise",
		rig.SessionEvent{Type: timeline.EventToolResult,
			Payload: mustJSON(t, map[string]any{"id": "orphan", "status": "ok"})},
		rig.SessionEvent{Type: timeline.EventToolCall, Payload: json.RawMessage(`{"id":`)},
		rig.SessionEvent{Type: timeline.EventToolResult, Payload: json.RawMessage(`{"id":`)},
		callEvent(t, "   ", intPtr(0), time.Time{}),
		callEvent(t, " call-ok ", intPtr(3), time.Time{}),
		resultEvent(t, "call-ok", "ok", "fine", time.Time{}),
		// 孤儿结果出现在调用之后、但 id 不匹配，仍应被跳过。
		resultEvent(t, "other", "error", "ignored", time.Time{}),
	)

	placements, err := timeline.Hydrate(context.Background(), log, "sess-noise")
	if err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	if len(placements) != 1 {
		t.Fatalf("placements = %+v, want only the valid call", placements)
	}
	placement := placements[0]
	if placement.CallID != "call-ok" {
		t.Fatalf("CallID = %q, want trimmed call-ok", placement.CallID)
	}
	if placement.TextOffset == nil || *placement.TextOffset != 3 {
		t.Fatalf("TextOffset = %v, want 3", placement.TextOffset)
	}
	if placement.Status != "ok" || placement.Summary != "fine" {
		t.Fatalf("placement = %+v, want its own result", placement)
	}
}

// TestHydrateReadsOnlyTheRequestedSession 确认日志读取按会话隔离，空会话与
// nil 日志不发读。
func TestHydrateReadsOnlyTheRequestedSession(t *testing.T) {
	log := newMemoryLog("sess-a", callEvent(t, "call-1", intPtr(0), time.Time{}))
	log.events["sess-b"] = []rig.SessionEvent{
		{SessionID: "sess-b", Type: timeline.EventToolCall,
			Payload: mustJSON(t, map[string]any{"id": "call-9", "text_offset": 9})},
	}

	placements, err := timeline.Hydrate(context.Background(), log, "sess-a")
	if err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	if len(placements) != 1 || placements[0].CallID != "call-1" {
		t.Fatalf("placements = %+v, want only sess-a's call", placements)
	}

	if placements, err := timeline.Hydrate(context.Background(), log, "  "); err != nil || placements != nil {
		t.Fatalf("Hydrate(empty session) = %+v, %v; want nil, nil", placements, err)
	}
	if placements, err := timeline.Hydrate(context.Background(), nil, "sess-a"); err != nil || placements != nil {
		t.Fatalf("Hydrate(nil log) = %+v, %v; want nil, nil", placements, err)
	}
	if reads := log.readCount(); reads != 1 {
		t.Fatalf("log reads = %d, want only the one real read", reads)
	}
}

// TestHydratePropagatesLogErrorAndContext 覆盖失败路径：日志错误被包装且可
// errors.Is；已取消的 context 原样返回。
func TestHydratePropagatesLogErrorAndContext(t *testing.T) {
	boom := errors.New("boom")
	log := newMemoryLog("sess-err")
	log.err = boom
	if _, err := timeline.Hydrate(context.Background(), log, "sess-err"); !errors.Is(err, boom) {
		t.Fatalf("Hydrate error = %v, want wrapped boom", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := timeline.Hydrate(ctx, newMemoryLog("sess-cancel"), "sess-cancel"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Hydrate(canceled) = %v, want context.Canceled", err)
	}
}

// TestApplyHydratesLiveTimeline 覆盖落位：一条带两个调用的助手消息，两个
// 偏移都显式记录时时间线已知。
func TestApplyHydratesLiveTimeline(t *testing.T) {
	messages := []rig.Message{
		rig.NewMessage(rig.RoleUser, "run it"),
		assistantWithCalls(
			rig.ToolCall{ID: "call-1", Name: "bash"},
			rig.ToolCall{ID: "call-2", Name: "read_file"},
		),
	}
	placements := []timeline.Placement{
		{CallID: "call-1", TextOffset: intPtr(0), Status: "ok", Summary: "build ok"},
		{CallID: "call-2", TextOffset: intPtr(0), Status: "error", Summary: "missing"},
	}

	timelines := timeline.Apply(messages, placements)
	if len(timelines) != 1 {
		t.Fatalf("timelines = %+v, want one message", timelines)
	}
	hydrated := timelines[0]
	if hydrated.Message != 1 {
		t.Fatalf("Message = %d, want 1", hydrated.Message)
	}
	if !hydrated.Known {
		t.Fatal("a fully timed message must report a known timeline")
	}
	if len(hydrated.Tools) != 2 {
		t.Fatalf("Tools = %+v, want two", hydrated.Tools)
	}
	if hydrated.Tools[0].Tool != 0 || hydrated.Tools[1].Tool != 1 {
		t.Fatalf("tool indexes = %d/%d, want 0/1", hydrated.Tools[0].Tool, hydrated.Tools[1].Tool)
	}
	if got := hydrated.Tools[0].TextOffset; got == nil || *got != 0 || hydrated.Tools[0].Status != "ok" {
		t.Fatalf("first tool = %+v, want offset 0/status ok", hydrated.Tools[0].Placement)
	}
	if got := hydrated.Tools[1].Status; got != "error" || hydrated.Tools[1].Summary != "missing" {
		t.Fatalf("second tool = %+v, want error/missing", hydrated.Tools[1].Placement)
	}
}

// TestApplyKeepsPartialOffsetsUnknown 覆盖混合日志：一个调用有偏移、另一个
// 事件早于该字段；偏移仍落位，但消息整体保持未知（否则会有一个卡片错位到 0）。
func TestApplyKeepsPartialOffsetsUnknown(t *testing.T) {
	messages := []rig.Message{assistantWithCalls(
		rig.ToolCall{ID: "call-1", Name: "bash"},
		rig.ToolCall{ID: "call-2", Name: "read_file"},
	)}
	placements := []timeline.Placement{
		{CallID: "call-1", TextOffset: intPtr(12)},
		{CallID: "call-2"}, // 旧事件：没有 text_offset
	}

	timelines := timeline.Apply(messages, placements)
	if len(timelines) != 1 {
		t.Fatalf("timelines = %+v, want one", timelines)
	}
	if timelines[0].Known {
		t.Fatal("a partially hydratable timeline must stay unknown")
	}
	if got := timelines[0].Tools[0].TextOffset; got == nil || *got != 12 {
		t.Fatalf("recorded offset lost: %+v", timelines[0].Tools[0].Placement)
	}
	if timelines[0].Tools[1].TextOffset != nil {
		t.Fatalf("legacy call got a fabricated offset: %v", *timelines[0].Tools[1].TextOffset)
	}
}

// TestApplyPairsRepeatedCallIDsInOrder 覆盖按轮复用 id 的 provider：同一个
// call-0 出现在两轮，必须按时间顺序各取自己的事件，不能被后一轮覆盖。
func TestApplyPairsRepeatedCallIDsInOrder(t *testing.T) {
	messages := []rig.Message{
		assistantWithCalls(rig.ToolCall{ID: "call-0", Name: "bash"}),
		rig.NewMessage(rig.RoleUser, "again"),
		assistantWithCalls(rig.ToolCall{ID: "call-0", Name: "bash"}),
	}
	placements := []timeline.Placement{
		{CallID: "call-0", TextOffset: intPtr(11), Status: "ok", Summary: "first ok"},
		{CallID: "call-0", TextOffset: intPtr(12), Status: "error", Summary: "second failed"},
	}

	timelines := timeline.Apply(messages, placements)
	if len(timelines) != 2 {
		t.Fatalf("timelines = %+v, want two messages", timelines)
	}
	first, second := timelines[0], timelines[1]
	if first.Message != 0 || second.Message != 2 {
		t.Fatalf("message indexes = %d/%d, want 0/2", first.Message, second.Message)
	}
	if !first.Known || !second.Known {
		t.Fatal("repeated ids with matching counts must both be known")
	}
	if got := first.Tools[0]; got.TextOffset == nil || *got.TextOffset != 11 ||
		got.Status != "ok" || got.Summary != "first ok" {
		t.Fatalf("first turn tool = %+v, want turn-one event", got.Placement)
	}
	if got := second.Tools[0]; got.TextOffset == nil || *got.TextOffset != 12 ||
		got.Status != "error" || got.Summary != "second failed" {
		t.Fatalf("second turn tool = %+v, want turn-two event", got.Placement)
	}
}

// TestApplySkipsMismatchedCallIDCounts 覆盖回退/重新生成：日志里同一 id 有
// 两次调用、历史只剩一次，按 id 硬配会贴上旧运行的偏移，必须整体跳过。
func TestApplySkipsMismatchedCallIDCounts(t *testing.T) {
	messages := []rig.Message{assistantWithCalls(rig.ToolCall{ID: "call-0", Name: "bash"})}
	placements := []timeline.Placement{
		{CallID: "call-0", TextOffset: intPtr(4), Status: "ok", Summary: "dropped run"},
		{CallID: "call-0", TextOffset: intPtr(7), Status: "ok", Summary: "retried run"},
	}

	timelines := timeline.Apply(messages, placements)
	if len(timelines) != 1 {
		t.Fatalf("timelines = %+v, want one", timelines)
	}
	if timelines[0].Known {
		t.Fatal("ambiguous call-id counts must not certify the timeline")
	}
	if got := timelines[0].Tools[0]; got.TextOffset != nil || got.Status != "" || got.Summary != "" {
		t.Fatalf("ambiguous call got hydrated: %+v", got.Placement)
	}
}

// TestApplyKeepsOtherIDsWhenOneMismatches 确认一个 id 的条数不匹配不会连累
// 其他 id 的回放。
func TestApplyKeepsOtherIDsWhenOneMismatches(t *testing.T) {
	messages := []rig.Message{assistantWithCalls(
		rig.ToolCall{ID: "call-0", Name: "bash"},
		rig.ToolCall{ID: "call-1", Name: "read_file"},
	)}
	placements := []timeline.Placement{
		{CallID: "call-0", TextOffset: intPtr(4)},
		{CallID: "call-0", TextOffset: intPtr(7)},
		{CallID: "call-1", TextOffset: intPtr(5), Status: "ok", Summary: "fine"},
	}

	timelines := timeline.Apply(messages, placements)
	if len(timelines) != 1 {
		t.Fatalf("timelines = %+v, want one", timelines)
	}
	if timelines[0].Known {
		t.Fatal("one ambiguous id must keep the whole message unknown")
	}
	if timelines[0].Tools[0].TextOffset != nil {
		t.Fatalf("mismatched call got hydrated: %+v", timelines[0].Tools[0].Placement)
	}
	if got := timelines[0].Tools[1]; got.TextOffset == nil || *got.TextOffset != 5 || got.Status != "ok" {
		t.Fatalf("eligible call lost its hydration: %+v", got.Placement)
	}
}

// TestApplyEmptyCallIDNeverCertifies 覆盖空调用 id：它不参与排队，消息也
// 永远不能报告时间线已知（否则空 id 的卡片会被放到 0）。
func TestApplyEmptyCallIDNeverCertifies(t *testing.T) {
	messages := []rig.Message{assistantWithCalls(
		rig.ToolCall{ID: "  ", Name: "bash"},
		rig.ToolCall{ID: "call-1", Name: "read_file"},
	)}
	placements := []timeline.Placement{{CallID: "call-1", TextOffset: intPtr(3), Status: "ok"}}

	timelines := timeline.Apply(messages, placements)
	if len(timelines) != 1 {
		t.Fatalf("timelines = %+v, want one", timelines)
	}
	if timelines[0].Known {
		t.Fatal("a message with an empty call id cannot report a known timeline")
	}
	if timelines[0].Tools[0].CallID != "" || timelines[0].Tools[0].TextOffset != nil {
		t.Fatalf("empty-id call got hydrated: %+v", timelines[0].Tools[0].Placement)
	}
	if got := timelines[0].Tools[1]; got.TextOffset == nil || *got.TextOffset != 3 {
		t.Fatalf("valid call lost its offset: %+v", got.Placement)
	}
}

// TestApplyTrimsCallIDs 覆盖 id 两侧空白：历史与事件里的空白都必须归一化后
// 才能配对。
func TestApplyTrimsCallIDs(t *testing.T) {
	messages := []rig.Message{assistantWithCalls(rig.ToolCall{ID: " call-1 ", Name: "bash"})}
	placements := []timeline.Placement{{CallID: "  call-1", TextOffset: intPtr(2), Status: "ok"}}

	timelines := timeline.Apply(messages, placements)
	if len(timelines) != 1 || !timelines[0].Known {
		t.Fatalf("trimmed ids did not pair: %+v", timelines)
	}
	if got := timelines[0].Tools[0]; got.CallID != "call-1" || got.TextOffset == nil || *got.TextOffset != 2 {
		t.Fatalf("trimmed call = %+v, want call-1 @2", got.Placement)
	}
}

// TestApplyWithoutPlacementsOrCalls 覆盖空输入：没有工具调用时返回 nil；
// 没有事件时仍返回未知的时间线骨架。
func TestApplyWithoutPlacementsOrCalls(t *testing.T) {
	if got := timeline.Apply(nil, nil); got != nil {
		t.Fatalf("Apply(nil, nil) = %+v, want nil", got)
	}
	messages := []rig.Message{rig.NewMessage(rig.RoleUser, "hello")}
	if got := timeline.Apply(messages, nil); got != nil {
		t.Fatalf("Apply(no tool calls) = %+v, want nil", got)
	}

	messages = append(messages, assistantWithCalls(rig.ToolCall{ID: "call-1", Name: "bash"}))
	timelines := timeline.Apply(messages, nil)
	if len(timelines) != 1 {
		t.Fatalf("timelines = %+v, want the call message", timelines)
	}
	if timelines[0].Known {
		t.Fatal("no events must leave the timeline unknown")
	}
	if got := timelines[0].Tools[0]; got.CallID != "call-1" || got.TextOffset != nil {
		t.Fatalf("skeleton tool = %+v, want call-1 with unknown offset", got.Placement)
	}
}
