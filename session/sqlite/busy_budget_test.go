package sqlite_test

// 回归：busyRetryBudget 太短会让"重试"退化成"报错"——CI 的 -race 下 2s 会被打穿
// （TestHotWritePathsRetryOnBusy/MemoryAppend 实测）。这里用一个超过旧预算的持锁
// 时长证明重试真的在等锁。

import (
	"context"
	"testing"
	"time"

	rig "github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/memory"
	sqlitestore "github.com/zeokeel/zeokeel-rig/session/sqlite"
)

func TestBusyRetryOutlastsShortLockHolds(t *testing.T) {
	db, path := openTemp(t, sqlitestore.Options{BusyTimeout: time.Millisecond})
	// Append 追加到已有页面，先写一页种子（与 TestHotWritePathsRetryOnBusy 的 seed 一致）。
	if err := db.Memory().Write(context.Background(), memory.Page{
		Name: "log", Summary: "种子页", Content: "# log",
	}); err != nil {
		t.Fatalf("铺种子页: %v", err)
	}
	release := holdWriteLock(t, path)

	done := make(chan error, 1)
	go func() {
		done <- db.Memory().Append(context.Background(), "log", rig.NewMessage(rig.RoleUser, "一行").Text())
	}()

	// 持锁 2.5s：超过旧预算（2s），新预算（10s）应当仍能等到。
	time.Sleep(2500 * time.Millisecond)
	release()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("持锁 2.5s 后仍失败，重试预算太短: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("15s 内没有返回")
	}
}
