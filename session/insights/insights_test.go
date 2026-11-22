package insights

// Tests for the windowed aggregation: the fold rules, day/model/workspace/
// session buckets, the model_call-vs-usage dedupe, bounded reads and
// truncation, malformed/unknown events, the host cost callback, the leaderboard
// limit and the empty log.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	rig "github.com/zeokeel/zeokeel-rig"
)

// memLog is the in-memory EventLog under test.
type memLog struct {
	sessions []SessionInfo
	events   map[string][]rig.SessionEvent

	sessionsErr error
	lastErr     error

	sessionsCalls int
	lastCalls     int
	lastLimit     int
}

func (l *memLog) Sessions(ctx context.Context) ([]SessionInfo, error) {
	l.sessionsCalls++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if l.sessionsErr != nil {
		return nil, l.sessionsErr
	}
	return append([]SessionInfo(nil), l.sessions...), nil
}

func (l *memLog) Last(ctx context.Context, sessionID string, limit int) ([]rig.SessionEvent, error) {
	l.lastCalls++
	l.lastLimit = limit
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if l.lastErr != nil {
		return nil, l.lastErr
	}
	events := l.events[sessionID]
	if limit > 0 && len(events) > limit {
		events = events[len(events)-limit:]
	}
	return append([]rig.SessionEvent(nil), events...), nil
}

// catalogCost prices model-a and model-legacy; every other model is unpriced.
func catalogCost(model string, tokens TokenCount) (float64, bool) {
	switch model {
	case "model-a":
		cached := tokens.Cached
		if cached > tokens.Prompt {
			cached = tokens.Prompt
		}
		if cached < 0 {
			cached = 0
		}
		uncached := tokens.Prompt - cached
		return float64(uncached)/1_000_000*1 +
			float64(cached)/1_000_000*0.5 +
			float64(tokens.Completion)/1_000_000*2, true
	case "model-legacy":
		return float64(tokens.Prompt)/1_000_000*0.5 +
			float64(tokens.Completion)/1_000_000*1, true
	default:
		return 0, false
	}
}

func shanghai(t *testing.T) *time.Location {
	t.Helper()
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load Asia/Shanghai: %v", err)
	}
	return location
}

func modelCallEvent(at time.Time, model string, prompt, cached, completion, total int) rig.SessionEvent {
	payload, _ := json.Marshal(map[string]any{
		"iteration":        1,
		"model":            model,
		"promptTokens":     prompt,
		"cachedTokens":     cached,
		"completionTokens": completion,
		"totalTokens":      total,
		"contextTokens":    prompt,
	})
	return rig.SessionEvent{Type: EventModelCall, Payload: payload, Time: at}
}

func usageEvent(at time.Time, prompt, completion, cached, total, iterations int) rig.SessionEvent {
	payload, _ := json.Marshal(map[string]any{
		"promptTokens":     prompt,
		"completionTokens": completion,
		"totalTokens":      total,
		"cachedTokens":     cached,
		"iterations":       iterations,
	})
	return rig.SessionEvent{Type: EventUsage, Payload: payload, Time: at}
}

func runStartEvent(at time.Time, model string) rig.SessionEvent {
	payload, _ := json.Marshal(map[string]any{"model": model})
	return rig.SessionEvent{Type: EventRunStart, Payload: payload, Time: at}
}

func plainEvent(at time.Time, eventType string) rig.SessionEvent {
	return rig.SessionEvent{Type: eventType, Payload: []byte(`{}`), Time: at}
}

func approx(t *testing.T, want, got float64) {
	t.Helper()
	if diff := want - got; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("cost = %v, want %v", got, want)
	}
}

func windowOptions(location *time.Location, now time.Time) Options {
	return Options{
		Days:     7,
		Location: location,
		Now:      func() time.Time { return now },
		Cost:     catalogCost,
	}
}

