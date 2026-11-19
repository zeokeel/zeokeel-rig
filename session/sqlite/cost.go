package sqlite

// Session spend, kept. The meter answers "what has this run cost so far"; the
// database answers "what did last week cost" after the process is gone.

import (
	"context"
	"fmt"
	"time"

	"github.com/zeokeel/zeokeel-rig/costmeter"
)

// CostEntry is one settled request's usage and price.
type CostEntry struct {
	// Model is the model that served the request.
	Model string
	// PromptTokens, CompletionTokens and CachedTokens are the provider's counts.
	PromptTokens     int
	CompletionTokens int
	CachedTokens     int
	// CostUSD is the estimated price; 0 means the price table did not know the
	// model (the row is still stored, so the tokens are not lost).
	CostUSD float64
}

// CostEntryFromSummary turns a meter snapshot into a storable entry.
func CostEntryFromSummary(model string, summary costmeter.Summary) CostEntry {
	return CostEntry{
		Model:            model,
		PromptTokens:     summary.PromptTokens,
		CompletionTokens: summary.CompletionTokens,
		CachedTokens:     summary.CachedTokens,
		CostUSD:          summary.CostUSD,
	}
}

// CostFilter narrows a cost query.
type CostFilter struct {
	// SessionID limits the query to one conversation; empty means all.
	SessionID string
	// Since and Until bound the row time; zero values are unbounded.
	Since time.Time
	Until time.Time
}

// CostTotals is the summed usage over the filtered rows.
type CostTotals struct {
	Requests         int
	PromptTokens     int
	CompletionTokens int
	CachedTokens     int
	CostUSD          float64
}

// ModelCost is one model's share.
type ModelCost struct {
	Model string
	CostTotals
}

// SaveCost records one settled request against its session.
func (s *Store) SaveCost(ctx context.Context, sessionID string, entry CostEntry) error {
	if err := s.usable(); err != nil {
		return err
	}
	if sessionID == "" {
		return fmt.Errorf("sqlite: save cost: session id is required")
	}
	now := time.Now().UTC().UnixMilli()
	// A run can settle before its session row exists; the row is created here
	// so the cost is never dropped on a foreign key.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO rig_sessions(id, title, workspace_id, created_at, updated_at)
		 VALUES(?, '', '', ?, ?) ON CONFLICT(id) DO NOTHING`, sessionID, now, now); err != nil {
		return fmt.Errorf("sqlite: save cost: ensure session: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO rig_costs(session_id, model, prompt_tokens, completion_tokens, cached_tokens, cost_usd, at)
		 VALUES(?, ?, ?, ?, ?, ?, ?)`,
		sessionID, entry.Model, entry.PromptTokens, entry.CompletionTokens, entry.CachedTokens, entry.CostUSD, now); err != nil {
		return fmt.Errorf("sqlite: save cost: %w", err)
	}
	return nil
}

// CostTotals sums the rows the filter matches.
func (s *Store) CostTotals(ctx context.Context, filter CostFilter) (CostTotals, error) {
	where, args := costWhere(filter)
	var totals CostTotals
	var requests int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0),
		        COALESCE(SUM(cached_tokens), 0), COALESCE(SUM(cost_usd), 0)
		 FROM rig_costs`+where, args...).
		Scan(&requests, &totals.PromptTokens, &totals.CompletionTokens, &totals.CachedTokens, &totals.CostUSD)
	if err != nil {
		return CostTotals{}, fmt.Errorf("sqlite: cost totals: %w", err)
	}
	totals.Requests = requests
	return totals, nil
}

// CostByModel returns each model's share, most expensive first.
func (s *Store) CostByModel(ctx context.Context, filter CostFilter) ([]ModelCost, error) {
	where, args := costWhere(filter)
	rows, err := s.db.QueryContext(ctx,
		`SELECT model, COUNT(*), COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0),
		        COALESCE(SUM(cached_tokens), 0), COALESCE(SUM(cost_usd), 0)
		 FROM rig_costs`+where+` GROUP BY model ORDER BY SUM(cost_usd) DESC, model`, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: cost by model: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var costs []ModelCost
	for rows.Next() {
		var (
			entry    ModelCost
			requests int
		)
		if err := rows.Scan(&entry.Model, &requests, &entry.PromptTokens, &entry.CompletionTokens,
			&entry.CachedTokens, &entry.CostUSD); err != nil {
			return nil, fmt.Errorf("sqlite: cost by model: %w", err)
		}
		entry.Requests = requests
		costs = append(costs, entry)
	}
	return costs, rows.Err()
}

// costWhere renders the filter's WHERE clause.
func costWhere(filter CostFilter) (string, []any) {
	where := ""
	var args []any
	add := func(clause string, value any) {
		if where == "" {
			where = " WHERE "
		} else {
			where += " AND "
		}
		where += clause
		args = append(args, value)
	}
	if filter.SessionID != "" {
		add("session_id = ?", filter.SessionID)
	}
	if !filter.Since.IsZero() {
		add("at >= ?", filter.Since.UnixMilli())
	}
	if !filter.Until.IsZero() {
		add("at <= ?", filter.Until.UnixMilli())
	}
	return where, args
}
