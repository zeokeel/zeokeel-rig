package sqlite

// 本文件是存储的"会话"那一半：侧边栏要列出的会话清单，以及一次 run 追加进去的
// 消息。每次写入都是**一个事务**，所以崩溃之后要么整批落下、要么一条都没有——不会
// 出现"有消息没有会话行"，也不会出现"消息计数与实际行数不一致"。

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	rig "github.com/zeokeel/zeokeel-rig"
)

// maxTitleRunes 与目录存储保持一致：标题是第一条用户消息的摘要，不是一段正文；
// 按**符文**截断，避免把一个汉字从中间劈开。
const maxTitleRunes = 120

// newSessionID 生成一个**可排序**的 ID：前面是毫秒时间戳，后面是随机后缀，于是
// 按 ID 排序即按创建时间排序，同时跨进程也不会撞号。
func newSessionID(now time.Time) string {
	var suffix [6]byte
	_, _ = rand.Read(suffix[:])
	return fmt.Sprintf("%013d-%s", now.UnixMilli(), hex.EncodeToString(suffix[:]))
}

// titleFrom 返回一条消息正文的摘要（去首尾空白、按符文截断）。
func titleFrom(msg rig.Message) string {
	text := strings.TrimSpace(messageText(msg))
	if text == "" {
		return ""
	}
	runes := []rune(text)
	if len(runes) > maxTitleRunes {
		runes = runes[:maxTitleRunes]
	}
	return strings.TrimSpace(string(runes))
}

// messageText 把一条消息里所有文本片段拼起来。
func messageText(msg rig.Message) string {
	var builder strings.Builder
	for _, part := range msg.Content {
		if part.Type == rig.PartText {
			builder.WriteString(part.Text)
		}
	}
	return builder.String()
}

// Create 插入一个会话并返回落库后的行。ID 为空时自动生成；显式给出 ID 时按其
// 原样写入（导入器依赖这一点）。
//
// **ID 已存在时返回 ErrExists**，与目录存储（store.Dir）的行为一致——宿主换构造器
// 时 errors.Is 的判断照常成立。此前这里是 `ON CONFLICT DO NOTHING` 静默成功，会让
// "新建同名会话"看起来成功、实际读回的是旧会话。
func (s *Store) Create(ctx context.Context, meta Meta) (Meta, error) {
	if err := s.usable(); err != nil {
		return Meta{}, err
	}
	now := time.Now().UTC()
	if strings.TrimSpace(meta.ID) == "" {
		meta.ID = newSessionID(now)
	}
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = now
	}
	if meta.UpdatedAt.IsZero() {
		meta.UpdatedAt = meta.CreatedAt
	}
	var err error
	// 写路径统一走 retryBusy：另一个连接握着写锁时按退避重试，仍失败则报 ErrBusy。
	err = retryBusy(ctx, func() error {
		_, execErr := s.db.ExecContext(ctx,
			`INSERT INTO rig_sessions(id, title, workspace_id, created_at, updated_at, message_count, bytes)
			 VALUES(?, ?, ?, ?, ?, ?, ?)`,
			meta.ID, meta.Title, meta.Workspace, meta.CreatedAt.UnixMilli(), meta.UpdatedAt.UnixMilli(), meta.Messages, meta.Bytes)
		return execErr
	})
	if err != nil {
		if isUniqueViolation(err) {
			return Meta{}, fmt.Errorf("sqlite: create session %q: %w", meta.ID, ErrExists)
		}
		return Meta{}, fmt.Errorf("sqlite: create session %q: %w", meta.ID, err)
	}
	return s.meta(ctx, meta.ID)
}

// Append 写入一条消息并返回更新后的行。
func (s *Store) Append(ctx context.Context, id string, msg rig.Message) (Meta, error) {
	return s.AppendBatch(ctx, id, msg)
}

