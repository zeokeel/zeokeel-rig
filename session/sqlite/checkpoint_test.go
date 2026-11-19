package sqlite_test

// Close 的 WAL 收尾：关闭一个 WAL 模式库之前必须把 -wal 里的提交并回主库并截断，
// 否则用户"只拷 .db"得到的是缺最后一次写入的旧快照（见 backup_test.go 的反面用例）。

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	sqlitestore "github.com/zeokeel/zeokeel-rig/session/sqlite"
)

// TestCloseCheckpointsWAL 断言 Close 会做 PRAGMA wal_checkpoint(TRUNCATE)。
//
// 为什么故意开**两个** Store 指向同一个库：SQLite 自己在"最后一个连接正常关闭"
// 时会顺手 checkpoint 并删掉 -wal，于是单连接用例不管我们有没有显式 checkpoint
// 都会绿（这是本用例一开始的假绿）。只要还有第二个连接开着，关闭其中一个就**不会**
// 触发 SQLite 的自动收尾——此时 -wal 是否被截断，才是我们自己的 checkpoint 在
// 起作用的证据。
//
// 前置断言（写入后 -wal 非空）保证用例不会退化成"本来就没什么可 checkpoint 的"。
func TestCloseCheckpointsWAL(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "rig.sqlite")
	writer, err := sqlitestore.Open(sqlitestore.Options{Path: path})
	if err != nil {
		t.Fatalf("Open writer: %v", err)
	}
	// 旁观连接：它的存在让"最后一个连接关闭"这条 SQLite 内建路径不生效。
	bystander, err := sqlitestore.Open(sqlitestore.Options{Path: path})
	if err != nil {
		_ = writer.Close()
		t.Fatalf("Open bystander: %v", err)
	}
	defer func() { _ = bystander.Close() }()

	meta, err := writer.Create(ctx, sqlitestore.Meta{Title: "checkpoint"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := writer.AppendBatch(ctx, meta.ID,
		userMessage("第一句"),
		assistantMessage("第二句"),
	); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}

	wal := path + "-wal"
	info, err := os.Stat(wal)
	if err != nil {
		t.Fatalf("写入之后 -wal 不存在（本用例无法证明 checkpoint 生效）: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("写入之后 -wal 是空的（本用例无法证明 checkpoint 生效）")
	}

	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if info, err := os.Stat(wal); err == nil && info.Size() > 0 {
		t.Fatalf("Close 之后 -wal 仍有 %d 字节：没有做 wal_checkpoint(TRUNCATE)", info.Size())
	}

	// checkpoint 不能丢数据：旁观连接立刻就能读到全部写入。
	messages, err := bystander.History().Load(ctx, meta.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("close 之后消息数 = %d, want 2", len(messages))
	}
	if got := messages[1].Text(); got != "第二句" {
		t.Fatalf("close 之后第二条 = %q", got)
	}
}

// TestCloseIsIdempotentWithCheckpoint 保证新增的 checkpoint 不破坏"重复 Close 安全"
// 这条既有契约（第二次调用拿不到句柄，必须直接返回）。
func TestCloseIsIdempotentWithCheckpoint(t *testing.T) {
	store, _ := openTemp(t, sqlitestore.Options{})
	if err := store.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