func TestFoldWindowTotalsAndDimensions(t *testing.T) {
	location := shanghai(t)
	at15 := func(hour int) time.Time { return time.Date(2026, 11, 15, hour, 0, 0, 0, location) }
	at14 := func(hour int) time.Time { return time.Date(2026, 11, 14, hour, 0, 0, 0, location) }
	at13 := func(hour int) time.Time { return time.Date(2026, 11, 13, hour, 0, 0, 0, location) }

	log := &memLog{
		sessions: []SessionInfo{
			{ID: "s1", Title: "发布检查", WorkspaceID: "ws1", UpdatedAt: at15(9)},
			{ID: "s2", Title: "旧版会话", WorkspaceID: "", UpdatedAt: at14(23)},
			{ID: "s3", Title: "无价模型会话", WorkspaceID: "ws2", UpdatedAt: at13(12)},
		},
		events: map[string][]rig.SessionEvent{
			"s1": {
				runStartEvent(at15(9), "model-a"),
				modelCallEvent(at15(9).Add(time.Minute), "model-a", 1000, 400, 200, 1200),
				modelCallEvent(at15(9).Add(2*time.Minute), "model-a", 500, 0, 100, 600),
				plainEvent(at15(9).Add(3*time.Minute), EventToolCall),
				// The turn's cumulative usage event: tokens come from the
				// model_call events above, so this only bumps turns.
				usageEvent(at15(9).Add(4*time.Minute), 1500, 300, 400, 1800, 2),
				plainEvent(at15(9).Add(5*time.Minute), EventCompact),
			},
			"s2": {
				runStartEvent(at14(23), "model-legacy"),
				usageEvent(at14(23).Add(time.Minute), 3000, 400, 0, 3400, 2),
				plainEvent(at14(23).Add(2*time.Minute), EventToolCall),
			},
			"s3": {
				modelCallEvent(at13(12), "model-b", 100, 0, 50, 150),
			},
		},
	}

	result, err := Fold(context.Background(), log, windowOptions(location, at15(10)))
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if result.Days != 7 || len(result.Daily) != 7 {
		t.Fatalf("Days = %d, daily = %d, want 7/7", result.Days, len(result.Daily))
	}
	if want := time.Date(2026, 11, 9, 0, 0, 0, 0, location); !result.From.Equal(want) {
		t.Fatalf("From = %v, want %v", result.From, want)
	}
	if want := time.Date(2026, 11, 16, 0, 0, 0, 0, location); !result.To.Equal(want) {
		t.Fatalf("To = %v, want %v", result.To, want)
	}
	if !result.GeneratedAt.Equal(at15(10)) {
		t.Fatalf("GeneratedAt = %v, want the injected clock", result.GeneratedAt)
	}

	totals := result.Totals
	checks := []struct {
		name string
		got  int
		want int
	}{
		{"turns", totals.Turns, 2},
		{"steps", totals.Steps, 5}, // 2 model_call + 2 legacy iterations + 1 model_call
		{"prompt", totals.Tokens.Prompt, 4600},
		{"completion", totals.Tokens.Completion, 750},
		{"cached", totals.Tokens.Cached, 400},
		{"total", totals.Tokens.Total, 5350},
		{"toolCalls", totals.ToolCalls, 2},
		{"sessions", totals.Sessions, 3},
		{"compactions", totals.Compactions, 1},
		{"pricedCalls", totals.PricedCalls, 3},
		{"unpricedCalls", totals.UnpricedCalls, 1},
	}
	for _, check := range checks {
		if check.got != check.want {
			t.Fatalf("totals.%s = %d, want %d", check.name, check.got, check.want)
		}
	}
	if totals.CostKnown {
		t.Fatal("totals.CostKnown = true with an unpriced model in the window")
	}
	// s1: 600 uncached·1 + 400 cached·0.5 + 200 out·2 = 0.0012; then
	// 500·1 + 0 + 100·2 = 0.0007. s2: 3000·0.5 + 400·1 = 0.0019.
	approx(t, 0.0038, totals.CostUSD)

	if result.Daily[6].Day.Format("2006-01-02") != "2026-11-15" {
		t.Fatalf("last day = %v, want 2026-11-15", result.Daily[6].Day)
	}
	if result.Daily[0].Day.Format("2006-01-02") != "2026-11-09" {
		t.Fatalf("first day = %v, want 2026-11-09", result.Daily[0].Day)
	}
	day15 := result.Daily[6]
	if day15.Tokens.Total != 1800 || day15.Turns != 1 || day15.Steps != 2 ||
		day15.ToolCalls != 1 || day15.Sessions != 1 || !day15.CostKnown {
		t.Fatalf("day 11-15 = %+v, want 1800 tokens/1 turn/2 steps/1 tool/1 session priced", day15)
	}
	approx(t, 0.0019, day15.CostUSD)
	day14 := result.Daily[5]
	if day14.Tokens.Total != 3400 || day14.Sessions != 1 || !day14.CostKnown {
		t.Fatalf("day 11-14 = %+v, want 3400 tokens/1 session priced", day14)
	}
	approx(t, 0.0019, day14.CostUSD)
	day13 := result.Daily[4]
	if day13.Tokens.Total != 150 || day13.Sessions != 1 || day13.CostKnown {
		t.Fatalf("day 11-13 = %+v, want 150 tokens/1 session/unpriced", day13)
	}
	if result.Daily[0].Tokens.Total != 0 || result.Daily[0].Sessions != 0 {
		t.Fatalf("empty day = %+v, want zeros", result.Daily[0])
	}

	if len(result.Models) != 3 {
		t.Fatalf("models = %+v, want 3 rows", result.Models)
	}
	models := map[string]Model{}
	for _, row := range result.Models {
		models[row.Model] = row
	}
	if row := models["model-a"]; row.Calls != 2 || row.Tokens.Total != 1800 || !row.CostKnown {
		t.Fatalf("model-a row = %+v, want 2 calls/1800 tokens/priced", row)
	}
	if row := models["model-legacy"]; row.Calls != 2 || row.Tokens.Total != 3400 || !row.CostKnown {
		t.Fatalf("model-legacy row = %+v, want 2 calls/3400 tokens/priced (usage iterations)", row)
	}
	if row := models["model-b"]; row.CostKnown || row.CostUSD != 0 {
		t.Fatalf("model-b row = %+v, want no cost", row)
	}
	if result.Models[0].Model != "model-legacy" {
		t.Fatalf("model order = %+v, want model-legacy first", result.Models)
	}

	if len(result.Workspaces) != 3 {
		t.Fatalf("workspaces = %+v, want 3 rows", result.Workspaces)
	}
	workspaces := map[string]Workspace{}
	for _, row := range result.Workspaces {
		workspaces[row.WorkspaceID] = row
	}
	if row := workspaces["ws1"]; row.Sessions != 1 || row.Tokens.Total != 1800 || !row.CostKnown {
		t.Fatalf("workspace ws1 = %+v", row)
	}
	if row := workspaces["ws2"]; row.Sessions != 1 || row.Tokens.Total != 150 || row.CostKnown {
		t.Fatalf("workspace ws2 = %+v", row)
	}
	if row := workspaces[""]; row.Sessions != 1 || row.Tokens.Total != 3400 || !row.CostKnown {
		t.Fatalf("unassociated workspace = %+v", row)
	}
	if result.Workspaces[0].WorkspaceID != "" {
		t.Fatalf("workspace order = %+v, want the 3400-token workspace first", result.Workspaces)
	}

	if len(result.Sessions) != 3 {
		t.Fatalf("sessions = %+v, want 3 rows", result.Sessions)
	}
	if result.Sessions[0].SessionID != "s2" || result.Sessions[0].Model != "model-legacy" ||
		result.Sessions[0].Tokens.Total != 3400 || result.Sessions[0].Title != "旧版会话" {
		t.Fatalf("leaderboard[0] = %+v, want s2/model-legacy/3400", result.Sessions[0])
	}
	if s1 := result.Sessions[1]; s1.SessionID != "s1" || s1.Model != "model-a" ||
		s1.Turns != 1 || s1.Steps != 2 || s1.ToolCalls != 1 || s1.Tokens.Total != 1800 {
		t.Fatalf("leaderboard[1] = %+v, want s1/model-a/1 turn/2 steps/1 tool/1800", s1)
	}
	if !result.Sessions[1].CostKnown {
		t.Fatal("s1 should be fully priced")
	}
	if result.Sessions[2].SessionID != "s3" || result.Sessions[2].CostKnown {
		t.Fatalf("leaderboard[2] = %+v, want s3 unpriced", result.Sessions[2])
	}
	if result.Truncated {
		t.Fatal("Truncated = true without hitting the read bound")
	}
}