// AppendBatch 在**一个事务**里写入多条消息：要么全部落下，要么一条都不写。
// 会话行不存在时顺带创建，这正是"从 UI 的新建对话开始一次 run"需要的行为。
func (s *Store) AppendBatch(ctx context.Context, id string, msgs ...rig.Message) (Meta, error) {
	if err := s.usable(); err != nil {
		return Meta{}, err
	}
	if len(msgs) == 0 {
		return s.meta(ctx, id)
	}
	// 整个事务放在 retryBusy 里：锁冲突时**必须重开事务**（重新读 MAX(ordinal)），
	// 而不是重试单条语句——沿用旧快照会把同一批消息插到已被别人占掉的序号上。
	err := retryBusy(ctx, func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		now := time.Now().UTC()
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO rig_sessions(id, title, workspace_id, created_at, updated_at)
			 VALUES(?, '', '', ?, ?) ON CONFLICT(id) DO NOTHING`,
			id, now.UnixMilli(), now.UnixMilli()); err != nil {
			return fmt.Errorf("ensure session %q: %w", id, err)
		}
		var next int
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(ordinal)+1, 0) FROM rig_messages WHERE session_id = ?`, id).Scan(&next); err != nil {
			return fmt.Errorf("next ordinal: %w", err)
		}
		added := 0
		for index, msg := range msgs {
			payload, err := json.Marshal(msg)
			if err != nil {
				return fmt.Errorf("encode message: %w", err)
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO rig_messages(session_id, ordinal, payload) VALUES(?, ?, ?)`,
				id, next+index, payload); err != nil {
				return fmt.Errorf("insert message: %w", err)
			}
			added++
		}
		// The title is the first user message and never changes afterwards: a
		// sidebar that renamed itself mid-conversation would be disorienting.
		if _, err := tx.ExecContext(ctx,
			`UPDATE rig_sessions SET title = ? WHERE id = ? AND title = ''`,
			firstUserTitle(msgs), id); err != nil {
			return fmt.Errorf("title: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE rig_sessions SET updated_at = ?, revision = revision + 1,
			        message_count = message_count + ? WHERE id = ?`,
			now.UnixMilli(), added, id); err != nil {
			return fmt.Errorf("update session: %w", err)
		}
		return tx.Commit()
	})
	if err != nil {
		return Meta{}, fmt.Errorf("sqlite: append: %w", err)
	}
	// 提交之后再回读，**必须**用不带取消的 ctx：调用方的 ctx 可能在这几微秒里被
	// 取消（超时、用户点了停止），那样我们会返回一个"取消错误"，而数据其实已经落库
	// ——宿主看到错误后重试，就会把同一批消息写两遍。
	return s.meta(context.WithoutCancel(ctx), id)
}

// firstUserTitle 返回这一批消息隐含的标题；当整批里没有带正文的用户消息时返回空串
// （例如以助手消息开场的回合，标题保持占位符）。
func firstUserTitle(msgs []rig.Message) string {
	for _, msg := range msgs {
		if msg.Role != rig.RoleUser {
			continue
		}
		if title := titleFrom(msg); title != "" {
			return title
		}
	}
	return ""
}

// MetaPatch 是 UpdateMeta 的补丁：nil 字段表示不改。
//
// 指针而不是值，是为了把"不改"和"改成空串"分开：`Title: nil` 保留原标题，
// `Title: ptr("")` 是**显式清空**——用户把自动生成的标题删掉时需要的正是后者。
type MetaPatch struct {
	// Title 是新的会话标题；nil 表示不改。
	Title *string
	// WorkspaceID 是新的工作区 ID；nil 表示不改。
	WorkspaceID *string
}

