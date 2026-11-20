package sqlite

// 长期记忆（wiki）的 SQLite 后端：一页一行，索引是四列的排序读取，搜索是对
// 标题/摘要/正文的 LIKE。与文件后端共用同一套规则（`memory` 包导出的那些），
// 区别只在于"落盘"换成了"落库"。
//
// # 工作区分区
//
// 一个库常常被多个仓库共用。文件后端按 `<状态目录>/memory/<workspaceID>/` 分区，
// 这里用 `rig_memory_pages.workspace_id` 做同一件事：
//   - `Options.Workspace` 非空 → 本 store 的 wiki 只读写该工作区的行；
//   - 空串（既有行为）→ 不按工作区过滤，全部可见，升级前的老行（workspace_id 为空
//     串）照常读得出来。
//
// 表的主键仍然是 `name`（历史迁移段不能改，也不重建表），所以**物理**主键里拼上了
// 工作区前缀（见 storageName）：否则 A 仓与 B 仓各有一页 "build" 时会在主键上相撞，
// 后写的那个会静默覆盖前一个——那正是本功能要消灭的串味。
// 对外的页名始终是逻辑名（logicalMemoryName 拆掉前缀）。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig/memory"
)

// memoryScopeSep 是物理主键里工作区与页名之间的分隔符（ASCII 单元分隔符）。
//
// 页名只允许 `[a-z0-9-]`（见 memory.ValidateName），这个控制符不可能出现在逻辑页名
// 里，所以"工作区 + 分隔符 + 页名"的拼接与拆解都是无歧义的。
const memoryScopeSep = "\x1f"

// Memory 返回由本数据库承载的 wiki 后端（按 Options.Workspace 分区）。
func (s *Store) Memory() memory.Wiki {
	return memoryStore{store: s, workspace: s.workspace}
}

// memoryStore 用 rig_memory_pages 实现 memory.Wiki。
type memoryStore struct {
	store *Store
	// workspace 是本视图认领的工作区；空串 = 不分区（全部可见，既有行为）。
	workspace string
}

var _ memory.Wiki = memoryStore{}

// storageName 把逻辑页名映射成 rig_memory_pages 的物理主键。
func (m memoryStore) storageName(name string) string {
	if m.workspace == "" {
		return name
	}
	return m.workspace + memoryScopeSep + name
}

// logicalMemoryName 从物理主键取回逻辑页名（索引与检索结果展示用）。
func logicalMemoryName(stored string) string {
	if index := strings.LastIndex(stored, memoryScopeSep); index >= 0 {
		return stored[index+1:]
	}
	return stored
}

// scoped 返回"本视图的工作区条件"，ok=false 表示空工作区（不加条件）。
func (m memoryStore) scoped() (string, []any, bool) {
	if m.workspace == "" {
		return "", nil, false
	}
	return " AND workspace_id = ?", []any{m.workspace}, true
}

// Index 返回本视图里**全部**页面的元数据，最近修改的在前。
//
// 刻意不在 SQL 里写 LIMIT：契约要求 Index 返回完整目录，截断是工具层的事
// （它会自己算"还有 N 页没显示"）。在这里截到 60 会让工具层永远看不到尾巴，
// 于是"还有 5 页"这句话在数据库后端上永远不出现——用户以为只有 60 页。
//
// 空工作区视图里不同工作区的同名页会出现同名的多行元数据（它们确实是不同的页）；
// 需要精确读写某一页时请在对应工作区的 store 上做。
func (m memoryStore) Index(ctx context.Context) ([]memory.PageMeta, error) {
	if m.workspace == "" {
		return m.metas(ctx,
			`SELECT name, title, summary, updated_at, bytes FROM rig_memory_pages
			 ORDER BY updated_at DESC, name ASC`)
	}
	return m.metas(ctx,
		`SELECT name, title, summary, updated_at, bytes FROM rig_memory_pages
		 WHERE workspace_id = ?
		 ORDER BY updated_at DESC, name ASC`, m.workspace)
}

// Read 返回一页（含正文）。
func (m memoryStore) Read(ctx context.Context, name string) (memory.Page, error) {
	query := `SELECT name, title, summary, content, updated_at FROM rig_memory_pages WHERE name = ?`
	args := []any{m.storageName(name)}
	if clause, scopeArgs, ok := m.scoped(); ok {
		query += clause
		args = append(args, scopeArgs...)
	}
	var (
		page      memory.Page
		stored    string
		updatedAt int64
	)
	err := m.store.db.QueryRowContext(ctx, query, args...).
		Scan(&stored, &page.Title, &page.Summary, &page.Content, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return memory.Page{}, memory.ErrNotFound
	}
	if err != nil {
		return memory.Page{}, fmt.Errorf("sqlite: read memory %q: %w", name, err)
	}
	page.Name = logicalMemoryName(stored)
	page.Updated = time.UnixMilli(updatedAt).UTC()
	return page, nil
}

