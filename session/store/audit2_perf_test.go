package store_test

// 第 2D 轮审计：10k 条消息之后的 Load 耗时。

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/session/store"
)

// 追加 10k 条消息，然后测 Load 耗时与条数。
func TestAuditPerfLoadAfter10kMessages(t *testing.T) {
	dir := t.TempDir()
	opened := openStore(t, dir)
	meta, err := opened.Create(context.Background(), store.Meta{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	const total = 10000
	startAppend := time.Now()
	for i := 0; i < total; i++ {
		if _, err := opened.Append(context.Background(), meta.ID, user(fmt.Sprintf("message %05d payload", i))); err != nil {
			t.Fatalf("Append(%d): %v", i, err)
		}
	}
	appendElapsed := time.Since(startAppend)

	startLoad := time.Now()
	loadedMeta, messages, err := opened.Load(context.Background(), meta.ID)
	loadElapsed := time.Since(startLoad)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(messages) != total {
		t.Fatalf("Load 条数 = %d, want %d", len(messages), total)
	}
	if loadedMeta.Messages != total {
		t.Fatalf("Meta.Messages = %d, want %d", loadedMeta.Messages, total)
	}
	t.Logf("10k 条：追加 %v（平均 %v/条），Load %v，文件 %d 字节",
		appendElapsed, appendElapsed/total, loadElapsed, loadedMeta.Bytes)

	// 重开（扫目录 + Load）也必须快：这是产品里"打开会话列表"的路径。
	_ = opened.Close()
	startReopen := time.Now()
	reopened := openStore(t, dir)
	_, messages, err = reopened.Load(context.Background(), meta.ID)
	reopenElapsed := time.Since(startReopen)
	if err != nil {
		t.Fatalf("重开 Load: %v", err)
	}
	if len(messages) != total {
		t.Fatalf("重开 Load 条数 = %d", len(messages))
	}
	t.Logf("重开 + Load：%v", reopenElapsed)
	if loadElapsed > 2*time.Second || reopenElapsed > 4*time.Second {
		t.Fatalf("Load 太慢：%v / 重开 %v", loadElapsed, reopenElapsed)
	}
}