// UpdateMeta 修改一个会话的元数据（改名、换工作区），返回落库后的行。
//
// 语义：
//   - nil 字段不改；空串是显式清空（见 MetaPatch）；
//   - 未知会话返回 ErrNotFound——与 Load/meta 是**同一个哨兵**（宿主换存储实现时
//     errors.Is 照常成立）。空补丁也要先确认会话存在，否则宿主会以为"改成功了"；
//   - updated_at 与 revision 按 Append/Replace 的规矩维护：改名也是一次写，列表按
//     updated_at 倒序、revision 用来在同毫秒写入之间保持顺序确定。空补丁是纯回读，
//     两者都不推进（避免"什么都没改"把会话顶到列表最前面）。
func (s *Store) UpdateMeta(ctx context.Context, id string, patch MetaPatch) (Meta, error) {
	if err := s.usable(); err != nil {
		return Meta{}, err
	}
	var (
		sets []string
		args []any
	)
	if patch.Title != nil {
		sets = append(sets, "title = ?")
		args = append(args, *patch.Title)
	}
	if patch.WorkspaceID != nil {
		sets = append(sets, "workspace_id = ?")
		args = append(args, *patch.WorkspaceID)
	}
	if len(sets) == 0 {
		return s.meta(ctx, id)
	}
	sets = append(sets, "updated_at = ?", "revision = revision + 1")
	args = append(args, time.Now().UTC().UnixMilli(), id)
	// 与其余写路径一样走 retryBusy：另一个窗口正在跑回合时，改名不能被随机拒绝。
	err := retryBusy(ctx, func() error {
		result, execErr := s.db.ExecContext(ctx,
			`UPDATE rig_sessions SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
		if execErr != nil {
			return execErr
		}
		affected, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			return rowsErr
		}
		if affected == 0 {
			return ErrNotFound
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Meta{}, ErrNotFound
		}
		return Meta{}, fmt.Errorf("sqlite: update session %q: %w", id, err)
	}
	// 同 AppendBatch：提交之后回读用不带取消的 ctx，否则"取消错误 + 已写入"会让
	// 宿主重试一次改名。
	return s.meta(context.WithoutCancel(ctx), id)
}

// Load 返回一个会话及其全部消息（按 ordinal 升序）。
func (s *Store) Load(ctx context.Context, id string) (Meta, []rig.Message, error) {
	if err := s.usable(); err != nil {
		return Meta{}, nil, err
	}
	meta, err := s.meta(ctx, id)
	if err != nil {
		return Meta{}, nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT payload FROM rig_messages WHERE session_id = ? ORDER BY ordinal`, id)
	if err != nil {
		return Meta{}, nil, fmt.Errorf("sqlite: load %q: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	var messages []rig.Message
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return Meta{}, nil, fmt.Errorf("sqlite: load %q: %w", id, err)
		}
		var msg rig.Message
		if err := json.Unmarshal(payload, &msg); err != nil {
			return Meta{}, nil, fmt.Errorf("sqlite: load %q: decode: %w", id, err)
		}
		messages = append(messages, msg)
	}
	if err := rows.Err(); err != nil {
		return Meta{}, nil, fmt.Errorf("sqlite: load %q: %w", id, err)
	}
	return meta, messages, nil
}

// meta 读取一条会话行。
func (s *Store) meta(ctx context.Context, id string) (Meta, error) {
	var (
		meta      Meta
		createdAt int64
		updatedAt int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, title, workspace_id, created_at, updated_at, message_count, bytes
		 FROM rig_sessions WHERE id = ?`, id).
		Scan(&meta.ID, &meta.Title, &meta.Workspace, &createdAt, &updatedAt, &meta.Messages, &meta.Bytes)
	if errors.Is(err, sql.ErrNoRows) {
		return Meta{}, ErrNotFound
	}
	if err != nil {
		return Meta{}, fmt.Errorf("sqlite: read session %q: %w", id, err)
	}
	meta.CreatedAt = time.UnixMilli(createdAt).UTC()
	meta.UpdatedAt = time.UnixMilli(updatedAt).UTC()
	return meta, nil
}

// List 按"最近更新优先"返回会话，并应用过滤条件。它**只读 sessions 表**：消息
// 条数是列，所以列一千个会话也不会解码任何一条消息。
func (s *Store) List(ctx context.Context, filter Filter) ([]Meta, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	query := `SELECT id, title, workspace_id, created_at, updated_at, message_count, bytes FROM rig_sessions`
	var (
		where []string
		args  []any
	)
	if filter.Workspace != "" {
		where = append(where, "workspace_id = ?")
		args = append(args, filter.Workspace)
	}
	if !filter.Since.IsZero() {
		where = append(where, "updated_at >= ?")
		args = append(args, filter.Since.UnixMilli())
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY updated_at DESC, revision DESC"
	if filter.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, filter.Limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var metas []Meta
	for rows.Next() {
		var (
			meta      Meta
			createdAt int64
			updatedAt int64
		)
		if err := rows.Scan(&meta.ID, &meta.Title, &meta.Workspace, &createdAt, &updatedAt, &meta.Messages, &meta.Bytes); err != nil {
			return nil, fmt.Errorf("sqlite: list sessions: %w", err)
		}
		meta.CreatedAt = time.UnixMilli(createdAt).UTC()
		meta.UpdatedAt = time.UnixMilli(updatedAt).UTC()
		metas = append(metas, meta)
	}
	return metas, rows.Err()
}

// Delete 删除一个会话及其消息、事件与检查点。删除不存在的会话**不算错误**：用户
// 连点两下不该报错。
//
// 两条 DELETE 必须在**同一个事务**里：分开执行时，进程在中间崩溃会留下孤儿事件行，
// 而会话一旦以同一 ID 重建，这些旧事件就会"复活"到新会话上。
func (s *Store) Delete(ctx context.Context, id string) error {
	if err := s.usable(); err != nil {
		return err
	}
	// 两条 DELETE 必须同事务（见上面的注释），整个事务用 retryBusy 包住：锁冲突时
	// 重开事务，而不是让"用户连点两下删除"里的第二次随机失败。
	err := retryBusy(ctx, func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, `DELETE FROM rig_sessions WHERE id = ?`, id); err != nil {
			return fmt.Errorf("delete session %q: %w", id, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM rig_events WHERE session_id = ?`, id); err != nil {
			return fmt.Errorf("delete events of %q: %w", id, err)
		}
		return tx.Commit()
	})
	if err != nil {
		return fmt.Errorf("sqlite: delete session %q: %w", id, err)
	}
	return nil
}