func TestWindowBoundariesAreLocalMidnights(t *testing.T) {
	location := shanghai(t)
	at := func(day, hour, minute, second int) time.Time {
		return time.Date(2026, 11, day, hour, minute, second, 0, location)
	}
	log := &memLog{
		sessions: []SessionInfo{{ID: "s1", UpdatedAt: at(15, 10, 0, 0)}},
		events: map[string][]rig.SessionEvent{
			"s1": {
				modelCallEvent(at(8, 23, 59, 59), "model-a", 1, 0, 0, 1), // before the window
				modelCallEvent(at(9, 0, 0, 0), "model-a", 10, 0, 0, 10),  // first in-window instant
				modelCallEvent(at(15, 23, 59, 59), "model-a", 100, 0, 0, 100),
				modelCallEvent(at(16, 0, 0, 0), "model-a", 1000, 0, 0, 1000), // after today
			},
		},
	}
	result, err := Fold(context.Background(), log, windowOptions(location, at(15, 10, 0, 0)))
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if got, want := result.Totals.Tokens.Total, 110; got != want {
		t.Fatalf("TotalTokens = %d, want %d (only the in-window events)", got, want)
	}
	if got, want := result.Totals.Steps, 2; got != want {
		t.Fatalf("Steps = %d, want %d", got, want)
	}
	if got, want := result.Daily[0].Tokens.Total, 10; got != want {
		t.Fatalf("first day tokens = %d, want %d (the 00:00 event)", got, want)
	}
	if got, want := result.Daily[6].Tokens.Total, 100; got != want {
		t.Fatalf("last day tokens = %d, want %d (23:59:59 stays on its day)", got, want)
	}
}

