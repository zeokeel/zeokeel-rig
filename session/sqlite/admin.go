package sqlite

// 本文件是会话的"管理面"：删掉历史里的某几条消息、按条数截断历史、给一个会话做
// 体检（条数/用量/载荷字节/检查点），以及删除检查点。
//
// 三条共同规矩：
//   - **一个事务**：消息删除与 `rig_sessions` 的计数/时间戳必须一起落下，否则进程
//     在中间崩溃会留下"消息没了、message_count 还是旧值"的会话；
//   - **幂等**：删不存在的序号、重复删同一批、keep 大于总数，都返回 0 行而不是错误；
//   - **未知会话**复用 `Load` 的哨兵 `ErrNotFound`（宿主换存储实现时 `errors.Is`
//     照常成立），关闭之后复用 `ErrClosed`。
//
// 删除**不重排 ordinal**（允许留空洞）：序号是宿主手里已经拿到的引用，重排会让一次
// 删除把 A 消息的序号悄悄指到 B 消息上；`Append` 走 `MAX(ordinal)+1`，所以空洞也
// 不会被新消息重新占用。代价是序号不再连续，读路径一律按 `ORDER BY ordinal`，不假设
// 密度。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"
)

// SessionStats 是一个会话的只读体检视图：宿主拿它回答"这个会话有多大、花了多少、
// 有没有可恢复的检查点"。
type SessionStats struct {
	// Messages 是消息条数（`rig_messages` 的行数，与 Meta.Messages 同源）。
	Messages int
	// Events 是事件条数（`rig_events` 的行数）。
	Events int
	// Costs 是已结算请求的行数（`rig_costs` 的行数）。
	Costs int
	// PromptTokens / CompletionTokens 是全部花费行的合计。
	PromptTokens     int
	CompletionTokens int
	// CostUSD 是全部花费行的金额合计（估算值，不是账单）。
	CostUSD float64
	// Bytes 是消息载荷的字节合计（`LENGTH(payload)`），不是会话文件大小。
	Bytes int64
	// Checkpoint 表示这个会话是否有可恢复的检查点。
	//
	// 检查点在 schema 里是**每会话单行**（`rig_checkpoints.session_id` 是主键），
	// 所以只有一个布尔值：不存在"攒了很多检查点要修剪"的问题，本包也不提供
	// `PruneCheckpoints`——删除入口只有 DeleteCheckpoint。
	Checkpoint bool
	// CreatedAt / UpdatedAt 取自会话行；会话存在时两者一定有值。
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ensureSession 在事务里确认会话行存在；不存在时返回 ErrNotFound。
//
// 删除类方法即使"这次什么都没删"也要先确认会话：静默成功会让宿主以为一个拼错的
// 会话 ID 是有效会话。
func ensureSession(ctx context.Context, tx *sql.Tx, sessionID string) error {
	var one int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM rig_sessions WHERE id = ?`, sessionID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("read session %q: %w", sessionID, err)
	}
	return nil
}

// touchAfterDelete 在同一个事务里按 Append 的规矩维护会话行：
// `message_count` 重算成实际行数（不靠"减去返回值"推算，历史里有空洞或旧计数已经
// 漂移时也不会越走越偏），`updated_at` 推进到本次写入，`revision` +1 用于同毫秒
// 写入之间保序。`title_cleared` 不受影响：删消息不是改标题，用户显式清空的标题
// 不该因为一次删除被重新打开自动命名的口子。
func touchAfterDelete(ctx context.Context, tx *sql.Tx, sessionID string) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE rig_sessions
		    SET message_count = (SELECT COUNT(*) FROM rig_messages WHERE session_id = ?),
		        updated_at = ?, revision = revision + 1
		  WHERE id = ?`,
		sessionID, time.Now().UTC().UnixMilli(), sessionID); err != nil {
		return fmt.Errorf("update session %q: %w", sessionID, err)
	}
	return nil
}

