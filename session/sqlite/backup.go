package sqlite

// 备份：把整个库的**一致快照**写成一个可以直接打开的文件。
//
// # 为什么不是"拷 .db"
//
// 本包默认 WAL 模式（见 open.go 的 DSN）。WAL 模式下已提交的事务可能只在 `-wal`
// 里，主库文件还停在更早的状态。直接 `cp sess.db backup.db`（不带 `-wal`/`-shm`）
// 得到的是**缺最近提交的旧快照**；如果拷贝与写入/checkpoint 重叠，甚至可能拿到页
// 被撕裂的副本，打开时报 `database disk image is malformed`。两种结果都不能当备份。
//
// # 为什么是 VACUUM INTO
//
// VACUUM INTO 在**一个读事务**里读主库 + WAL 的当前内容，把结果写成一个全新的、
// 已整理过的数据库文件；它不需要先 checkpoint、不需要暂停写入，也不用宿主理解
// WAL。产物是自包含的：只拷这一个文件就能打开（见 backup_test.go）。
//
// 它与 Close 的分工：Close 的 wal_checkpoint(TRUNCATE) 解决"关库之后主库是完整的"，
// Backup 解决"运行中随时要一份能拿走的副本"。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// ErrBackupTargetExists 表示备份目标已经存在。VACUUM INTO 拒绝覆盖已有文件；
// 本方法把它提前成一个可判定的哨兵，宿主可以据此决定"换名字/让用户确认/先删除"。
var ErrBackupTargetExists = errors.New("sqlite: backup target already exists")

// Backup 用 `VACUUM INTO` 把当前库的一致快照写到 path。
//
// 约定：
//   - path 由宿主选择，**必须不存在**（覆盖等于悄悄毁掉上一份备份 → ErrBackupTargetExists）；
//   - 目录必须已存在（本包不替宿主创建目录）；
//   - 可以在库正常运行/有写入时调用：VACUUM INTO 读的是活的库，产物包含调用时刻已提交
//     的全部数据；
//   - 库已关闭时返回 ErrClosed（不会 panic）；
//   - ctx 被取消时返回 ctx 的错误（VACUUM INTO 是可中断的）。
//
// 产物是单个自包含文件：不依赖 `-wal`/`-shm` 陪跑文件即可打开。
func (s *Store) Backup(ctx context.Context, path string) error {
	target := strings.TrimSpace(path)
	if target == "" {
		return errors.New("sqlite: backup path is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// 先查目标：VACUUM INTO 遇到已存在的文件会失败，但错误文案是驱动的内部信息；
	// 宿主需要的是一个能分支处理的"目标已占用"。
	if _, err := os.Stat(target); err == nil {
		return fmt.Errorf("%w: %s", ErrBackupTargetExists, target)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("sqlite: backup target %s: %w", target, err)
	}
	db, err := s.handle()
	if err != nil {
		return err
	}
	// 参数化：路径里出现引号/反斜杠时不能被拼进 SQL 文本。
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, target); err != nil {
		// 失败时可能留下 0 字节文件；删掉它，避免宿主以为"备份成功但很小"。
		if _, statErr := os.Stat(target); statErr == nil {
			_ = os.Remove(target)
		}
		return fmt.Errorf("sqlite: backup to %s: %w", target, err)
	}
	return nil
}
