package observability_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/session/observability"
)

const session = "s1"

func encode(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return encoded
}

func event(eventType string, seq int64, at time.Time, payload json.RawMessage) rig.SessionEvent {
	return rig.SessionEvent{SessionID: session, Type: eventType, Seq: seq, Time: at, Payload: payload}
}

func TestFoldEventsBuildsTimeline(t *testing.T) {
	start := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	events := []rig.SessionEvent{
		event(observability.EventContextSnapshot, 1, start, encode(t, observability.ContextSnapshotPayload{
			Model:              " gpt-5 ",
			Window:             200000,
			SystemPromptBytes:  1000,
			SystemPromptTokens: 250,
			Sections: []observability.Section{
				{ID: "system", Label: "system", Source: "system", Bytes: 1000, Tokens: 250, Hash: "abc"},
			},
			Tools:     []observability.Tool{{Name: "bash", Group: "shell", Tokens: 80}},
			ToolCount: 1,
			History:   observability.History{Messages: 4, User: 2, Assistant: 1, Tool: 1},
			Injected:  observability.Injected{AgentsMD: true},
			// 估算的 prompt 只在该会话还没有真实调用时代表当前占用。
			EstimatedPrompt: 7000,
		})),
		event(observability.EventModelCall, 2, start.Add(time.Second), encode(t, observability.ModelCallPayload{
			Iteration:        1,
			Model:            "gpt-5",
			PromptTokens:     7000,
			CachedTokens:     1000,
			CompletionTokens: 100,
			TotalTokens:      7100,
			ContextTokens:    7000,
		})),
		event(observability.EventModelCall, 3, start.Add(2*time.Second), encode(t, observability.ModelCallPayload{
			Iteration:        2,
			Model:            "gpt-5",
			PromptTokens:     9000,
			CachedTokens:     500,
			CompletionTokens: 200,
			TotalTokens:      9200,
			ContextTokens:    9000,
		})),
		event(observability.EventCompact, 4, start.Add(3*time.Second), encode(t, observability.CompactPayload{
			Before: 9000, After: 4000, Replaced: 12, Saved: 2, Inferred: true,
		})),
		event(observability.EventSpill, 5, start.Add(4*time.Second), encode(t, observability.SpillPayload{
			CallID: "call-1", Tool: " bash ", Path: "/tmp/spill.txt", Bytes: 1234,
			Preview: strings.Repeat("p", 300),
		})),
	}
	timeline := observability.FoldEvents(events, session)

	if timeline.Revision != 5 || timeline.Skipped != 0 {
		t.Fatalf("revision/skipped = %d/%d, want 5/0", timeline.Revision, timeline.Skipped)
	}
	if !timeline.HasSnapshot {
		t.Fatal("HasSnapshot = false")
	}
	snapshot := timeline.Snapshot
	if snapshot.Seq != 1 || !snapshot.At.Equal(start) {
		t.Fatalf("snapshot seq/at = %d/%v", snapshot.Seq, snapshot.At)
	}
	if snapshot.Model != "gpt-5" || snapshot.Window != 200000 {
		t.Fatalf("snapshot model/window = %q/%d", snapshot.Model, snapshot.Window)
	}
	if len(snapshot.Sections) != 1 || snapshot.Sections[0].ID != "system" || snapshot.Sections[0].Tokens != 250 {
		t.Fatalf("snapshot sections = %+v", snapshot.Sections)
	}
	if len(snapshot.Tools) != 1 || snapshot.Tools[0].Name != "bash" || snapshot.ToolCount != 1 {
		t.Fatalf("snapshot tools = %+v (count %d)", snapshot.Tools, snapshot.ToolCount)
	}
	if snapshot.History.Tool != 1 || timeline.ToolCalls != 1 {
		t.Fatalf("history tool / ToolCalls = %d/%d, want 1/1", snapshot.History.Tool, timeline.ToolCalls)
	}
	if !snapshot.Injected.AgentsMD || snapshot.Injected.Memory {
		t.Fatalf("snapshot injected = %+v", snapshot.Injected)
	}
	if timeline.Window != 200000 {
		t.Fatalf("window = %d, want 200000", timeline.Window)
	}
	// 快照估算先占位，随后被真实调用的 ContextTokens 覆盖，最后被压缩后的值覆盖。
	if timeline.Used != 4000 {
		t.Fatalf("used = %d, want 4000", timeline.Used)
	}

	if timeline.CallsSeen != 2 || len(timeline.Calls) != 2 {
		t.Fatalf("calls seen/len = %d/%d", timeline.CallsSeen, len(timeline.Calls))
	}
	second := timeline.Calls[1]
	if second.Seq != 3 || second.Iteration != 2 || second.Model != "gpt-5" {
		t.Fatalf("second call = %+v", second)
	}
	if second.Prompt != 9000 || second.Cached != 500 || second.Completion != 200 || second.Total != 9200 || second.ContextTokens != 9000 {
		t.Fatalf("second call tokens = %+v", second)
	}
	if !second.At.Equal(start.Add(2 * time.Second)) {
		t.Fatalf("second call at = %v", second.At)
	}
	if timeline.Iterations != 2 {
		t.Fatalf("iterations = %d, want 2", timeline.Iterations)
	}
	wantTokens := observability.Tokens{Prompt: 16000, Cached: 1500, Completion: 300, Total: 16300}
	if timeline.Tokens != wantTokens {
		t.Fatalf("tokens = %+v, want %+v", timeline.Tokens, wantTokens)
	}

	if timeline.CompactionsSeen != 1 || len(timeline.Compactions) != 1 {
		t.Fatalf("compactions seen/len = %d/%d", timeline.CompactionsSeen, len(timeline.Compactions))
	}
	compaction := timeline.Compactions[0]
	if compaction.Kind != "manual" || compaction.Before != 9000 || compaction.After != 4000 ||
		compaction.Replaced != 12 || compaction.Saved != 2 || !compaction.Inferred || compaction.Seq != 4 {
		t.Fatalf("compaction = %+v", compaction)
	}

	if timeline.SpillsSeen != 1 || len(timeline.Spills) != 1 {
		t.Fatalf("spills seen/len = %d/%d", timeline.SpillsSeen, len(timeline.Spills))
	}
	spill := timeline.Spills[0]
	if spill.CallID != "call-1" || spill.Tool != "bash" || spill.Path != "/tmp/spill.txt" || spill.Bytes != 1234 {
		t.Fatalf("spill = %+v", spill)
	}
	if got := len([]rune(spill.Preview)); got != observability.MaxPreviewRunes {
		t.Fatalf("preview runes = %d, want %d", got, observability.MaxPreviewRunes)
	}
}

