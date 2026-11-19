package sqlite_test

// What a session cost, kept: the meter is in memory for the run, the database is
// what the host shows the morning after a restart.

import (
	"context"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/costmeter"
	sqlitestore "github.com/zeokeel/zeokeel-rig/session/sqlite"
)

// TestSaveCostAccumulatesPerSession covers the ordinary write: every settled
// request adds to the session's running total.
func TestSaveCostAccumulatesPerSession(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	created, err := db.Create(ctx, sqlitestore.Meta{Title: "花费"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := db.SaveCost(ctx, created.ID, sqlitestore.CostEntry{
		Model: "gpt-x", PromptTokens: 100, CompletionTokens: 20, CachedTokens: 10, CostUSD: 0.02,
	}); err != nil {
		t.Fatalf("SaveCost: %v", err)
	}
	if err := db.SaveCost(ctx, created.ID, sqlitestore.CostEntry{
		Model: "gpt-x", PromptTokens: 50, CompletionTokens: 10, CostUSD: 0.01,
	}); err != nil {
		t.Fatalf("SaveCost second: %v", err)
	}
	total, err := db.CostTotals(ctx, sqlitestore.CostFilter{SessionID: created.ID})
	if err != nil {
		t.Fatalf("CostTotals: %v", err)
	}
	if total.Requests != 2 || total.PromptTokens != 150 || total.CompletionTokens != 30 || total.CachedTokens != 10 {
		t.Fatalf("totals = %+v", total)
	}
	if total.CostUSD < 0.0299 || total.CostUSD > 0.0301 {
		t.Fatalf("cost = %v, want 0.03", total.CostUSD)
	}
}

// TestCostTotalsByModelAndWindow covers the insights page: which model spent
// what, over a date range.
func TestCostTotalsByModelAndWindow(t *testing.T) {
	db, _ := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	created, err := db.Create(ctx, sqlitestore.Meta{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, model := range []string{"a", "a", "b"} {
		if err := db.SaveCost(ctx, created.ID, sqlitestore.CostEntry{Model: model, PromptTokens: 10, CostUSD: 0.001}); err != nil {
			t.Fatalf("SaveCost: %v", err)
		}
	}
	byModel, err := db.CostByModel(ctx, sqlitestore.CostFilter{})
	if err != nil {
		t.Fatalf("CostByModel: %v", err)
	}
	if len(byModel) != 2 || byModel[0].Model != "a" || byModel[0].Requests != 2 {
		t.Fatalf("by model = %+v", byModel)
	}
	if none, err := db.CostTotals(ctx, sqlitestore.CostFilter{Since: farFuture()}); err != nil || none.Requests != 0 {
		t.Fatalf("future window = %+v (err %v)", none, err)
	}
}

// TestCostSurvivesReopen is the point of storing it at all.
func TestCostSurvivesReopen(t *testing.T) {
	db, path := openTemp(t, sqlitestore.Options{})
	ctx := context.Background()
	created, err := db.Create(ctx, sqlitestore.Meta{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := db.SaveCost(ctx, created.ID, sqlitestore.CostEntry{Model: "m", PromptTokens: 5, CostUSD: 0.5}); err != nil {
		t.Fatalf("SaveCost: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	again, err := sqlitestore.Open(sqlitestore.Options{Path: path})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = again.Close() }()
	total, err := again.CostTotals(ctx, sqlitestore.CostFilter{})
	if err != nil {
		t.Fatalf("CostTotals: %v", err)
	}
	if total.CostUSD != 0.5 {
		t.Fatalf("cost after reopen = %v, want 0.5", total.CostUSD)
	}
}

// TestMeterSummaryConverts keeps the meter's own type usable without a
// dependency cycle.
func TestMeterSummaryConverts(t *testing.T) {
	summary := costmeter.Summary{Requests: 2, PromptTokens: 9, CompletionTokens: 3, CachedTokens: 1, CostUSD: 0.25}
	entry := sqlitestore.CostEntryFromSummary("m", summary)
	if entry.PromptTokens != 9 || entry.CompletionTokens != 3 || entry.CachedTokens != 1 || entry.CostUSD != 0.25 {
		t.Fatalf("entry = %+v", entry)
	}
}

func farFuture() time.Time { return time.Now().Add(240 * time.Hour) }
