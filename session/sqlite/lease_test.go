package sqlite_test

// 会话租约：agent 每个 Run 开头都会 RecoverInterruptedTurn，它会把"有检查点、没
// 有结束标记"的回合当成崩溃残留并补一条合成消息。因此同一个库被两个进程打开时，
// "这个会话现在是谁在跑"必须有一个**库内**的答案——进程内的锁到不了另一个进程。
//
// 这里用两个 Store（= 两条连接 = SQLite 文件锁看到的两个写者）来模拟两个进程：
// 租约的原子性完全由那一条 upsert 与 SQLite 的文件锁保证，不依赖进程内状态。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	sqlitestore "github.com/zeokeel/zeokeel-rig/session/sqlite"
)

// openSecond 在同一个库文件上开第二个 store（第二个"进程"）。
func openSecond(t *testing.T, path string) *sqlitestore.Store {
	t.Helper()
	second, err := sqlitestore.Open(sqlitestore.Options{Path: path})
	if err != nil {
		t.Fatalf("第二个 store: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	return second
}

// TestLeaseBlocksSecondProcess 是跨进程污染的核心回归：A 持有租约时 B 拿不到，
// A 释放之后 B 立刻可以拿。
func TestLeaseBlocksSecondProcess(t *testing.T) {
	ctx := context.Background()
	first, path := openTemp(t, sqlitestore.Options{})
	second := openSecond(t, path)

	if err := first.AcquireLease(ctx, "s1", "proc-a", time.Minute); err != nil {
		t.Fatalf("A 第一次 AcquireLease: %v", err)
	}
	err := second.AcquireLease(ctx, "s1", "proc-b", time.Minute)
	if !errors.Is(err, sqlitestore.ErrLeaseHeld) {
		t.Fatalf("B 在 A 持有期间 AcquireLease = %v，want ErrLeaseHeld", err)
	}
	// 诊断信息里要能看清是谁持有：UI 要显示"另一个窗口正在跑这个会话"。
	if !strings.Contains(err.Error(), "proc-a") {
		t.Fatalf("ErrLeaseHeld 没有带上持有者：%v", err)
	}

	// 同一个 owner 重复 Acquire 是幂等续租，不是冲突（进程重启后重入同一会话）。
	if err := first.AcquireLease(ctx, "s1", "proc-a", time.Minute); err != nil {
		t.Fatalf("同一 owner 再次 AcquireLease: %v", err)
	}
	// 续租只有持有者能做。
	if err := second.RenewLease(ctx, "s1", "proc-b", time.Minute); !errors.Is(err, sqlitestore.ErrLeaseHeld) {
		t.Fatalf("B RenewLease = %v，want ErrLeaseHeld", err)
	}
	if err := first.RenewLease(ctx, "s1", "proc-a", time.Minute); err != nil {
		t.Fatalf("A RenewLease: %v", err)
	}
	// 租约状态可读（agent 侧判"活回合还是残留"要用它）。
	held, err := second.LeaseOwner(ctx, "s1")
	if err != nil {
		t.Fatalf("LeaseOwner: %v", err)
	}
	if held.Owner != "proc-a" || held.SessionID != "s1" {
		t.Fatalf("LeaseOwner = %+v，want owner proc-a / session s1", held)
	}
	if held.Expired(time.Now()) {
		t.Fatalf("刚续租的租约被判成过期：%+v", held)
	}

	// A 释放之后 B 可以拿。
	if err := first.ReleaseLease(ctx, "s1", "proc-a"); err != nil {
		t.Fatalf("A ReleaseLease: %v", err)
	}
	if _, err := second.LeaseOwner(ctx, "s1"); !errors.Is(err, sqlitestore.ErrNoLease) {
		t.Fatalf("释放之后 LeaseOwner = %v，want ErrNoLease", err)
	}
	if err := second.AcquireLease(ctx, "s1", "proc-b", time.Minute); err != nil {
		t.Fatalf("A 释放后 B AcquireLease: %v", err)
	}

	// Release 是幂等的，而且**只删自己的行**：A 迟到的 Release 不能把 B 的租约删掉。
	if err := first.ReleaseLease(ctx, "s1", "proc-a"); err != nil {
		t.Fatalf("释放不属于自己的租约应该静默成功，得到 %v", err)
	}
	if held, err := second.LeaseOwner(ctx, "s1"); err != nil || held.Owner != "proc-b" {
		t.Fatalf("迟到的 Release 删掉了新持有者的租约：%+v / %v", held, err)
	}
}

// TestLeaseExpiresAndCanBeTakenOver 钉住 TTL 语义：未过期时别人拿不到；过期之后
// 可以被抢占，而且**老持有者醒来之后不能再续租、也删不掉新租约**。
func TestLeaseExpiresAndCanBeTakenOver(t *testing.T) {
	ctx := context.Background()
	first, path := openTemp(t, sqlitestore.Options{})
	second := openSecond(t, path)

	// TTL 用一个真实的短值：租约的新鲜度是对**墙钟**的比较，不睡觉就只能靠把
	// 时钟拨回去。
	const ttl = 150 * time.Millisecond
	if err := first.AcquireLease(ctx, "s1", "proc-a", ttl); err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	// 还没过期：抢不到。
	if err := second.AcquireLease(ctx, "s1", "proc-b", time.Minute); !errors.Is(err, sqlitestore.ErrLeaseHeld) {
		t.Fatalf("租约未过期时 B AcquireLease = %v，want ErrLeaseHeld", err)
	}
	// 等它过期（轮询而不是死等，避免把测试绑死在某个具体的时钟粒度上）。
	deadline := time.Now().Add(3 * time.Second)
	for {
		held, err := first.LeaseOwner(ctx, "s1")
		if err != nil {
			t.Fatalf("LeaseOwner: %v", err)
		}
		if held.Expired(time.Now()) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("租约始终没有过期：%+v", held)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// 过期即可被抢占。
	if err := second.AcquireLease(ctx, "s1", "proc-b", time.Minute); err != nil {
		t.Fatalf("过期之后 B 无法抢占：%v", err)
	}
	held, err := second.LeaseOwner(ctx, "s1")
	if err != nil {
		t.Fatalf("LeaseOwner: %v", err)
	}
	if held.Owner != "proc-b" {
		t.Fatalf("抢占之后 owner = %q，want proc-b", held.Owner)
	}
	// 老持有者不能再续租（否则两个进程会同时认为自己持有）。
	if err := first.RenewLease(ctx, "s1", "proc-a", time.Minute); !errors.Is(err, sqlitestore.ErrLeaseHeld) {
		t.Fatalf("被抢走之后 A RenewLease = %v，want ErrLeaseHeld", err)
	}
	// 也不能删掉新持有者的行。
	if err := first.ReleaseLease(ctx, "s1", "proc-a"); err != nil {
		t.Fatalf("A ReleaseLease: %v", err)
	}
	if held, err := second.LeaseOwner(ctx, "s1"); err != nil || held.Owner != "proc-b" {
		t.Fatalf("老持有者的 Release 删掉了新租约：%+v / %v", held, err)
	}
}

// TestLeaseTableIsCreatedWhenUpgradingAnOlderDatabase 钉住"租约表随升级建出来"。
//
// 这是 coreVersion 那一类故障的回归：新增表却漏了版本推进时，**新库**有表、**老库**
// 没有，运行期一取租约就是 `no such table: rig_session_leases`（而且没有任何提示）。
// 这里把一个当前库伪装成 v2（删表 + 把 user_version 拨回 2），再正常打开：迁移
// 必须把表和索引建回来。
func TestLeaseTableIsCreatedWhenUpgradingAnOlderDatabase(t *testing.T) {
	ctx := context.Background()
	old, path := openTemp(t, sqlitestore.Options{})
	if err := old.AcquireLease(ctx, "s1", "proc-a", time.Minute); err != nil {
		t.Fatalf("新库 AcquireLease: %v", err)
	}
	// 表与索引都在：这是"只加新表与新索引"的可见证据。
	schema := schemaOf(t, old)
	if !has(schema.Tables, "rig_session_leases") {
		t.Fatalf("表 rig_session_leases 缺失；tables = %v", schema.Tables)
	}
	if !has(schema.Indexes, "rig_session_leases_expires_at") {
		t.Fatalf("索引 rig_session_leases_expires_at 缺失；indexes = %v", schema.Indexes)
	}
	for _, statement := range []string{
		`DROP TABLE rig_session_leases`,
		`PRAGMA user_version = 2`,
	} {
		if _, err := old.DB().ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	upgraded, err := sqlitestore.Open(sqlitestore.Options{Path: path})
	if err != nil {
		t.Fatalf("升级打开: %v", err)
	}
	defer func() { _ = upgraded.Close() }()
	if err := upgraded.AcquireLease(ctx, "s1", "proc-a", time.Minute); err != nil {
		t.Fatalf("升级之后 AcquireLease = %v（老库没建出租约表）", err)
	}
}

// TestLeaseRejectsUnusableArguments 钉住入参校验：空会话 ID、空 owner、非正 TTL
// 都必须直接报错，而不是写一行没人能释放的租约。
func TestLeaseRejectsUnusableArguments(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t, sqlitestore.Options{})
	for _, tc := range []struct {
		name      string
		sessionID string
		owner     string
		ttl       time.Duration
	}{
		{"空会话 ID", "  ", "proc-a", time.Minute},
		{"空 owner", "s1", "", time.Minute},
		{"零 TTL", "s1", "proc-a", 0},
		{"负 TTL", "s1", "proc-a", -time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := db.AcquireLease(ctx, tc.sessionID, tc.owner, tc.ttl); err == nil {
				t.Fatalf("%s: AcquireLease 返回 nil，want 参数错误", tc.name)
			}
			if err := db.RenewLease(ctx, tc.sessionID, tc.owner, tc.ttl); err == nil {
				t.Fatalf("%s: RenewLease 返回 nil，want 参数错误", tc.name)
			}
		})
	}
	// 租约不要求会话行已经存在：Run 开头的租约可能在第一条消息之前拿。
	if err := db.AcquireLease(ctx, "还没建的会话", "proc-a", time.Minute); err != nil {
		t.Fatalf("给尚未建行的会话取租约: %v", err)
	}
	// 关闭之后是 ErrClosed（与其余方法一致），不是空指针 panic。
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := db.AcquireLease(ctx, "s1", "proc-a", time.Minute); !errors.Is(err, sqlitestore.ErrClosed) {
		t.Fatalf("关闭之后 AcquireLease = %v，want ErrClosed", err)
	}
	if _, err := db.LeaseOwner(ctx, "s1"); !errors.Is(err, sqlitestore.ErrClosed) {
		t.Fatalf("关闭之后 LeaseOwner = %v，want ErrClosed", err)
	}
	if err := db.ReleaseLease(ctx, "s1", "proc-a"); !errors.Is(err, sqlitestore.ErrClosed) {
		t.Fatalf("关闭之后 ReleaseLease = %v，want ErrClosed", err)
	}
	if err := db.RenewLease(ctx, "s1", "proc-a", time.Minute); !errors.Is(err, sqlitestore.ErrClosed) {
		t.Fatalf("关闭之后 RenewLease = %v，want ErrClosed", err)
	}
}