// Write 覆盖一页（不存在则创建）。
//
// 覆盖已有页时会在 Log 段追加一行 `rewritten (was N bytes)`，与文件后端一致：
// 读的人能看出这页是被重写过的，而不是长出来的。正文为空等于清空页面（契约允许，
// 想彻底删掉请用 Delete）。
func (m memoryStore) Write(ctx context.Context, page memory.Page) error {
	return m.writeLocked(ctx, page, true)
}

// writeLocked 是 Write 与 Append 共用的写入口。
//
// recordRewrite 决定"覆盖已有页"时要不要往 Log 段记一行重写说明：
//   - Write 传 true（用户明确覆盖，值得留痕）；
//   - Append 传 false（它只是在 Log 段追加，本来就不是"重写"，记一行
//     `rewritten (was N bytes)` 会让模型在正文里读到一条与事实不符的记录）。
func (m memoryStore) writeLocked(ctx context.Context, page memory.Page, recordRewrite bool) error {
	if err := memory.ValidateName(page.Name); err != nil {
		return err
	}
	summary, err := memory.NormalizeSummary(page.Summary)
	if err != nil {
		return err
	}
	updated := page.Updated
	if updated.IsZero() {
		updated = time.Now().UTC()
	} else {
		updated = updated.UTC()
	}
	title := memory.NormalizeTitle(page.Name, page.Title)
	stored := m.storageName(page.Name)
	// 读旧长度 + 写新正文必须在**一个事务**里：旧实现是"SELECT bytes"与
	// "INSERT … ON CONFLICT"两条裸语句，读与写之间别人提交的正文会被静默覆盖，
	// `rewritten (was N bytes)` 记下的 N 也成了一个从未紧挨着它存在过的长度。
	//
	// retryBusy 包住**整个事务**（而不是单条语句）：锁冲突重试时必须重开事务、
	// 重新读一次，否则重试用的是过期快照，等于把丢写换成了慢速丢写。
	err = retryBusy(ctx, func() error {
		tx, beginErr := m.store.db.BeginTx(ctx, nil)
		if beginErr != nil {
			return beginErr
		}
		defer func() { _ = tx.Rollback() }()
		content := page.Content
		if recordRewrite {
			var previous int
			readErr := tx.QueryRowContext(ctx,
				`SELECT bytes FROM rig_memory_pages WHERE name = ?`, stored).Scan(&previous)
			switch {
			case errors.Is(readErr, sql.ErrNoRows):
				// 新页：没有"之前"可记。
			case readErr != nil:
				return readErr
			default:
				content = memory.RewriteLog(content, previous)
			}
		}
		if _, execErr := tx.ExecContext(ctx,
			`INSERT INTO rig_memory_pages(name, workspace_id, title, summary, content, updated_at, bytes)
			 VALUES(?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(name) DO UPDATE SET
			   workspace_id = excluded.workspace_id,
			   title = excluded.title, summary = excluded.summary,
			   content = excluded.content, updated_at = excluded.updated_at, bytes = excluded.bytes`,
			stored, m.workspace, title, summary, content, updated.UnixMilli(), len(content)); execErr != nil {
			return execErr
		}
		return tx.Commit()
	})
	if err != nil {
		return fmt.Errorf("sqlite: write memory %q: %w", page.Name, err)
	}
	return nil
}

// Append 把 text 追加到页面的 Log 段（带日期前缀）。
//
// 页不存在时返回 ErrNotFound（与另外两个后端一致）：Append 不是"顺手建页"的
// 入口，否则模型拼错一次名字就会多出一页几乎空的记忆。
//
// 整个读-改-写在**一个事务**里完成，且第一条语句就是一次真正的写（`UPDATE` 同一
// 行、值不变）：SQLite 是延迟取锁的，先 `SELECT` 再 `UPDATE` 的"三步无事务"写法
// 会让 12 个并发 append 各自读到同一份旧正文、各自写回自己那份 → 只剩最后一行的
// 内容，而每一次都返回 nil。先写一次把这个库的写锁拿到手，后面读到的一定是最新值。
func (m memoryStore) Append(ctx context.Context, name, text string) error {
	if err := memory.ValidateName(name); err != nil {
		return err
	}
	stored := m.storageName(name)
	err := retryBusy(ctx, func() error {
		tx, beginErr := m.store.db.BeginTx(ctx, nil)
		if beginErr != nil {
			return beginErr
		}
		defer func() { _ = tx.Rollback() }()

		// 取写锁（值不变，只是为了让并发 append 串行化，见上面的注释）。
		if _, execErr := tx.ExecContext(ctx,
			`UPDATE rig_memory_pages SET updated_at = updated_at WHERE name = ?`, stored); execErr != nil {
			return execErr
		}
		var (
			content string
			summary string
		)
		readErr := tx.QueryRowContext(ctx,
			`SELECT content, summary FROM rig_memory_pages WHERE name = ?`, stored).Scan(&content, &summary)
		if errors.Is(readErr, sql.ErrNoRows) {
			return memory.ErrNotFound
		}
		if readErr != nil {
			return readErr
		}
		content = memory.AppendLog(content, text)
		if strings.TrimSpace(summary) == "" {
			// 页面是"先建后追加"建出来的（Write 时没有摘要是不可能的，所以这里通常
			// 出现在旧数据上）：用第一行补齐索引行，否则这一页在 index 里不可见。
			summary = firstLine(text)
		}
		updated := time.Now().UTC()
		if _, execErr := tx.ExecContext(ctx,
			`UPDATE rig_memory_pages SET content = ?, summary = ?, updated_at = ?, bytes = ? WHERE name = ?`,
			content, summary, updated.UnixMilli(), len(content), stored); execErr != nil {
			return execErr
		}
		return tx.Commit()
	})
	if err != nil {
		return fmt.Errorf("sqlite: append memory %q: %w", name, err)
	}
	return nil
}