func TestFoldEventsToleratesMalformedPayloads(t *testing.T) {
	now := time.Now()
	validAnonymous := rig.SessionEvent{
		Type:    observability.EventModelCall,
		Payload: encode(t, observability.ModelCallPayload{Iteration: 1, PromptTokens: 10, ContextTokens: 10}),
	}
	events := []rig.SessionEvent{
		// 空 payload、JSON null、坏 JSON、字段类型不匹配：都跳过。
		event(observability.EventContextSnapshot, 1, now, nil),
		event(observability.EventModelCall, 2, now, json.RawMessage("null")),
		event(observability.EventCompact, 3, now, json.RawMessage(`{`)),
		event(observability.EventSpill, 4, now, json.RawMessage(`{"bytes":"not-a-number"}`)),
		// 未参与折叠的事件类型：跳过（compact_archive 属于产品侧，本包不认识）。
		event("compact_archive", 5, now, json.RawMessage(`{"kind":"auto"}`)),
		// 其它会话：忽略，且不算跳过。
		{SessionID: "other", Type: observability.EventModelCall, Payload: encode(t, observability.ModelCallPayload{PromptTokens: 99})},
		// 匿名单据与会话事件都折叠。
		validAnonymous,
		// 荒谬的负数字段压回 0，事件本身仍然有效。
		event(observability.EventModelCall, 6, now, json.RawMessage(`{"promptTokens":-5,"completionTokens":-1,"contextTokens":-1,"iteration":-2}`)),
		// 未知字段被忽略，已知字段照常解码。
		event(observability.EventModelCall, 7, now, json.RawMessage(`{"promptTokens":7,"iteration":1,"mystery":{"x":1}}`)),
	}
	timeline := observability.FoldEvents(events, session)

	if timeline.Skipped != 5 {
		t.Fatalf("skipped = %d, want 5", timeline.Skipped)
	}
	if timeline.Revision != 3 || timeline.CallsSeen != 3 || len(timeline.Calls) != 3 {
		t.Fatalf("revision/calls seen/len = %d/%d/%d", timeline.Revision, timeline.CallsSeen, len(timeline.Calls))
	}
	if timeline.HasSnapshot {
		t.Fatal("malformed snapshot payload produced a snapshot")
	}
	if timeline.Tokens.Prompt != 17 {
		t.Fatalf("prompt tokens = %d, want 10+0+7", timeline.Tokens.Prompt)
	}
	if timeline.Tokens.Completion != 0 {
		t.Fatalf("completion tokens = %d, want 0", timeline.Tokens.Completion)
	}
	if timeline.Iterations != 1 {
		t.Fatalf("iterations = %d, want 1", timeline.Iterations)
	}
	// 最后一次调用的 contextTokens 被压成 0，不能把占用写成负数。
	if timeline.Used != 10 {
		t.Fatalf("used = %d, want 10", timeline.Used)
	}
}

