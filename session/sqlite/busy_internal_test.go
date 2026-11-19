package sqlite

// 第 11 条的内部用例：isBusy / retryBusy 是未导出的实现细节，只能在包内断言。

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// 第 11 条：isBusy 能识别驱动的锁错误，retryBusy 不吞掉其它错误。
func TestBusyRetryBehaviour(t *testing.T) {
	ctx := context.Background()
	attempts := 0
	err := retryBusy(ctx, func() error {
		attempts++
		if attempts < 3 {
			return errors.New("database is locked (5) (SQLITE_BUSY)")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("retryBusy 在可重试错误后仍失败：%v", err)
	}
	if attempts != 3 {
		t.Fatalf("重试次数 = %d，want 3", attempts)
	}
	// ctx 取消之后不再重试。
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := retryBusy(cancelled, func() error { return errors.New("database is locked") }); !errors.Is(err, context.Canceled) {
		t.Fatalf("ctx 取消时 err = %v，want context.Canceled", err)
	}
	// 非锁错误原样返回。
	if err := retryBusy(ctx, func() error { return errors.New("syntax error") }); err == nil || strings.Contains(err.Error(), "locked") {
		t.Fatalf("非锁错误被改写：%v", err)
	}
}