// Replace 在一个事务里整体替换会话的历史：压缩会写入摘要、同时丢掉被替换掉的消息，
// 崩溃时不允许只完成一半。
func (s *Store) Replace(ctx context.Context, id string, msgs []rig.Message) (Meta, error) {
	if err := s.usable(); err != nil {
		return Meta{}, err
	}
	// 压缩路径同样要能挺过"另一个窗口正在写"：整个事务重开，不能只重试某一条。
	err := retryBusy(ctx, func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, `DELETE FROM rig_messages WHERE session_id = ?`, id); err != nil {
			return fmt.Errorf("clear: %w", err)
		}
		for index, msg := range msgs {
			payload, err := json.Marshal(msg)
			if err != nil {
				return fmt.Errorf("encode: %w", err)
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO rig_messages(session_id, ordinal, payload) VALUES(?, ?, ?)`,
				id, index, payload); err != nil {
				return fmt.Errorf("insert: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE rig_sessions SET message_count = ?, updated_at = ?, revision = revision + 1 WHERE id = ?`,
			len(msgs), time.Now().UTC().UnixMilli(), id); err != nil {
			return fmt.Errorf("update: %w", err)
		}
		return tx.Commit()
	})
	if err != nil {
		return Meta{}, fmt.Errorf("sqlite: replace: %w", err)
	}
	// 同 AppendBatch：提交之后用不带取消的 ctx 回读，否则"取消错误 + 已写入"会让
	// 宿主重试并写出第二份历史。
	return s.meta(context.WithoutCancel(ctx), id)
}

// usable 报告"已关闭"状态，避免空句柄直接 panic。
func (s *Store) usable() error {
	if s == nil {
		return ErrClosed
	}
	_, err := s.db.load()
	return err
}
