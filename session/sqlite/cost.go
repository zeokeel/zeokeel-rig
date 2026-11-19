package sqlite

// 会话花费的持久化。内存里的计量表回答"这一轮到现在花了多少"；数据库回答的是
// "上周一共花了多少"——进程退出之后还答得出。

import (
	"context"
	"fmt"
	"time"

	"github.com/zeokeel/zeokeel-rig/costmeter"
)

// CostEntry 是一次已结算请求的用量与价格。
type CostEntry struct {
	// Model 是服务该请求的模型名。
	Model string
	// PromptTokens / CompletionTokens / CachedTokens 是提供方返回的计数。
	PromptTokens     int
	CompletionTokens int
	CachedTokens     int
	// CostUSD 是估算价格；为 0 表示价格表里没有这个模型（这一行仍然会存下来，
	// 用量数据不会因此丢失）。
	CostUSD float64
}

// CostEntryFromSummary 把计量表的一次快照转成可存储的记录。
func CostEntryFromSummary(model string, summary costmeter.Summary) CostEntry {
	return CostEntry{
		Model:            model,
		PromptTokens:     summary.PromptTokens,
		CompletionTokens: summary.CompletionTokens,
		CachedTokens:     summary.CachedTokens,
		CostUSD:          summary.CostUSD,
	}
}

// CostFilter 收窄一次花费查询。
type CostFilter struct {
	// SessionID 限定到某一个会话；空表示全部。
	SessionID string
	// Since / Until 限定记录时间；零值表示不限。
	Since time.Time
	Until time.Time
}

// CostTotals 是过滤后各行用量的合计。
type CostTotals struct {
	Requests         int
	PromptTokens     int
	CompletionTokens int
	CachedTokens     int
	CostUSD          float64
}

// ModelCost 是某一个模型占的份额。
type ModelCost struct {
	Model string
	CostTotals
}

// SaveCost 记录一次已结算请求（归属到它的会话）。
func (s *Store) SaveCost(ctx context.Context, sessionID string, entry CostEntry) error {
	if err := s.usable(); err != nil {
		return err
	}
	if sessionID == "" {
		return fmt.Errorf("sqlite: save cost: session id is required")
	}
	now := time.Now().UTC().UnixMilli()
	// 两条语句都在 retryBusy 里：**补会话行那句也要重试**——它同样是写，锁冲突时
	// 只包住后面那条 INSERT 会让这条便宜路径（补行）成为"花费写不进去"的原因。
	err := retryBusy(ctx, func() error {
		// 一次 run 可能先结算、后建会话行；这里顺手把会话行补出来，避免外键把花费丢掉。
		if _, execErr := s.db.ExecContext(ctx,
			`INSERT INTO rig_sessions(id, title, workspace_id, created_at, updated_at)
			 VALUES(?, '', '', ?, ?) ON CONFLICT(id) DO NOTHING`, sessionID, now, now); execErr != nil {
			return fmt.Errorf("ensure session: %w", execErr)
		}
		_, execErr := s.db.ExecContext(ctx,
			`INSERT INTO rig_costs(session_id, model, prompt_tokens, completion_tokens, cached_tokens, cost_usd, at)
			 VALUES(?, ?, ?, ?, ?, ?, ?)`,
			sessionID, entry.Model, entry.PromptTokens, entry.CompletionTokens, entry.CachedTokens, entry.CostUSD, now)
		return execErr
	})
	if err != nil {
		return fmt.Errorf("sqlite: save cost: %w", err)
	}
	return nil
}

// CostTotals 汇总过滤命中的那些行。
func (s *Store) CostTotals(ctx context.Context, filter CostFilter) (CostTotals, error) {
	// 关闭之后必须返回 ErrClosed：此前漏了这一步，Close 之后再调用会直接 panic。
	if err := s.usable(); err != nil {
		return CostTotals{}, err
	}
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

// CostByModel 返回每个模型占的份额，按花费从高到低。
func (s *Store) CostByModel(ctx context.Context, filter CostFilter) ([]ModelCost, error) {
	// 同 CostTotals：关闭之后必须返回 ErrClosed 而不是 panic。
	if err := s.usable(); err != nil {
		return nil, err
	}
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

// costWhere 渲染过滤条件对应的 WHERE 子句。
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