func TestNoDoubleCountWhenModelCallsAndUsageCoexist(t *testing.T) {
	location := shanghai(t)
	when := time.Date(2026, 11, 15, 9, 0, 0, 0, location)
	log := &memLog{
		sessions: []SessionInfo{{ID: "s1", UpdatedAt: when}},
		events: map[string][]rig.SessionEvent{
			"s1": {
				runStartEvent(when, "model-a"),
				modelCallEvent(when, "model-a", 100, 0, 10, 110),
				// The same turn's cumulative usage event would double count.
				usageEvent(when.Add(time.Minute), 100, 10, 0, 110, 1),
			},
		},
	}
	result, err := Fold(context.Background(), log, windowOptions(location, when))
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if result.Totals.Tokens.Prompt != 100 || result.Totals.Tokens.Completion != 10 || result.Totals.Tokens.Total != 110 {
		t.Fatalf("totals = %+v, want the model_call tokens only", result.Totals)
	}
	if result.Totals.Turns != 1 || result.Totals.Steps != 1 {
		t.Fatalf("turns/steps = %d/%d, want 1/1", result.Totals.Turns, result.Totals.Steps)
	}
}

func TestUnpricedModelNeverReportsCost(t *testing.T) {
	location := shanghai(t)
	when := time.Date(2026, 11, 15, 9, 0, 0, 0, location)
	log := &memLog{
		sessions: []SessionInfo{{ID: "s1", UpdatedAt: when}},
		events: map[string][]rig.SessionEvent{
			"s1": {modelCallEvent(when, "unpriced", 1_000_000, 0, 500_000, 1_500_000)},
		},
	}
	result, err := Fold(context.Background(), log, windowOptions(location, when))
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if result.Totals.CostKnown || result.Totals.CostUSD != 0 || result.Totals.PricedCalls != 0 ||
		result.Totals.UnpricedCalls != 1 {
		t.Fatalf("totals = %+v, want unpriced/zero cost", result.Totals)
	}
	if result.Models[0].CostKnown || result.Models[0].CostUSD != 0 ||
		result.Sessions[0].CostKnown || result.Daily[6].CostKnown {
		t.Fatalf("unpriced rows invented a cost: %+v / %+v / %+v", result.Models[0], result.Sessions[0], result.Daily[6])
	}
}

func TestNilCostCallbackMarksEverythingUnpriced(t *testing.T) {
	location := shanghai(t)
	when := time.Date(2026, 11, 15, 9, 0, 0, 0, location)
	log := &memLog{
		sessions: []SessionInfo{{ID: "s1", UpdatedAt: when}},
		events: map[string][]rig.SessionEvent{
			"s1": {modelCallEvent(when, "model-a", 10, 0, 5, 15)},
		},
	}
	result, err := Fold(context.Background(), log, Options{
		Location: location,
		Now:      func() time.Time { return when },
	})
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if result.Totals.CostKnown || result.Totals.PricedCalls != 0 ||
		result.Totals.UnpricedCalls != 1 || result.Totals.CostUSD != 0 {
		t.Fatalf("totals = %+v, want one unpriced call without a callback", result.Totals)
	}
}

