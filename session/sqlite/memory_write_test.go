package sqlite_test

// 记忆页的覆盖写：`Write` 是"读旧长度 → 写新正文"的读-改-写，必须整段在一个
// 事务里，并且重试时**整体重开**（重新读），否则重试读到的是过期快照。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/memory"
	sqlitestore "github.com/zeokeel/zeokeel-rig/session/sqlite"
)

// TestMemoryWriteIsAtomic 钉住两件事：
//
//  1. 撞上写锁时 `Write` 会重试，而不是把 SQLITE_BUSY 抛给调用方；
//  2. 重试重开整个事务——它读到的"上一版正文"必须是别人**已经提交**的那一版。
//     旧实现是"SELECT bytes … 然后 INSERT … ON CONFLICT"两步无事务：读与写之间
//     别人提交的正文会被静默覆盖，`rewritten (was N bytes)` 里记的 N 也是一个
//     从来没有紧挨着它存在过的长度（=丢写，而且丢得没有痕迹）。
//
// 断言选在 rewrite 账本上，是因为它是这次写入唯一可观测的"我读到了什么"。
func TestMemoryWriteIsAtomic(t *testing.T) {
	ctx := context.Background()
	db, path := openTemp(t, sqlitestore.Options{BusyTimeout: time.Millisecond})
	wiki := db.Memory()
	if err := wiki.Write(ctx, memory.Page{Name: "hot", Summary: "并发覆盖目标", Content: "A"}); err != nil {
		t.Fatalf("Write(初始): %v", err)
	}

	// 第二个进程（第二个连接）已经把这一页改成 137 字节，但还没提交。
	other, err := sqlitestore.Open(sqlitestore.Options{Path: path, BusyTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("打开第二个 store: %v", err)
	}
	defer func() { _ = other.Close() }()
	tx, err := other.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	fresh := strings.Repeat("B", 137)
	if _, err := tx.ExecContext(ctx,
		`UPDATE rig_memory_pages SET content = ?, bytes = ? WHERE name = ?`,
		fresh, len(fresh), "hot"); err != nil {
		_ = tx.Rollback()
		t.Fatalf("另一方写入: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- wiki.Write(ctx, memory.Page{Name: "hot", Summary: "覆盖", Content: strings.Repeat("C", 200)})
	}()
	// 让它是真的撞上锁（busy_timeout 只有 1ms）再提交。
	time.Sleep(30 * time.Millisecond)
	if err := tx.Commit(); err != nil {
		t.Fatalf("另一方提交: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("写锁释放后 Write 仍然失败（没有重试）：%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Write 在 5s 内没有返回")
	}

	page, err := wiki.Read(ctx, "hot")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !strings.Contains(page.Content, "rewritten (was 137 bytes)") {
		t.Fatalf("重试读到的不是已提交的那一版（丢写）：\n%s", page.Content)
	}
	metas, err := wiki.Index(ctx)
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if len(metas) != 1 || metas[0].Bytes != len(page.Content) {
		t.Fatalf("索引里的 bytes = %+v，want %d（行内长度必须与正文一致）", metas, len(page.Content))
	}
}