// Delete 删除本视图里的一页；不存在时返回 ErrNotFound。
func (m memoryStore) Delete(ctx context.Context, name string) error {
	query := `DELETE FROM rig_memory_pages WHERE name = ?`
	args := []any{m.storageName(name)}
	if clause, scopeArgs, ok := m.scoped(); ok {
		query += clause
		args = append(args, scopeArgs...)
	}
	var result sql.Result
	err := retryBusy(ctx, func() error {
		var execErr error
		result, execErr = m.store.db.ExecContext(ctx, query, args...)
		return execErr
	})
	if err != nil {
		return fmt.Errorf("sqlite: delete memory %q: %w", name, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: delete memory %q: %w", name, err)
	}
	if affected == 0 {
		return memory.ErrNotFound
	}
	return nil
}

// Search 在本视图的标题/摘要/正文里找关键词，只返回元数据（不把正文拖进上下文）。
//
// 排序与另两个后端一致：**标题命中 > 摘要命中 > 只在正文命中**，同档再按最近修改。
// 只按 updated_at 排会让"名字就叫这个标题"的页面排在一条很久以前在正文里提过一次
// 的页面后面，模型据此读错页。
//
// 成本：正文过滤是全表 I/O（每条候选行都要读出 content 再 LIKE）。个人客户端的
// 记忆规模在几十到几百页，可以接受；要更快就得建全文索引（FTS5），那是另一次
// 契约变更。
func (m memoryStore) Search(ctx context.Context, query string) ([]memory.PageMeta, error) {
	text := strings.TrimSpace(query)
	if text == "" {
		return nil, nil
	}
	like := "%" + escapeLike(text) + "%"
	sql := `SELECT name, title, summary, updated_at, bytes FROM rig_memory_pages
	        WHERE (title LIKE ? ESCAPE '\' OR summary LIKE ? ESCAPE '\' OR content LIKE ? ESCAPE '\')`
	args := []any{like, like, like}
	if clause, scopeArgs, ok := m.scoped(); ok {
		sql += clause
		args = append(args, scopeArgs...)
	}
	sql += ` ORDER BY CASE
	                   WHEN title LIKE ? ESCAPE '\' THEN 0
	                   WHEN summary LIKE ? ESCAPE '\' THEN 1
	                   ELSE 2
	                 END,
	                 updated_at DESC, name ASC
	        LIMIT ?`
	args = append(args, like, like, memory.MaxSearchResults)
	return m.metas(ctx, sql, args...)
}

// metas 执行一条元数据查询，并把物理主键还原成逻辑页名。
func (m memoryStore) metas(ctx context.Context, query string, args ...any) ([]memory.PageMeta, error) {
	rows, err := m.store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: read memory index: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var metas []memory.PageMeta
	for rows.Next() {
		var (
			meta      memory.PageMeta
			stored    string
			updatedAt int64
		)
		if err := rows.Scan(&stored, &meta.Title, &meta.Summary, &updatedAt, &meta.Bytes); err != nil {
			return nil, fmt.Errorf("sqlite: read memory index: %w", err)
		}
		meta.Name = logicalMemoryName(stored)
		meta.Updated = time.UnixMilli(updatedAt).UTC()
		metas = append(metas, meta)
	}
	return metas, rows.Err()
}

// firstLine 是"追加顺带建页"时那一页的索引行。
//
// 按 **rune** 截断而不是字节：摘要上限是字节数没错，但直接切字节会把一个多字节
// 字符劈成两半，index 里就出现半个汉字（UTF-8 无效）。
func firstLine(text string) string {
	line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(text), "\n", 2)[0])
	if line == "" {
		return "log"
	}
	clamped, _ := memory.ClampText(line, memory.MaxSummaryBytes)
	return clamped
}