func TestEmptyLogIsAnHonestEmptyWindow(t *testing.T) {
	location := shanghai(t)
	now := time.Date(2026, 11, 15, 10, 0, 0, 0, location)
	result, err := Fold(context.Background(), &memLog{}, Options{
		Location: location,
		Now:      func() time.Time { return now },
		Cost:     catalogCost,
	})
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if result.Days != DefaultDays || len(result.Daily) != DefaultDays {
		t.Fatalf("default window = %d/%d, want %d/%d", result.Days, len(result.Daily), DefaultDays, DefaultDays)
	}
	if want := time.Date(2026, 11, 9, 0, 0, 0, 0, location); !result.From.Equal(want) {
		t.Fatalf("From = %v, want %v", result.From, want)
	}
	if want := time.Date(2026, 11, 16, 0, 0, 0, 0, location); !result.To.Equal(want) {
		t.Fatalf("To = %v, want %v", result.To, want)
	}
	if result.Totals.Sessions != 0 || result.Totals.Tokens.Total != 0 || result.Totals.CostKnown {
		t.Fatalf("empty totals = %+v", result.Totals)
	}
	if len(result.Models) != 0 || len(result.Workspaces) != 0 || len(result.Sessions) != 0 {
		t.Fatalf("empty rows: models %v, workspaces %v, sessions %v",
			result.Models, result.Workspaces, result.Sessions)
	}
	if result.Models == nil || result.Workspaces == nil || result.Sessions == nil {
		t.Fatal("empty rows must be empty slices, not nil")
	}
	for index, day := range result.Daily {
		want := time.Date(2026, 11, 9+index, 0, 0, 0, 0, location)
		if !day.Day.Equal(want) || day.Tokens.Total != 0 || day.Sessions != 0 || day.CostKnown {
			t.Fatalf("empty day %d = %+v, want %v zeros", index, day, want)
		}
	}
}

func TestSessionsOutsideWindowAreNotRead(t *testing.T) {
	location := shanghai(t)
	old := time.Date(2026, 10, 1, 9, 0, 0, 0, location)
	log := &memLog{
		sessions: []SessionInfo{{ID: "old", UpdatedAt: old}},
		events:   map[string][]rig.SessionEvent{"old": {modelCallEvent(old, "model-a", 1, 0, 0, 1)}},
	}
	now := time.Date(2026, 11, 15, 10, 0, 0, 0, location)
	if _, err := Fold(context.Background(), log, Options{
		Days: 7, Location: location, Now: func() time.Time { return now },
	}); err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if log.lastCalls != 0 {
		t.Fatalf("Last calls = %d, want 0 (an out-of-window session is not read)", log.lastCalls)
	}
}

func TestSessionWithZeroUpdatedAtIsRead(t *testing.T) {
	location := shanghai(t)
	when := time.Date(2026, 11, 15, 9, 0, 0, 0, location)
	log := &memLog{
		sessions: []SessionInfo{{ID: "unknown-updated"}},
		events:   map[string][]rig.SessionEvent{"unknown-updated": {modelCallEvent(when, "model-a", 1, 0, 0, 1)}},
	}
	result, err := Fold(context.Background(), log, Options{
		Location: location, Now: func() time.Time { return when },
	})
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if log.lastCalls != 1 || result.Totals.Steps != 1 {
		t.Fatalf("zero-updated session: Last=%d steps=%d, want 1/1", log.lastCalls, result.Totals.Steps)
	}
}

func TestTruncatedReadIsReported(t *testing.T) {
	location := shanghai(t)
	mk := func(hour int) time.Time { return time.Date(2026, 11, 15, hour, 0, 0, 0, location) }
	log := &memLog{
		sessions: []SessionInfo{{ID: "s1", UpdatedAt: mk(12)}},
		events: map[string][]rig.SessionEvent{
			"s1": {
				modelCallEvent(mk(8), "model-a", 1, 0, 0, 1),
				modelCallEvent(mk(9), "model-a", 1, 0, 0, 1),
				modelCallEvent(mk(10), "model-a", 1, 0, 0, 1),
			},
		},
	}
	result, err := Fold(context.Background(), log, Options{
		Days:             7,
		EventsPerSession: 2,
		Location:         location,
		Now:              func() time.Time { return mk(12) },
	})
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if !result.Truncated || result.TruncatedSessions != 1 {
		t.Fatalf("truncated = %v/%d, want true/1", result.Truncated, result.TruncatedSessions)
	}
	if result.Totals.Steps != 2 {
		t.Fatalf("steps = %d, want the 2 newest events", result.Totals.Steps)
	}
	if log.lastLimit != 2 {
		t.Fatalf("Last limit = %d, want 2", log.lastLimit)
	}
}

