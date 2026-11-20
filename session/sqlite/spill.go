package sqlite

// 超大的工具结果：与产生它的会话放在同一个数据库里。模型只看到一个短句柄，
// 字节留在库里，并随这个库一起被备份/回收。
//
// # 回收按作用域分区
//
// 一个库里放着所有会话的行，而 agent 每次 Save 之后都按 MaxFiles 调一次 GC。回收
// 必须只作用于**同一个作用域**（通常是会话 ID，见 rig.WithSpillScope）：全局裁剪
// 会让会话 A 的回收删掉会话 B 的行，B 的 `ref:` 句柄从此读不回来——句柄失效就是
// 模型的工具输出永久丢失。这与文件后端（agent/spill.go 的 pruneSpillFiles 只删同
// 前缀文件）是同一个承诺。

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	rig "github.com/zeokeel/zeokeel-rig"
)

// Spill 返回由本数据库承载的溢出结果存储。
func (s *Store) Spill() rig.SpillStore { return spillStore{store: s} }

// spillStore 用 rig_spill 实现 rig.SpillStore。
type spillStore struct{ store *Store }

var _ rig.SpillStore = spillStore{}

// Save 存下一条超大结果并返回它的句柄。
//
// 句柄里带毫秒时间戳与随机后缀：它要能被人读（"这是什么时候哪次调用的"），
// 又要保证同一毫秒里两次调用不会撞 id。
//
// 行上记下 ctx 的作用域（rig.SpillScope，通常是会话 ID），GC 只在这个作用域内
// 裁剪；ctx 上没有作用域时落空串，之后也只被空作用域的 GC 回收。
func (s spillStore) Save(ctx context.Context, call rig.ToolCall, content string) (rig.SpillRef, error) {
	var suffix [6]byte
	_, _ = rand.Read(suffix[:])
	now := time.Now().UTC()
	id := fmt.Sprintf("spill-%013d-%s", now.UnixMilli(), hex.EncodeToString(suffix[:]))
	scope := rig.SpillScope(ctx)
	// Save 在 agent 热路径上（每次工具结果过大都写一条），锁冲突时必须自己重试。
	err := retryBusy(ctx, func() error {
		_, execErr := s.store.db.ExecContext(ctx,
			`INSERT INTO rig_spill(id, tool, bytes, content, created_at, scope) VALUES(?, ?, ?, ?, ?, ?)`,
			id, call.Name, len(content), content, now.UnixMilli(), scope)
		return execErr
	})
	if err != nil {
		return rig.SpillRef{}, fmt.Errorf("sqlite: save spilled result: %w", err)
	}
	return rig.SpillRef{ID: id, Bytes: len(content)}, nil
}

// Load 返回句柄背后的正文；句柄不存在时返回 rig.ErrSpillNotFound。
//
// 它是 spill_read 工具的落地实现：模型拿到的 `ref:` 必须有一条真实的读回路径，
// 否则"结果太大，已存起来"就等于"结果永久丢失"。
func (s spillStore) Load(ctx context.Context, id string) (string, error) {
	var content string
	err := s.store.db.QueryRowContext(ctx, `SELECT content FROM rig_spill WHERE id = ?`, id).Scan(&content)
	if errors.Is(err, sql.ErrNoRows) {
		return "", rig.ErrSpillNotFound
	}
	if err != nil {
		return "", fmt.Errorf("sqlite: load spilled result %q: %w", id, err)
	}
	return content, nil
}

// GC 只保留**本作用域**里最新的 keep 条，返回删掉的行数。
//
// 作用域取自 ctx（rig.WithSpillScope，通常是会话 ID）：一个库里放着所有会话的
// 溢出结果，全局裁剪会让一个会话的 MaxFiles 删掉另一个会话的句柄。文件后端按
// 文件名里的作用域前缀分区，这里按 scope 列分区——两个后端行为一致。
//
// ctx 上没有作用域时作用域是空串，于是**只**回收空作用域的行（老数据与不经过
// agent 的调用方都落在这一桶）。不要把它实现成"空作用域就全库回收"：那正是旧的
// 全局裁剪，会把上面那个 bug 原样带回来。
//
// 按 seq 而不是 created_at 排序：同一毫秒写入的多条结果 created_at 相同，
// 按时间排序时"最新的是哪条"是不确定的；seq 由 AUTOINCREMENT 保证单调
// （不会因为删行而回退复用）。
func (s spillStore) GC(ctx context.Context, keep int) (int, error) {
	if keep <= 0 {
		return 0, nil
	}
	scope := rig.SpillScope(ctx)
	var result sql.Result
	err := retryBusy(ctx, func() error {
		var execErr error
		result, execErr = s.store.db.ExecContext(ctx,
			`DELETE FROM rig_spill WHERE scope = ? AND id NOT IN (
			   SELECT id FROM rig_spill WHERE scope = ? ORDER BY seq DESC LIMIT ?
			 )`, scope, scope, keep)
		return execErr
	})
	if err != nil {
		return 0, fmt.Errorf("sqlite: collect spilled results: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sqlite: collect spilled results: %w", err)
	}
	return int(affected), nil
}
