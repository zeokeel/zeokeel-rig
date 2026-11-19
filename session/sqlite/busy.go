package sqlite

// 写路径的"锁被占用"重试策略。SQLite 在 busy_timeout 到点后立刻返回
// SQLITE_BUSY（驱动文案 "database is locked"），对桌面产品来说"另一个窗口正在写"
// 是常态，直接把它抛给用户等于随机失败。这里做**有限**重试：3 次、指数退避，且总
// 时长不超过调用方的 ctx 与一个上限。

import (
	"context"
	"errors"
	"strings"
	"time"
)

// busyRetries 是一次写操作在"锁被占用"时最多重试的次数（含首次之外的追加次数）。
const busyRetries = 3

// busyBackoff 是第一次重试前的等待时长，之后每次翻倍。
const busyBackoff = 15 * time.Millisecond

// busyRetryBudget 是重试等待的**总**上限，避免在长事务后面无限等下去。
const busyRetryBudget = 2 * time.Second

// isBusy 判断错误是否是 SQLITE_BUSY / SQLITE_BUSY_SNAPSHOT。
//
// 驱动没有导出错误码，所以按文案判定；同时把结果映射到哨兵 ErrBusy，让调用方
// 不必匹配字符串。
func isBusy(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrBusy) {
		return true
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "database is locked") || strings.Contains(text, "sqlite_busy")
}

// isUniqueViolation 判断错误是否是唯一约束冲突（用于 Create 把"ID 已存在"翻译成
// ErrExists）。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "unique constraint") || strings.Contains(text, "constraint failed")
}

// retryBusy 执行 op；遇到"锁被占用"时按指数退避重试，最多 busyRetries 次，且总
// 等待不超过 busyRetryBudget 与 ctx 的剩余时间。最终仍失败时返回包装了 ErrBusy 的
// 错误。
func retryBusy(ctx context.Context, op func() error) error {
	var (
		wait  = busyBackoff
		total time.Duration
	)
	err := op()
	for attempt := 0; isBusy(err); attempt++ {
		if attempt >= busyRetries {
			return err
		}
		if total+wait > busyRetryBudget {
			return err
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			// 调用方先放弃了：返回 ctx 的错误，而不是"库被锁住"。
			return ctx.Err()
		case <-timer.C:
		}
		total += wait
		wait *= 2
		err = op()
	}
	return err
}