// DeleteMessages 按 ordinal 删除一个会话里的若干条消息，返回**真正删掉的行数**。
//
// 语义：
//   - 不存在的 ordinal、重复给出的 ordinal 一律幂等跳过（不算错误）；
//   - **允许留空洞**，不重排 ordinal（见文件头）；
//   - 一条都没删到时只做存在性检查：不推进 `updated_at` / `revision`，否则一次误点
//     会把这个会话顶到列表最前面；
//   - 删到时 `message_count` 重算、`updated_at` 推进、`revision` +1（与 Append 同一
//     套规矩）；`title_cleared` 不变；
//   - 未知会话返回 ErrNotFound，关闭之后返回 ErrClosed。
func (s *Store) DeleteMessages(ctx context.Context, sessionID string, ordinals []int) (int, error) {
	if err := s.usable(); err != nil {
		return 0, err
	}
	// 去重并按升序处理：SQL 的 IN 列表顺序不影响结果，但对同一批序号的两次调用
	// 形状一致，日志与测试都更好读；负数序号不可能存在，直接跳过省一个占位符。
	unique := make([]int, 0, len(ordinals))
	seen := make(map[int]struct{}, len(ordinals))
	for _, ordinal := range ordinals {
		if ordinal < 0 {
			continue
		}
		if _, ok := seen[ordinal]; ok {
			continue
		}
		seen[ordinal] = struct{}{}
		unique = append(unique, ordinal)
	}
	sort.Ints(unique)

	deleted := 0
	// 删除 + 会话行维护在同一个事务里；锁冲突时整个事务重开（与 Append/Replace 一致）。
	err := retryBusy(ctx, func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := ensureSession(ctx, tx, sessionID); err != nil {
			return err
		}
		if len(unique) == 0 {
			// 空序号列表：只确认会话存在，不写任何行。
			deleted = 0
			return tx.Commit()
		}
		query := `DELETE FROM rig_messages WHERE session_id = ? AND ordinal IN (?`
		args := make([]any, 0, len(unique)+1)
		args = append(args, sessionID, unique[0])
		for _, ordinal := range unique[1:] {
			query += `, ?`
			args = append(args, ordinal)
		}
		query += `)`
		result, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("delete messages: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		deleted = int(affected)
		if deleted == 0 {
			return tx.Commit()
		}
		if err := touchAfterDelete(ctx, tx, sessionID); err != nil {
			return err
		}
		return tx.Commit()
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("sqlite: delete messages of %q: %w", sessionID, err)
	}
	return deleted, nil
}

// TruncateMessages 只保留最新的 keep 条消息（按 ordinal），返回删掉的行数；
// `keep <= 0` 表示删空。keep 大于现有条数时一行都不删。
//
// "最新"只按 ordinal 判定，不看时间戳：ordinal 是写入顺序，而消息自己的时间字段
// 可能是调用方补的历史值。删除同样**不重排**剩下的 ordinal。
//
// 与 DeleteMessages 一样：删到行时维护 `message_count` / `updated_at` / `revision`，
// 没删到行时什么也不动；未知会话返回 ErrNotFound，关闭之后返回 ErrClosed。
func (s *Store) TruncateMessages(ctx context.Context, sessionID string, keep int) (int, error) {
	if err := s.usable(); err != nil {
		return 0, err
	}
	deleted := 0
	err := retryBusy(ctx, func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := ensureSession(ctx, tx, sessionID); err != nil {
			return err
		}
		// keep<=0 走"删空"，避免把 0 传给 LIMIT（语序上虽然合法，但语义不直观）。
		statement := `DELETE FROM rig_messages WHERE session_id = ?`
		args := []any{sessionID}
		if keep > 0 {
			// 排序键必须是 ordinal：载荷里的时间可能是调用方补的，不能用它选"最新"。
			statement = `DELETE FROM rig_messages WHERE session_id = ? AND ordinal NOT IN (
				SELECT ordinal FROM rig_messages WHERE session_id = ? ORDER BY ordinal DESC LIMIT ?
			)`
			args = append(args, sessionID, keep)
		}
		result, err := tx.ExecContext(ctx, statement, args...)
		if err != nil {
			return fmt.Errorf("truncate messages: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		deleted = int(affected)
		if deleted == 0 {
			return tx.Commit()
		}
		if err := touchAfterDelete(ctx, tx, sessionID); err != nil {
			return err
		}
		return tx.Commit()
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("sqlite: truncate messages of %q: %w", sessionID, err)
	}
	return deleted, nil
}

// Stats 汇总一个会话的体检数据：消息/事件/花费的条数、token 与金额合计、消息载荷
// 的字节合计，以及是否存在检查点。
//
// 未知会话返回 ErrNotFound（与 Load 同一个哨兵），关闭之后返回 ErrClosed。
//
// 实现是几条独立的聚合查询（不包在一个读事务里）：这些都是"只增不改"的面，两边
// 之间的竞争最多让某个数字落后一个正在提交的回合，不会读到半截状态；为此把整个
// 会话锁进一个快照的代价比收益大。
func (s *Store) Stats(ctx context.Context, sessionID string) (SessionStats, error) {
	if err := s.usable(); err != nil {
		return SessionStats{}, err
	}
	// 先读会话行：它同时提供存在性（ErrNotFound）与创建/更新时间。
	meta, err := s.meta(ctx, sessionID)
	if err != nil {
		return SessionStats{}, err
	}
	stats := SessionStats{CreatedAt: meta.CreatedAt, UpdatedAt: meta.UpdatedAt}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(LENGTH(payload)), 0) FROM rig_messages WHERE session_id = ?`,
		sessionID).Scan(&stats.Messages, &stats.Bytes); err != nil {
		return SessionStats{}, fmt.Errorf("sqlite: stats messages of %q: %w", sessionID, err)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rig_events WHERE session_id = ?`, sessionID).Scan(&stats.Events); err != nil {
		return SessionStats{}, fmt.Errorf("sqlite: stats events of %q: %w", sessionID, err)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0),
		        COALESCE(SUM(cost_usd), 0)
		   FROM rig_costs WHERE session_id = ?`, sessionID).
		Scan(&stats.Costs, &stats.PromptTokens, &stats.CompletionTokens, &stats.CostUSD); err != nil {
		return SessionStats{}, fmt.Errorf("sqlite: stats costs of %q: %w", sessionID, err)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM rig_checkpoints WHERE session_id = ?)`, sessionID).
		Scan(&stats.Checkpoint); err != nil {
		return SessionStats{}, fmt.Errorf("sqlite: stats checkpoint of %q: %w", sessionID, err)
	}
	return stats, nil
}

// DeleteCheckpoint 删除一个会话的检查点，返回是否真的删掉了一行；没有检查点时返回
// false, nil（幂等）。
//
// 检查点在 schema 里是**每会话单行**（`rig_checkpoints.session_id` 是主键，见
// schema.go 段 1），所以不需要 `PruneCheckpoints` 一类的修剪入口：要么有且只有
// 一行，要么没有。未知会话也返回 false, nil——删除检查点与 Delete 一样是幂等的
// 删除，不该因为会话已经不在而报错；关闭之后返回 ErrClosed。
func (s *Store) DeleteCheckpoint(ctx context.Context, sessionID string) (bool, error) {
	if err := s.usable(); err != nil {
		return false, err
	}
	deleted := false
	err := retryBusy(ctx, func() error {
		result, execErr := s.db.ExecContext(ctx, `DELETE FROM rig_checkpoints WHERE session_id = ?`, sessionID)
		if execErr != nil {
			return execErr
		}
		affected, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			return rowsErr
		}
		deleted = affected > 0
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("sqlite: delete checkpoint %q: %w", sessionID, err)
	}
	return deleted, nil
}