func TestBoundHitOutsideWindowIsNotTruncation(t *testing.T) {
	location := shanghai(t)
	from := time.Date(2026, 11, 9, 0, 0, 0, 0, location)
	old := from.Add(-time.Hour)
	newer := time.Date(2026, 11, 15, 9, 0, 0, 0, location)
	log := &memLog{
		sessions: []SessionInfo{{ID: "s1", UpdatedAt: newer}},
		events: map[string][]rig.SessionEvent{
			"s1": {
				modelCallEvent(old, "model-a", 1, 0, 0, 1),
				modelCallEvent(newer, "model-a", 10, 0, 0, 10),
				modelCallEvent(newer.Add(time.Minute), "model-a", 100, 0, 0, 100),
			},
		},
	}
	result, err := Fold(context.Background(), log, Options{
		Days:             1,
		EventsPerSession: 3,
		Location:         location,
		Now:              func() time.Time { return newer.Add(time.Hour) },
	})
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if result.Truncated || result.TruncatedSessions != 0 {
		t.Fatalf("truncated = %v/%d, want false/0: the oldest read event is outside the window",
			result.Truncated, result.TruncatedSessions)
	}
	if result.Totals.Steps != 2 || result.Totals.Tokens.Total != 110 {
		t.Fatalf("steps/tokens = %d/%d, want 2/110", result.Totals.Steps, result.Totals.Tokens.Total)
	}
}

func TestMalformedAndUnknownEventsAreSkipped(t *testing.T) {
	location := shanghai(t)
	when := time.Date(2026, 11, 15, 9, 0, 0, 0, location)
	log := &memLog{
		sessions: []SessionInfo{
			{ID: "s1", UpdatedAt: when},
			{ID: "s2", UpdatedAt: when},
		},
		events: map[string][]rig.SessionEvent{
			"s1": {
				{Type: EventRunStart, Time: when, Payload: []byte(`{"model":123}`)},
				{Type: EventUsage, Time: when.Add(3 * time.Minute), Payload: nil},
				{Type: EventUsage, Time: when.Add(4 * time.Minute), Payload: []byte(`{}`)},
				{Type: EventToolCall, Time: when.Add(5 * time.Minute), Payload: []byte(`{"x":`)},
				{Type: EventCompact, Time: when.Add(6 * time.Minute)},
				{Type: EventSpill, Time: when.Add(7 * time.Minute)},
				{Type: "run_end", Time: when.Add(8 * time.Minute), Payload: []byte(`{}`)},
				{Type: "", Time: when.Add(9 * time.Minute), Payload: []byte(`{}`)},
			},
			// A session whose only model_calls are malformed contributes
			// nothing (no step, no token, no active session).
			"s2": {
				{Type: EventModelCall, Time: when.Add(time.Minute), Payload: []byte(`{not json`)},
				{Type: EventModelCall, Time: when.Add(2 * time.Minute), Payload: []byte(`{"promptTokens":"many"}`)},
			},
		},
	}
	result, err := Fold(context.Background(), log, windowOptions(location, when))
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if result.Totals.Turns != 1 || result.Totals.Steps != 1 {
		t.Fatalf("turns/steps = %d/%d, want 1/1 (only the decodable usage event)",
			result.Totals.Turns, result.Totals.Steps)
	}
	if result.Totals.ToolCalls != 1 || result.Totals.Compactions != 1 || result.Totals.Spills != 1 {
		t.Fatalf("audit counts = %+v, want 1/1/1", result.Totals)
	}
	if result.Totals.Tokens.Total != 0 || result.Totals.PricedCalls != 0 || result.Totals.UnpricedCalls != 0 {
		t.Fatalf("malformed events contributed tokens: %+v", result.Totals)
	}
	if len(result.Models) != 1 || result.Models[0].Model != "" || result.Models[0].Calls != 1 {
		t.Fatalf("models = %+v, want the single unattributed legacy call", result.Models)
	}
	if len(result.Sessions) != 1 || result.Sessions[0].SessionID != "s1" ||
		result.Sessions[0].Turns != 1 || result.Sessions[0].Steps != 1 {
		t.Fatalf("sessions = %+v, want only s1 with the usage turn", result.Sessions)
	}
	if result.Totals.Sessions != 1 {
		t.Fatalf("active sessions = %d, want 1 (s2 had no decodable event)", result.Totals.Sessions)
	}
	if result.Truncated {
		t.Fatal("Truncated = true without a bound hit")
	}
}