func TestFoldEventsBounds(t *testing.T) {
	now := time.Now()
	var events []rig.SessionEvent
	for index := 0; index < 250; index++ {
		events = append(events, event(observability.EventModelCall, int64(index+1), now, encode(t, observability.ModelCallPayload{
			Iteration: index + 1, PromptTokens: 10, CompletionTokens: 1, TotalTokens: 11, ContextTokens: index + 1,
		})))
	}
	for index := 0; index < 250; index++ {
		events = append(events, event(observability.EventCompact, int64(1000+index), now, encode(t, observability.CompactPayload{
			Kind: "manual", Before: 900, After: 100,
		})))
	}
	for index := 0; index < 250; index++ {
		events = append(events, event(observability.EventSpill, int64(2000+index), now, encode(t, observability.SpillPayload{
			Tool: "bash", Bytes: index, Preview: "x",
		})))
	}
	timeline := observability.FoldEvents(events, session)

	if timeline.CallsSeen != 250 || len(timeline.Calls) != observability.MaxCalls {
		t.Fatalf("calls seen/len = %d/%d, want 250/%d", timeline.CallsSeen, len(timeline.Calls), observability.MaxCalls)
	}
	if first, last := timeline.Calls[0], timeline.Calls[len(timeline.Calls)-1]; first.Iteration != 51 || last.Iteration != 250 {
		t.Fatalf("bounded call window = %d..%d, want 51..250", first.Iteration, last.Iteration)
	}
	if timeline.CompactionsSeen != 250 || len(timeline.Compactions) != observability.MaxEvents {
		t.Fatalf("compactions seen/len = %d/%d", timeline.CompactionsSeen, len(timeline.Compactions))
	}
	if timeline.SpillsSeen != 250 || len(timeline.Spills) != observability.MaxEvents {
		t.Fatalf("spills seen/len = %d/%d", timeline.SpillsSeen, len(timeline.Spills))
	}
	// 汇总覆盖全部调用，不随列表上界缩水。
	if timeline.Tokens.Prompt != 2500 || timeline.Tokens.Completion != 250 || timeline.Tokens.Total != 2750 {
		t.Fatalf("tokens = %+v", timeline.Tokens)
	}
	// 压缩事件把 Used 改成压缩后的值，所以最终占用是最后一次压缩的 After。
	if timeline.Iterations != 250 || timeline.Used != 100 {
		t.Fatalf("iterations/used = %d/%d, want 250/100", timeline.Iterations, timeline.Used)
	}
	if lastSpill := timeline.Spills[len(timeline.Spills)-1]; lastSpill.Bytes != 249 {
		t.Fatalf("last spill bytes = %d, want 249", lastSpill.Bytes)
	}
}

