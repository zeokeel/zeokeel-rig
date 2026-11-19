package sqlite

// 超大的工具结果：与产生它的会话放在同一个数据库里。模型只看到一个短句柄，
// 字节留在库里，并随这个库一起被备份/回收。

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
func (s spillStore) Save(ctx context.Context, call rig.ToolCall, content string) (rig.SpillRef, error) {
	var suffix [6]byte
	_, _ = rand.Read(suffix[:])
	now := time.Now().UTC()
	id := fmt.Sprintf("spill-%013d-%s", now.UnixMilli(), hex.EncodeToString(suffix[:]))
	if _, err := s.store.db.ExecContext(ctx,
		`INSERT INTO rig_spill(id, tool, bytes, content, created_at) VALUES(?, ?, ?, ?, ?)`,
		id, call.Name, len(content), content, now.UnixMilli()); err != nil {
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

// GC 只保留最新的 keep 条，返回删掉的行数。
//
// 按 seq 而不是 created_at 排序：同一毫秒写入的多条结果 created_at 相同，
// 按时间排序时"最新的是哪条"是不确定的；seq 由 AUTOINCREMENT 保证单调
// （不会因为删行而回退复用）。
func (s spillStore) GC(ctx context.Context, keep int) (int, error) {
	if keep <= 0 {
		return 0, nil
	}
	result, err := s.store.db.ExecContext(ctx,
		`DELETE FROM rig_spill WHERE id NOT IN (
		   SELECT id FROM rig_spill ORDER BY seq DESC LIMIT ?
		 )`, keep)
	if err != nil {
		return 0, fmt.Errorf("sqlite: collect spilled results: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sqlite: collect spilled results: %w", err)
	}
	return int(affected), nil
}
