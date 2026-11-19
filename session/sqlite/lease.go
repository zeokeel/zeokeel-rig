package sqlite

// 会话租约：同一个库被两个进程同时打开时，"这个会话现在由谁在跑回合"必须有
// 一个**库内**的答案。
//
// # 为什么需要它
//
// agent 每个 Run 开头都会 RecoverInterruptedTurn：它把"有检查点、没有结束标记"的
// 回合判定为崩溃残留，然后补一条合成消息、把回合收尾。第二个进程打开同一个库时
// 因此会把第一个进程**正在跑**的回合当成残留——写坏用户可见的历史，而且不可回退。
// 进程内的锁到不了另一个进程，所以判断必须落在这张表上。
//
// # 语义
//
//   - 一个会话同一时刻只有一个 owner：`session_id` 是主键，AcquireLease 是一条
//     `INSERT … ON CONFLICT DO UPDATE … WHERE`，所以"读-判断-写"是原子的，不需要
//     额外的事务或进程内锁。
//   - **过期即可被抢占**：`expires_at <= 现在` 时任何人都能拿到，owner 换成新进程。
//   - 同一个 owner 重复 Acquire 是**幂等续租**（刷新 acquired_at/expires_at），
//     不是冲突——进程重启后重入同一个会话不该被自己上一次的租约挡住。
//   - RenewLease / ReleaseLease 都带 `owner = ?`：被抢走之后，老进程既续不了租、
//     也删不掉新持有者的行（否则"两个进程都以为自己持有"或"老进程把新进程踢掉"）。
//   - 租约**不要求会话行已经存在**：Run 可能在第一条消息之前就取租约（那时
//     rig_sessions 里还没有行）。所以这张表没有外键。
//
// # 时钟依赖（重要）
//
// 判断用的是**墙钟**毫秒，而且是"当前进程的时钟"与"上一个持有者写下的
// expires_at"比较。因此：
//   - 同一台机器上（租约最常见的场景）没有问题；
//   - 共享网络文件系统上两个时钟不同的机器会出问题：快的一方会提前抢占活着的
//     回合（等于回到租约要解决的污染问题），慢的一方会长时间拿不到；
//   - 系统时间被手工回拨也可能让一个本该过期的租约继续有效。
//
// 所以 TTL 要选得让"一次正常的回合"远小于它（并配合 RenewLease 在长回合里续），
// 而调用方在 ErrLeaseHeld 上应该有"等一下再试"的退路，而不是立刻失败。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrLeaseHeld 表示这个会话的租约在**别人**手里，而且还没过期。
//
// 它与 ErrBusy 是两件事：ErrBusy 是"库的写锁被别人占着（等一会儿就好）"，
// ErrLeaseHeld 是"这个会话的回合由另一个进程在跑（不该去动它的历史）"。
var ErrLeaseHeld = errors.New("sqlite: session lease is held by another owner")

// ErrNoLease 表示这个会话当前没有租约行：从未获取、已释放，或已被清扫。
var ErrNoLease = errors.New("sqlite: session has no lease")

// Lease 是会话租约的当前状态。
type Lease struct {
	// SessionID 是租约保护的会话。
	SessionID string
	// Owner 是当前持有者的标识（进程/实例级唯一即可，例如 PID + 随机后缀）。
	Owner string
	// AcquiredAt 是当前持有者拿到（或最后一次重复 Acquire 到）租约的时间。
	AcquiredAt time.Time
	// ExpiresAt 是租约到期时间。到期之后可被任何进程抢占。
	ExpiresAt time.Time
}

// Expired 报告在 at 时刻这个租约是否已经可以被抢占。
//
// 判据是 `!at.Before(ExpiresAt)`：正好到期就算过期——与 SQL 里的
// `expires_at <= 现在` 保持一致，免得 Go 侧与库内判断差一毫秒。
func (l Lease) Expired(at time.Time) bool { return !at.Before(l.ExpiresAt) }

// leaseColumns 是读出租约行的列顺序（四个读方法共用，避免漂移）。
const leaseColumns = `session_id, owner, acquired_at, expires_at`

// AcquireLease 尝试取得 sessionID 的租约，ttl 是本进程认为它可以保持多久。
//
// 拿不到（别人持有且未过期）返回 ErrLeaseHeld，错误文本里带持有者与到期时间，
// 便于 UI 直接显示"另一个窗口正在跑这个会话"。已关闭的 store 返回 ErrClosed。
func (s *Store) AcquireLease(ctx context.Context, sessionID, owner string, ttl time.Duration) error {
	if err := validateLeaseArgs(sessionID, owner, ttl); err != nil {
		return err
	}
	if err := s.usable(); err != nil {
		return err
	}
	now := time.Now().UTC()
	expires := now.Add(ttl)
	var affected int64
	err := retryBusy(ctx, func() error {
		result, execErr := s.db.ExecContext(ctx,
			`INSERT INTO rig_session_leases(session_id, owner, acquired_at, expires_at)
			 VALUES(?, ?, ?, ?)
			 ON CONFLICT(session_id) DO UPDATE SET
			   owner       = excluded.owner,
			   acquired_at = excluded.acquired_at,
			   expires_at  = excluded.expires_at
			 WHERE rig_session_leases.expires_at <= ? OR rig_session_leases.owner = excluded.owner`,
			sessionID, owner, now.UnixMilli(), expires.UnixMilli(), now.UnixMilli())
		if execErr != nil {
			return execErr
		}
		affected, execErr = result.RowsAffected()
		return execErr
	})
	if err != nil {
		return fmt.Errorf("sqlite: acquire lease %q: %w", sessionID, err)
	}
	if affected > 0 {
		return nil
	}
	// 没有抢到：把当前持有人读出来，让错误能直接说明"是谁、到什么时候"。
	if held, lookupErr := s.LeaseOwner(ctx, sessionID); lookupErr == nil {
		return fmt.Errorf("sqlite: acquire lease %q: %w (owner %q until %s)",
			sessionID, ErrLeaseHeld, held.Owner, held.ExpiresAt.Format(time.RFC3339Nano))
	}
	// 读不到（例如对方在这几微秒里释放了）：仍然是"没拿到"，但别编造持有者。
	return fmt.Errorf("sqlite: acquire lease %q: %w", sessionID, ErrLeaseHeld)
}

