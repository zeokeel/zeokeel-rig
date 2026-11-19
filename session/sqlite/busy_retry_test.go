package sqlite_test

// 写路径的"锁被占用"重试：SQLITE_BUSY 在桌面产品里是常态（一个窗口 + 一个后台
// 任务开着同一个文件），不是错误。这里钉住三件事：
//
//  1. 每个写入口在写锁释放之后都能**自己**成功，不需要宿主重试；
//  2. 重试有明确上限：锁一直被握着时会在一个有限时间内返回 ErrBusy，而不是挂住；
//  3. ctx 取消要在退避期间生效，不能"等满预算再说"。
//
// 制造 BUSY 的方式是第二个 Store（等于第二个连接/第二个进程）先拿住写事务，被测
// 的 Store 用 1ms 的 busy_timeout——这样每次尝试都会立刻拿到 SQLITE_BUSY，而不是
// 靠默认的 5s 等待把测试拖成慢测试。

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	rig "github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/memory"
	sqlitestore "github.com/zeokeel/zeokeel-rig/session/sqlite"
)

// holdWriteLock 在 path 上开第二个连接并**握着写锁**，返回一个幂等的释放函数。
//
// 写锁用一条真实的 INSERT 取得（延迟事务在第一条写语句上才升级为写锁）；释放用
// Rollback，于是这条占位行不会留在库里。
func holdWriteLock(t *testing.T, path string) func() {
	t.Helper()
	holder, err := sqlitestore.Open(sqlitestore.Options{Path: path, BusyTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("打开持锁者: %v", err)
	}
	tx, err := holder.DB().BeginTx(context.Background(), nil)
	if err != nil {
		_ = holder.Close()
		t.Fatalf("持锁者 BeginTx: %v", err)
	}
	if _, err := tx.ExecContext(context.Background(),
		`INSERT INTO rig_memory_pages(name, title, summary, content, updated_at, bytes)
		 VALUES('lock-holder', '', '', '', 0, 0)`); err != nil {
		_ = tx.Rollback()
		_ = holder.Close()
		t.Fatalf("持锁者取写锁: %v", err)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = tx.Rollback()
			_ = holder.Close()
		})
	}
}

// TestAppendBatchRetriesOnBusy 是热路径的回归：AppendBatch 曾经完全没有重试，
// 于是"另一个窗口正在写"会直接变成这个回合的失败。
func TestAppendBatchRetriesOnBusy(t *testing.T) {
	ctx := context.Background()
	db, path := openTemp(t, sqlitestore.Options{BusyTimeout: time.Millisecond})
	release := holdWriteLock(t, path)
	defer release()

	result := make(chan error, 1)
	go func() {
		_, err := db.AppendBatch(ctx, "busy", userMessage("等锁"))
		result <- err
	}()
	// 让第一次尝试真的撞上锁（busy_timeout 只有 1ms），再放开。
	time.Sleep(40 * time.Millisecond)
	release()

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("写锁释放后 AppendBatch 仍然失败（没有重试）：%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("AppendBatch 在 5s 内没有返回")
	}
	// 重试不是"假成功"：消息必须真的落库，而且只落一份。
	_, messages, err := db.Load(ctx, "busy")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("消息条数 = %d，want 1（重试不能重复写）", len(messages))
	}
}

// TestAppendBatchRetryIsBoundedAndCancelable 钉住重试的两条边界：锁一直被握着时
// 有限时间内返回 ErrBusy；ctx 到期时立刻放弃。
//
// 重试由 busyRetryBudget（10s，等锁的天花板）决定，不再有"次数"上限——这里断言
// 的是**可观测行为**：耗时有下界（真的退避重试过）也有上界（不是无限等待），并且
// ctx 到期时立刻放弃。
func TestAppendBatchRetryIsBoundedAndCancelable(t *testing.T) {
	db, path := openTemp(t, sqlitestore.Options{BusyTimeout: time.Millisecond})
	release := holdWriteLock(t, path)
	defer release()

	start := time.Now()
	_, err := db.AppendBatch(context.Background(), "busy", userMessage("永远等不到"))
	elapsed := time.Since(start)
	if !errors.Is(err, sqlitestore.ErrBusy) {
		t.Fatalf("锁一直被握着时 err = %v，want ErrBusy", err)
	}
	if elapsed > 12*time.Second {
		t.Fatalf("重试没有上限（应受 busyRetryBudget≈10s 约束）：耗时 %v", elapsed)
	}
	if elapsed < 60*time.Millisecond {
		t.Fatalf("一次都没退避重试就放弃了：耗时 %v", elapsed)
	}

	// ctx 在退避期间到期：必须返回 ctx 的错误，而不是继续重试到预算用尽。
	deadline, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	start = time.Now()
	_, err = db.AppendBatch(deadline, "busy", userMessage("取消"))
	elapsed = time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ctx 到期时 err = %v，want context.DeadlineExceeded", err)
	}
	if elapsed > time.Second {
		t.Fatalf("ctx 取消没有中止重试：耗时 %v", elapsed)
	}
}

