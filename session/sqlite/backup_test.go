package sqlite_test

// Backup 的存在理由：WAL 模式下"用户自己拷 .db"不是一个安全动作。这里同时给出
// 正向契约（备份可独立打开、内容一致）与反面证据（直接拷主库会怎样）。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	sqlitestore "github.com/zeokeel/zeokeel-rig/session/sqlite"
)

// TestBackupProducesReadableCopy：Backup 之后打开备份文件，能读到同样数据。
func TestBackupProducesReadableCopy(t *testing.T) {
	ctx := context.Background()
	store, _ := openTemp(t, sqlitestore.Options{})
	meta, err := store.Create(ctx, sqlitestore.Meta{Title: "backup"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := store.AppendBatch(ctx, meta.ID,
		userMessage("要备份的第一句"),
		assistantMessage("第二句"),
	); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "backup.sqlite")
	if err := store.Backup(ctx, dest); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	// 备份必须**自带**全部内容：只拷这一个文件就能打开，不靠 -wal/-shm 陪跑。
	if _, err := os.Stat(dest + "-wal"); err == nil {
		t.Fatal("Backup 产物旁边出现了 -wal：副本不能依赖 WAL 陪跑文件")
	}
	if _, err := os.Stat(dest + "-shm"); err == nil {
		t.Fatal("Backup 产物旁边出现了 -shm：副本不能依赖共享内存文件")
	}

	backup, err := sqlitestore.Open(sqlitestore.Options{Path: dest})
	if err != nil {
		t.Fatalf("打开备份: %v", err)
	}
	defer func() { _ = backup.Close() }()
	messages, err := backup.History().Load(ctx, meta.ID)
	if err != nil {
		t.Fatalf("备份 Load: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("备份里消息数 = %d, want 2", len(messages))
	}
	if got := messages[0].Text(); got != "要备份的第一句" {
		t.Fatalf("备份里第一条 = %q", got)
	}

	// 目标已存在时必须拒绝，而不是覆盖（覆盖 = 悄悄毁掉上一份备份）。
	if err := store.Backup(ctx, dest); err == nil {
		t.Fatal("Backup 覆盖了已存在的目标文件")
	}
	// 关闭之后不再能备份：返回可判定的 ErrClosed，而不是 panic。
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := store.Backup(ctx, filepath.Join(t.TempDir(), "closed.sqlite")); !errors.Is(err, sqlitestore.ErrClosed) {
		t.Fatalf("关闭后 Backup = %v, want ErrClosed", err)
	}
}

// TestCopyWithoutCheckpointIsNotABackup 是**反面证据**，故意跳过。
//
// 为什么跳过而不是让它进 CI：直接拷主库得到的东西取决于"拷贝落在一次写入/
// checkpoint 的哪个瞬间"——
//
//  1. 能打开、但**静默缺最近提交**的旧快照（主库还没并回最新 WAL 帧）；
//  2. 页被撕裂的副本，打开时报 `database disk image is malformed`。
//
// 第 1 种是这里实测到的结果（modernc.org/sqlite v1.62.0，见下面的数字）：副本打开
// 完全正常，只是**一条消息都没有**——比报错更危险，因为用户会以为备份成功。第 2 种
// 需要拷贝与写入/checkpoint 并发，不可稳定复现，放进 CI 就是一条随机红的用例。
//
// 两种结果都说明"cp 主库"不是备份，所以结论写成注释而不是断言：想要"一个文件、打开
// 就能用"的副本，必须走 Backup（VACUUM INTO），或者 Close（wal_checkpoint(TRUNCATE)）
// 之后再拷。
//
// 实测代码（本机跑出来的输出）：
//
//	store, path := openTemp(t, Options{})
//	meta, _ := store.Create(ctx, Meta{})
//	for i := 0; i < 40; i++ { store.AppendBatch(ctx, meta.ID, userMessage("m")) }
//	os.Stat(path+"-wal")            // => 762232 字节：40 条写入全在 WAL 里
//	main, _ := os.ReadFile(path)    // 只拷主库
//	os.WriteFile(dest, main, 0o600)
//	cp, err := Open(Options{Path: dest})
//	//   err == nil（打开成功！不是 malformed）
//	msgs, _ := cp.History().Load(ctx, meta.ID)
//	//   len(msgs) == 0，而活库是 40 → 副本静默丢了**全部**写入
func TestCopyWithoutCheckpointIsNotABackup(t *testing.T) {
	t.Skip("反面证据：直接 cp 主库（不 checkpoint）得到的副本能打开，但静默缺少最近提交" +
		"（实测 40 条 → 0 条）；与写入并发时还可能 malformed。见本函数注释里的实测代码。")
}