// RenewLease 把租约延长到"现在 + ttl"。
//
// 只有当前 owner 能续：行不存在、或 owner 已经换成别人（自己过了期被抢占），都
// 返回 ErrLeaseHeld。续租成功只证明"此刻 owner 还是自己"——不保证在下一个写操作
// 提交时还是，所以长回合要在每次落库前续租。
func (s *Store) RenewLease(ctx context.Context, sessionID, owner string, ttl time.Duration) error {
	if err := validateLeaseArgs(sessionID, owner, ttl); err != nil {
		return err
	}
	if err := s.usable(); err != nil {
		return err
	}
	expires := time.Now().UTC().Add(ttl)
	var affected int64
	err := retryBusy(ctx, func() error {
		result, execErr := s.db.ExecContext(ctx,
			`UPDATE rig_session_leases SET expires_at = ? WHERE session_id = ? AND owner = ?`,
			expires.UnixMilli(), sessionID, owner)
		if execErr != nil {
			return execErr
		}
		affected, execErr = result.RowsAffected()
		return execErr
	})
	if err != nil {
		return fmt.Errorf("sqlite: renew lease %q: %w", sessionID, err)
	}
	if affected > 0 {
		return nil
	}
	if held, lookupErr := s.LeaseOwner(ctx, sessionID); lookupErr == nil {
		return fmt.Errorf("sqlite: renew lease %q: %w (owner %q until %s)",
			sessionID, ErrLeaseHeld, held.Owner, held.ExpiresAt.Format(time.RFC3339Nano))
	}
	return fmt.Errorf("sqlite: renew lease %q: %w (no lease held)", sessionID, ErrLeaseHeld)
}

// ReleaseLease 释放自己持有的租约。
//
// 幂等：没有租约、或者租约已经属于别人，都**不算错误**——释放是一个收尾动作，
// 收尾不该因为"被别人抢走了"而让 Shutdown/Run 失败。别人的行**不会**被删掉
// （DELETE 带 owner 条件，不是"按会话删"）。
func (s *Store) ReleaseLease(ctx context.Context, sessionID, owner string) error {
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("sqlite: lease requires a session id")
	}
	if strings.TrimSpace(owner) == "" {
		return errors.New("sqlite: lease requires an owner")
	}
	if err := s.usable(); err != nil {
		return err
	}
	err := retryBusy(ctx, func() error {
		_, execErr := s.db.ExecContext(ctx,
			`DELETE FROM rig_session_leases WHERE session_id = ? AND owner = ?`, sessionID, owner)
		return execErr
	})
	if err != nil {
		return fmt.Errorf("sqlite: release lease %q: %w", sessionID, err)
	}
	return nil
}

// LeaseOwner 读出租约的当前状态。
//
// 它给"这是活着的回合还是崩溃残留"提供数据：调用方拿到 owner 与 expires_at 之后
// 自己决定要不要 RecoverInterruptedTurn（这个判断在 agent 侧，不在本包）。
// 会话没有租约行时返回 ErrNoLease。
func (s *Store) LeaseOwner(ctx context.Context, sessionID string) (Lease, error) {
	if strings.TrimSpace(sessionID) == "" {
		return Lease{}, errors.New("sqlite: lease requires a session id")
	}
	if err := s.usable(); err != nil {
		return Lease{}, err
	}
	var (
		lease      Lease
		acquiredAt int64
		expiresAt  int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT `+leaseColumns+` FROM rig_session_leases WHERE session_id = ?`, sessionID).
		Scan(&lease.SessionID, &lease.Owner, &acquiredAt, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Lease{}, ErrNoLease
	}
	if err != nil {
		return Lease{}, fmt.Errorf("sqlite: read lease %q: %w", sessionID, err)
	}
	lease.AcquiredAt = time.UnixMilli(acquiredAt).UTC()
	lease.ExpiresAt = time.UnixMilli(expiresAt).UTC()
	return lease, nil
}

// validateLeaseArgs 拒绝那些"会写下一行没人能释放的租约"的入参。
func validateLeaseArgs(sessionID, owner string, ttl time.Duration) error {
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("sqlite: lease requires a session id")
	}
	if strings.TrimSpace(owner) == "" {
		return errors.New("sqlite: lease requires an owner")
	}
	if ttl <= 0 {
		return fmt.Errorf("sqlite: lease ttl must be positive, got %v", ttl)
	}
	return nil
}