func TestFoldReadsLog(t *testing.T) {
	log := &fakeLog{events: []rig.SessionEvent{
		event(observability.EventModelCall, 1, time.Now(), encode(t, observability.ModelCallPayload{PromptTokens: 5})),
	}}

	timeline, err := observability.Fold(context.Background(), log, " s1 ", 0)
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if log.limit != observability.DefaultWindow || log.sessionID != "s1" {
		t.Fatalf("log call = %q/%d", log.sessionID, log.limit)
	}
	if timeline.CallsSeen != 1 || timeline.Revision != 1 {
		t.Fatalf("timeline = %+v", timeline)
	}

	if _, err := observability.Fold(context.Background(), log, session, 7); err != nil {
		t.Fatalf("Fold window: %v", err)
	}
	if log.limit != 7 {
		t.Fatalf("limit = %d, want 7", log.limit)
	}

	if timeline, err := observability.Fold(context.Background(), nil, session, 0); err != nil || timeline.Revision != 0 {
		t.Fatalf("nil log = %+v, %v", timeline, err)
	}

	empty := &fakeLog{}
	if timeline, err := observability.Fold(context.Background(), empty, "   ", 0); err != nil || timeline.Revision != 0 {
		t.Fatalf("empty session = %+v, %v", timeline, err)
	}
	if empty.calls != 0 {
		t.Fatalf("empty session queried the log %d times", empty.calls)
	}

	boom := errors.New("log unavailable")
	if _, err := observability.Fold(context.Background(), &fakeLog{err: boom}, session, 0); !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the log error", err)
	}
}

// TestFoldReadsProductPayloads 用产品侧真实写出的 payload（contracts 结构体没有
// JSON tag，因此键名是 Go 字段名、首字母大写）验证折叠的兼容性。
func TestFoldReadsProductPayloads(t *testing.T) {
	now := time.Now()
	snapshotPayload := json.RawMessage(`{
		"model":"gpt-5","window":128000,"systemPromptBytes":10,"systemPromptTokens":3,
		"sections":[{"ID":"system","Label":"system","Source":"system","Bytes":10,"Tokens":3,"Hash":"h"}],
		"tools":[{"Name":"bash","Group":"shell","Tokens":80}],
		"toolCount":1,
		"history":{"Messages":2,"User":1,"Assistant":1,"Tool":0},
		"injected":{"AgentsMD":true,"Preset":false,"Memory":false,"Skills":false},
		"estimatedPrompt":1234
	}`)
	callPayload := json.RawMessage(`{"iteration":1,"model":"gpt-5","promptTokens":1234,"cachedTokens":234,"completionTokens":56,"totalTokens":1290,"contextTokens":1234}`)
	timeline := observability.FoldEvents([]rig.SessionEvent{
		event(observability.EventContextSnapshot, 1, now, snapshotPayload),
		event(observability.EventModelCall, 2, now, callPayload),
	}, session)

	if !timeline.HasSnapshot {
		t.Fatal("product snapshot payload was not folded")
	}
	if got := timeline.Snapshot.Sections; len(got) != 1 || got[0].ID != "system" || got[0].Hash != "h" {
		t.Fatalf("sections = %+v", got)
	}
	if got := timeline.Snapshot.Tools; len(got) != 1 || got[0].Name != "bash" || got[0].Tokens != 80 {
		t.Fatalf("tools = %+v", got)
	}
	if !timeline.Snapshot.Injected.AgentsMD || timeline.Snapshot.History.Messages != 2 {
		t.Fatalf("injected/history = %+v/%+v", timeline.Snapshot.Injected, timeline.Snapshot.History)
	}
	if timeline.Snapshot.EstimatedPrompt != 1234 || timeline.Window != 128000 {
		t.Fatalf("estimated/window = %d/%d", timeline.Snapshot.EstimatedPrompt, timeline.Window)
	}
	if len(timeline.Calls) != 1 || timeline.Calls[0].Cached != 234 {
		t.Fatalf("calls = %+v", timeline.Calls)
	}
}