func TestCostCallbackReceivesClampedTokens(t *testing.T) {
	location := shanghai(t)
	when := time.Date(2026, 11, 15, 9, 0, 0, 0, location)
	type call struct {
		model  string
		tokens TokenCount
	}
	var calls []call
	cost := func(model string, tokens TokenCount) (float64, bool) {
		calls = append(calls, call{model: model, tokens: tokens})
		if model != "model-a" {
			return 0, false
		}
		return float64(tokens.Prompt)/1_000_000*1 + float64(tokens.Completion)/1_000_000*2, true
	}
	log := &memLog{
		sessions: []SessionInfo{{ID: "s1", UpdatedAt: when}},
		events: map[string][]rig.SessionEvent{
			"s1": {
				// All-negative sample clamps to zero and is not counted as a call.
				modelCallEvent(when, "model-a", -5, -2, -3, -10),
				// Missing total falls back to prompt+completion and is priced.
				modelCallEvent(when.Add(time.Minute), "model-a", 1_000_000, 0, 0, 0),
				// Unknown model is unpriced.
				modelCallEvent(when.Add(2*time.Minute), "unknown", 10, 0, 5, 15),
			},
		},
	}
	result, err := Fold(context.Background(), log, Options{
		Days:     7,
		Location: location,
		Now:      func() time.Time { return when },
		Cost:     cost,
	})
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if len(calls) != 3 {
		t.Fatalf("cost callback calls = %d, want 3 token-bearing contributions", len(calls))
	}
	if calls[0].tokens != (TokenCount{}) {
		t.Fatalf("negative sample reached the callback as %+v, want zeros", calls[0].tokens)
	}
	if calls[1].tokens.Prompt != 1_000_000 || calls[1].tokens.Total != 1_000_000 ||
		calls[1].tokens.Completion != 0 {
		t.Fatalf("second sample = %+v, want the prompt-fallback total", calls[1].tokens)
	}
	if calls[2].model != "unknown" || calls[2].tokens.Prompt != 10 || calls[2].tokens.Completion != 5 {
		t.Fatalf("third sample = %+v, want the unknown model's tokens", calls[2])
	}
	if result.Totals.PricedCalls != 1 || result.Totals.UnpricedCalls != 1 {
		t.Fatalf("priced/unpriced = %d/%d, want 1/1 (zero-token call is not counted)",
			result.Totals.PricedCalls, result.Totals.UnpricedCalls)
	}
	if !approxEqual(result.Totals.CostUSD, 1.0) || result.Totals.CostKnown {
		t.Fatalf("totals cost = %v known=%v, want 1.0/false", result.Totals.CostUSD, result.Totals.CostKnown)
	}
}

func TestModelAttributionFallsBackToRunAndPriorEvents(t *testing.T) {
	location := shanghai(t)
	when := time.Date(2026, 11, 15, 9, 0, 0, 0, location)
	previousWeek := time.Date(2026, 11, 8, 9, 0, 0, 0, location)
	log := &memLog{
		sessions: []SessionInfo{
			{ID: "s1", UpdatedAt: when},
			{ID: "s2", UpdatedAt: when},
		},
		events: map[string][]rig.SessionEvent{
			"s1": {
				runStartEvent(when, "model-a"),
				{Type: EventModelCall, Time: when.Add(time.Minute),
					Payload: []byte(`{"promptTokens":10,"completionTokens":5,"totalTokens":15}`)},
				runStartEvent(when.Add(2*time.Minute), "model-b"),
				{Type: EventModelCall, Time: when.Add(3 * time.Minute),
					Payload: []byte(`{"promptTokens":20,"completionTokens":10,"totalTokens":30}`)},
			},
			"s2": {
				// The only model mention is outside the window; a usage-only
				// session still attributes its turn to it.
				modelCallEvent(previousWeek, "model-legacy", 1, 0, 0, 1),
				usageEvent(when, 100, 50, 0, 150, 1),
			},
		},
	}
	result, err := Fold(context.Background(), log, windowOptions(location, when))
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	models := map[string]Model{}
	for _, row := range result.Models {
		models[row.Model] = row
	}
	if row, ok := models["model-a"]; !ok || row.Calls != 1 {
		t.Fatalf("model-a row = %+v, want the first call attributed by run_start", row)
	}
	if row, ok := models["model-b"]; !ok || row.Calls != 1 {
		t.Fatalf("model-b row = %+v, want the second call attributed by the new run_start", row)
	}
	if row, ok := models["model-legacy"]; !ok || row.Calls != 1 || row.Tokens.Total != 150 {
		t.Fatalf("model-legacy row = %+v, want the out-of-window fallback attribution", row)
	}
	sessions := map[string]Session{}
	for _, row := range result.Sessions {
		sessions[row.SessionID] = row
	}
	if sessions["s1"].Model != "model-a" || sessions["s2"].Model != "model-legacy" {
		t.Fatalf("session models = %q/%q, want model-a/model-legacy (model_calls tie breaks by id)",
			sessions["s1"].Model, sessions["s2"].Model)
	}
}