// TestHotWritePathsRetryOnBusy 覆盖其余写入口：热路径上"没有重试"的不止
// AppendBatch 一个。每一个入口都必须在写锁释放后自行成功。
func TestHotWritePathsRetryOnBusy(t *testing.T) {
	ctx := context.Background()
	seed := func(db *sqlitestore.Store) error {
		if err := db.Memory().Write(ctx, memory.Page{Name: "log", Summary: "种子页", Content: "# log"}); err != nil {
			return err
		}
		_, err := db.Create(ctx, sqlitestore.Meta{ID: "s1"})
		return err
	}
	cases := []struct {
		name string
		seed bool
		call func(db *sqlitestore.Store) error
	}{
		{"AppendBatch", false, func(db *sqlitestore.Store) error {
			_, err := db.AppendBatch(ctx, "s1", userMessage("x"))
			return err
		}},
		{"Replace", true, func(db *sqlitestore.Store) error {
			// Replace 只重写既有会话的消息（它不会像 AppendBatch 那样补会话行），
			// 所以这里要先有 s1 这一行，否则失败的是外键而不是"锁"。
			_, err := db.Replace(ctx, "s1", []rig.Message{userMessage("替换")})
			return err
		}},
		{"Create", false, func(db *sqlitestore.Store) error {
			_, err := db.Create(ctx, sqlitestore.Meta{ID: "created"})
			return err
		}},
		{"Delete", false, func(db *sqlitestore.Store) error {
			return db.Delete(ctx, "s1")
		}},
		{"CheckpointSave", false, func(db *sqlitestore.Store) error {
			return db.Checkpoints().Save(ctx, "s1", &rig.Checkpoint{SessionID: "s1", TurnID: "t1", Phase: rig.CheckpointPhaseRequested})
		}},
		{"AppendEvent", false, func(db *sqlitestore.Store) error {
			return db.SessionLog().AppendEvent(ctx, rig.SessionEvent{SessionID: "s1", Type: "note"})
		}},
		{"SaveCost", false, func(db *sqlitestore.Store) error {
			return db.SaveCost(ctx, "s1", sqlitestore.CostEntry{Model: "m", CostUSD: 0.5})
		}},
		{"MemoryWrite", false, func(db *sqlitestore.Store) error {
			return db.Memory().Write(ctx, memory.Page{Name: "page", Summary: "摘要", Content: "正文"})
		}},
		{"MemoryAppend", true, func(db *sqlitestore.Store) error {
			return db.Memory().Append(ctx, "log", "一行")
		}},
		{"MemoryDelete", true, func(db *sqlitestore.Store) error {
			return db.Memory().Delete(ctx, "log")
		}},
		{"SpillSave", false, func(db *sqlitestore.Store) error {
			_, err := db.Spill().Save(ctx, rig.ToolCall{Name: "bash"}, "很大的输出")
			return err
		}},
		{"SpillGC", false, func(db *sqlitestore.Store) error {
			_, err := db.Spill().GC(ctx, 1)
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, path := openTemp(t, sqlitestore.Options{BusyTimeout: time.Millisecond})
			if tc.seed {
				if err := seed(db); err != nil {
					t.Fatalf("铺种子数据: %v", err)
				}
			}
			release := holdWriteLock(t, path)
			defer release()
			done := make(chan error, 1)
			go func() { done <- tc.call(db) }()
			time.Sleep(30 * time.Millisecond)
			release()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("%s 在写锁释放后仍然失败（没有重试）：%v", tc.name, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("%s 在 5s 内没有返回", tc.name)
			}
		})
	}
}