func TestParseSpillSummary(t *testing.T) {
	fileSummary := rig.SpillSummaryPrefix + "\npath: /tmp/spill.txt\nbytes: 2048\npreview: hello world"
	parsed, ok := observability.ParseSpillSummary(fileSummary)
	if !ok {
		t.Fatal("file summary not parsed")
	}
	if parsed.Path != "/tmp/spill.txt" || parsed.Bytes != 2048 || parsed.Preview != "hello world" || parsed.Ref != "" {
		t.Fatalf("file summary = %+v", parsed)
	}

	storeSummary := rig.SpillSummaryPrefix + "\nref: spill-abc\nbytes: 10\npreview: x"
	parsed, ok = observability.ParseSpillSummary(storeSummary)
	if !ok || parsed.Ref != "spill-abc" || parsed.Bytes != 10 {
		t.Fatalf("store summary = %+v, %v", parsed, ok)
	}

	// 没有可识别字段时不算 spill 摘要。
	if _, ok := observability.ParseSpillSummary(rig.SpillSummaryPrefix); ok {
		t.Fatal("bare prefix parsed as a spill summary")
	}
	if _, ok := observability.ParseSpillSummary("plain tool output"); ok {
		t.Fatal("non-spill content parsed as a spill summary")
	}

	// 坏字节数被忽略，但路径仍然让摘要可用。
	parsed, ok = observability.ParseSpillSummary(rig.SpillSummaryPrefix + "\npath: /tmp/a\nbytes: nope")
	if !ok || parsed.Path != "/tmp/a" || parsed.Bytes != 0 {
		t.Fatalf("bad bytes summary = %+v, %v", parsed, ok)
	}
}

func TestAutoCompactDrop(t *testing.T) {
	cases := []struct {
		previous, current int
		want              bool
	}{
		{10000, 7000, true},
		{10000, 8000, true}, // 恰好 20%
		{10000, 8100, false},
		{10000, 10000, false},
		{10000, 12000, false},
		{4096, 1000, true},
		{4095, 1000, false}, // 太小，波动不算压缩
		{10000, 0, false},
	}
	for _, testCase := range cases {
		if got := observability.AutoCompactDrop(testCase.previous, testCase.current); got != testCase.want {
			t.Fatalf("AutoCompactDrop(%d, %d) = %v, want %v", testCase.previous, testCase.current, got, testCase.want)
		}
	}
}

func TestObservedEvent(t *testing.T) {
	for _, eventType := range []string{
		observability.EventContextSnapshot,
		observability.EventModelCall,
		observability.EventCompact,
		observability.EventSpill,
	} {
		if !observability.ObservedEvent(eventType) {
			t.Fatalf("ObservedEvent(%q) = false", eventType)
		}
	}
	for _, eventType := range []string{"compact_archive", "spill_read", ""} {
		if observability.ObservedEvent(eventType) {
			t.Fatalf("ObservedEvent(%q) = true", eventType)
		}
	}
}

type fakeLog struct {
	events    []rig.SessionEvent
	err       error
	sessionID string
	limit     int
	calls     int
}

func (f *fakeLog) Last(_ context.Context, sessionID string, limit int) ([]rig.SessionEvent, error) {
	f.calls++
	f.sessionID = sessionID
	f.limit = limit
	if f.err != nil {
		return nil, f.err
	}
	return f.events, nil
}