func TestLeaderboardLimitAndClamps(t *testing.T) {
	location := shanghai(t)
	when := time.Date(2026, 11, 15, 9, 0, 0, 0, location)
	log := &memLog{
		sessions: []SessionInfo{
			{ID: "small", UpdatedAt: when},
			{ID: "large", UpdatedAt: when},
			{ID: "medium", UpdatedAt: when},
		},
		events: map[string][]rig.SessionEvent{
			"small":  {modelCallEvent(when, "model-a", 10, 0, 0, 10)},
			"large":  {modelCallEvent(when, "model-a", 1000, 0, 0, 1000)},
			"medium": {modelCallEvent(when, "model-a", 100, 0, 0, 100)},
		},
	}
	result, err := Fold(context.Background(), log, Options{
		Days: 1000, SessionLimit: 2, Location: location,
		Now: func() time.Time { return when },
	})
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if result.Days != MaxDays || len(result.Daily) != MaxDays {
		t.Fatalf("Days/daily = %d/%d, want %d/%d", result.Days, len(result.Daily), MaxDays, MaxDays)
	}
	if len(result.Sessions) != 2 {
		t.Fatalf("leaderboard = %+v, want 2 rows", result.Sessions)
	}
	if result.Sessions[0].SessionID != "large" || result.Sessions[1].SessionID != "medium" {
		t.Fatalf("leaderboard order = %+v, want large then medium", result.Sessions)
	}
	if result.Totals.Sessions != 3 {
		t.Fatalf("active sessions = %d, want 3 (the leaderboard is bounded, the total is not)",
			result.Totals.Sessions)
	}
}

func TestZeroTokenCallHasNoCostKnowledge(t *testing.T) {
	location := shanghai(t)
	when := time.Date(2026, 11, 15, 9, 0, 0, 0, location)
	log := &memLog{
		sessions: []SessionInfo{{ID: "s1", UpdatedAt: when}},
		events:   map[string][]rig.SessionEvent{"s1": {modelCallEvent(when, "model-a", 0, 0, 0, 0)}},
	}
	result, err := Fold(context.Background(), log, windowOptions(location, when))
	if err != nil {
		t.Fatalf("Fold: %v", err)
	}
	if result.Totals.Steps != 1 || result.Totals.PricedCalls != 0 || result.Totals.UnpricedCalls != 0 {
		t.Fatalf("totals = %+v, want one zero-token step and no priced/unpriced call", result.Totals)
	}
	if result.Totals.CostKnown {
		t.Fatal("a zero-token call must not claim cost knowledge")
	}
	if len(result.Models) != 1 || result.Models[0].CostKnown {
		t.Fatalf("models = %+v, want one model row without cost knowledge", result.Models)
	}
}

func TestErrorsAndNilInputs(t *testing.T) {
	if _, err := Fold(context.Background(), nil, Options{}); !errors.Is(err, ErrNoLog) {
		t.Fatalf("nil log error = %v, want ErrNoLog", err)
	}
	sessionsErr := errors.New("sessions broke")
	log := &memLog{sessionsErr: sessionsErr}
	if _, err := Fold(context.Background(), log, Options{}); !errors.Is(err, sessionsErr) {
		t.Fatalf("sessions error = %v, want wrapped sentinel", err)
	}
	lastErr := errors.New("last broke")
	log = &memLog{
		sessions: []SessionInfo{{ID: "s1"}},
		lastErr:  lastErr,
	}
	if _, err := Fold(context.Background(), log, Options{}); !errors.Is(err, lastErr) {
		t.Fatalf("last error = %v, want wrapped sentinel", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	log = &memLog{}
	if _, err := Fold(canceled, log, Options{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled ctx error = %v, want context.Canceled", err)
	}
	if log.sessionsCalls != 0 {
		t.Fatalf("Sessions called %d times after cancellation, want 0", log.sessionsCalls)
	}
	// A nil context means background.
	if _, err := Fold(context.TODO(), &memLog{}, Options{}); err != nil { //nolint:staticcheck
		t.Fatalf("Fold(nil ctx): %v", err)
	}
}

func approxEqual(got, want float64) bool {
	diff := got - want
	return diff < 1e-9 && diff > -1e-9
}
