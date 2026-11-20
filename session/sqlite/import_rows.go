package sqlite

// 通用导入入口：不经过 `session/store` 的目录（那是 ImportDir 的来源）。
//
// 为什么需要它：ImportDir 只能吃 NDJSON 目录，于是"从另一个库、从宿主的备份格式、
// 从导出的可选面"这三条路都没有入口——导出加了事件/花费/检查点之后，导入端就只能
// 干看着。ImportRows 把"一个会话的行"作为**数据**接进来：元数据 + 消息，外加可选的
// 事件、花费、检查点。
//
// 幂等是硬要求（备份会被重复投递）：消息按 ordinal 逐条比身份，已一致就 Skipped；
// 只是前缀就续写；中间分叉既不覆盖也不静默跳过，而是给一条可读的 Conflicts（复用
// ImportReport）。事件按 (session_id, seq) 幂等，花费按整行相等去重，检查点按主键
// 覆盖。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	rig "github.com/zeokeel/zeokeel-rig"
)

// SessionRows 是一次导入的完整载荷：一个会话的元数据、消息，以及可选的事件、
// 花费与检查点（它们的零值表示"这次不导入这一面"）。
//
// Meta.ID 必填：没有 ID 的行集合无法与库内历史对账，ImportRows 会直接报错而不是
// 生成一个新会话（那会让每次导入都多出一个孤儿会话）。
type SessionRows struct {
	// Meta 是会话元数据（标题、工作区、时间戳）。
	Meta Meta
	// Messages 是完整历史（按 ordinal 升序）。
	Messages []rig.Message
	// Events / Costs / Checkpoint 是可选面。
	Events     []rig.SessionEvent
	Costs      []CostEntry
	Checkpoint *rig.Checkpoint
}

// ImportRows 把一个会话的行导入 target。
//
// 语义与 ImportDir 逐条一致：
//   - 库里没有这个会话：Create + AppendBatch（Meta 里的标题/工作区原样落库）；
//   - 库里有、且条数与内容都一致：Skipped（不重写消息，不产生第二份历史）；
//   - 库里有、且是投递内容的前缀：只续写缺的那一段；
//   - 库里的条数更多、或中间某条对不上：记进 Conflicts，**一条都不写**。
//
// 可选面在消息这一段被接受之后才写（冲突时连事件都不落地，否则库会处在一个
// "历史是旧的、事件是新的"的半截状态）。
//
// 返回的 error 只表示"这次调用本身没法进行"（target 为 nil、已关闭、没有 ID）；
// 单个会话的问题（冲突、写失败）都在 ImportReport 里。
func ImportRows(ctx context.Context, target *Store, rows SessionRows) (ImportReport, error) {
	var report ImportReport
	if target == nil {
		return report, errors.New("sqlite: import target is nil")
	}
	if err := target.usable(); err != nil {
		return report, err
	}
	id := strings.TrimSpace(rows.Meta.ID)
	if id == "" {
		return report, errors.New("sqlite: ImportRows needs SessionRows.Meta.ID: rows without a session id cannot be reconciled")
	}
	if rows.Checkpoint != nil && rows.Checkpoint.SessionID != "" && rows.Checkpoint.SessionID != id {
		return report, fmt.Errorf("sqlite: ImportRows: checkpoint belongs to %q, not %q",
			rows.Checkpoint.SessionID, id)
	}
	report.Sessions = 1
	report.Messages = len(rows.Messages)

	stored, exists, err := storedMessagesFor(ctx, target, id)
	if err != nil {
		return report, err
	}
	switch {
	case !exists:
		meta := rows.Meta
		meta.ID = id
		if _, err := target.Create(ctx, meta); err != nil {
			report.Failed = append(report.Failed, fmt.Sprintf("%s: %v", id, err))
			return report, nil
		}
		if len(rows.Messages) > 0 {
			if _, err := target.AppendBatch(ctx, id, rows.Messages...); err != nil {
				report.Failed = append(report.Failed, fmt.Sprintf("%s: %v", id, err))
				return report, nil
			}
		}
		report.Imported++
	default:
		switch {
		case len(stored) > len(rows.Messages):
			// 库内比投递的多：投递的那份被截短了，或者根本不是同一个会话。追加会
			// 产生重复、截断会丢数据，所以两者都不做——留一条可诊断记录。
			report.Conflicts = append(report.Conflicts, fmt.Sprintf(
				"%s: 库内已有 %d 条，投递的只有 %d 条；那份可能被截短或不是同一个会话，本次没有写入",
				id, len(stored), len(rows.Messages)))
			return report, nil
		}
		if position, diverged := firstDivergence(stored, rows.Messages); diverged {
			report.Conflicts = append(report.Conflicts, fmt.Sprintf(
				"session %s: existing message #%d does not match source (existing=%s, source=%s)",
				id, position+1, describeMessage(stored[position]), describeMessage(rows.Messages[position])))
			return report, nil
		}
		if len(stored) == len(rows.Messages) {
			report.Skipped = append(report.Skipped, id)
		} else {
			if _, err := target.AppendBatch(ctx, id, rows.Messages[len(stored):]...); err != nil {
				report.Failed = append(report.Failed, fmt.Sprintf("%s: %v", id, err))
				return report, nil
			}
			report.Imported++
		}
		// 消息已对账：元数据按投递值对齐（空值视为"没提供"，不清空已有标题）。
		if _, err := applyImportedMeta(ctx, target, id, rows.Meta, false); err != nil {
			report.Failed = append(report.Failed, fmt.Sprintf("%s: %v", id, err))
			return report, nil
		}
	}

	if rows.Events != nil {
		written, err := importEvents(ctx, target, id, rows.Events)
		if err != nil {
			report.Failed = append(report.Failed, fmt.Sprintf("%s: %v", id, err))
			return report, nil
		}
		report.Events += written
	}
	if rows.Costs != nil {
		written, err := importCosts(ctx, target, id, rows.Costs)
		if err != nil {
			report.Failed = append(report.Failed, fmt.Sprintf("%s: %v", id, err))
			return report, nil
		}
		report.Costs += written
	}
	if rows.Checkpoint != nil {
		written, err := importCheckpoint(ctx, target, id, rows.Checkpoint)
		if err != nil {
			report.Failed = append(report.Failed, fmt.Sprintf("%s: %v", id, err))
			return report, nil
		}
		report.Checkpoints += written
	}
	return report, nil
}

// storedMessagesFor 读库内某个会话的消息；exists=false 表示库里没有这个会话。
func storedMessagesFor(ctx context.Context, target *Store, id string) ([]rig.Message, bool, error) {
	_, messages, err := target.Load(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return messages, true, nil
}

// applyImportedMeta 把导入载荷里的元数据对齐到库里。
//
// exact 决定空值的含义：
//   - false（ImportRows）：空标题/空工作区视为"没提供"，不清空已有值——通用导入
//     的调用方常常只关心消息；
//   - true（导出目录的元数据清单）：清单是来源的事实，空就是空（用户显式清过标题
//     的那种情况必须能还原）。
//
// 只有真的不同才 UpdateMeta：它会把 updated_at/revision 往前推，重复导入同一份
// 数据不该把会话顶到列表最前面。
func applyImportedMeta(ctx context.Context, target *Store, id string, want Meta, exact bool) (bool, error) {
	stored, err := target.meta(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	patch := MetaPatch{}
	if want.Title != stored.Title && (exact || want.Title != "") {
		title := want.Title
		patch.Title = &title
	}
	if want.Workspace != stored.Workspace && (exact || want.Workspace != "") {
		workspace := want.Workspace
		patch.WorkspaceID = &workspace
	}
	if patch.Title == nil && patch.WorkspaceID == nil {
		return false, nil
	}
	if _, err := target.UpdateMeta(ctx, id, patch); err != nil {
		return false, err
	}
	return true, nil
}

// importEvents 写入事件：seq 已给出时按 (session_id, seq) 幂等（重复导入不追加），
// 没给出时顺延 AppendEvent。返回真正新写入的行数。
func importEvents(ctx context.Context, target *Store, id string, events []rig.SessionEvent) (int, error) {
	written := 0
	for _, event := range events {
		if event.Type == "" {
			return written, errors.New("event has no type")
		}
		payload := event.Payload
		if len(payload) == 0 {
			payload = json.RawMessage(`{}`)
		}
		at := event.Time
		if at.IsZero() {
			at = time.Now().UTC()
		}
		if event.Seq <= 0 {
			if err := target.SessionLog().AppendEvent(ctx, rig.SessionEvent{
				SessionID: id, Type: event.Type, Time: at, Payload: payload,
			}); err != nil {
				return written, err
			}
			written++
			continue
		}
		result, err := target.db.ExecContext(ctx,
			`INSERT INTO rig_events(session_id, seq, kind, at, payload) VALUES(?, ?, ?, ?, ?)
			 ON CONFLICT(session_id, seq) DO NOTHING`,
			id, event.Seq, event.Type, at.UnixMilli(), []byte(payload))
		if err != nil {
			return written, fmt.Errorf("insert event #%d: %w", event.Seq, err)
		}
		if affected, rowsErr := result.RowsAffected(); rowsErr == nil {
			written += int(affected)
		}
	}
	return written, nil
}

// importCosts 写入花费行：按整行相等去重（库里已有完全相同的一行就跳过），返回
// 真正新写入的行数。
//
// 为什么不做"先删该会话的全部花费再重写"：那会把宿主自己补记的花费一起删掉，而
// 重复导入本该是"什么都不变"。
func importCosts(ctx context.Context, target *Store, id string, costs []CostEntry) (int, error) {
	written := 0
	for _, entry := range costs {
		at := entry.At
		if at.IsZero() {
			at = time.Now().UTC()
		} else {
			at = at.UTC()
		}
		exists, err := costRowExists(ctx, target, id, entry, at)
		if err != nil {
			return written, err
		}
		if exists {
			continue
		}
		if err := target.SaveCost(ctx, id, CostEntry{
			Model:            entry.Model,
			WorkspaceID:      entry.WorkspaceID,
			RunID:            entry.RunID,
			PromptTokens:     entry.PromptTokens,
			CompletionTokens: entry.CompletionTokens,
			CachedTokens:     entry.CachedTokens,
			CostUSD:          entry.CostUSD,
			At:               at,
		}); err != nil {
			return written, err
		}
		written++
	}
	return written, nil
}

// costRowExists 报告库里是否已有与 entry 完全相同的花费行。
func costRowExists(ctx context.Context, target *Store, id string, entry CostEntry, at time.Time) (bool, error) {
	var count int
	err := target.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rig_costs
		 WHERE session_id = ? AND model = ? AND workspace_id = ? AND run_id = ?
		   AND prompt_tokens = ? AND completion_tokens = ? AND cached_tokens = ?
		   AND cost_usd = ? AND at = ?`,
		id, entry.Model, entry.WorkspaceID, entry.RunID, entry.PromptTokens,
		entry.CompletionTokens, entry.CachedTokens, entry.CostUSD, at.UnixMilli()).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("probe cost row: %w", err)
	}
	return count > 0, nil
}

// importCheckpoint 覆盖写检查点（一个会话只有一行，天然幂等）。
func importCheckpoint(ctx context.Context, target *Store, id string, checkpoint *rig.Checkpoint) (int, error) {
	if checkpoint == nil {
		return 0, nil
	}
	restored := *checkpoint
	restored.SessionID = id
	if err := target.Checkpoints().Save(ctx, id, &restored); err != nil {
		return 0, err
	}
	return 1, nil
}

// importExportedExtras 是 ImportDir 的第二遍：把导出目录里的会话元数据清单与三个
// 可选面导入 target。
//
// 第一遍（消息）由 ImportDir 自己完成——它带着续跑、指纹与冲突的全部逻辑；这一遍
// 只处理消息之外的东西，并且**只对库里确实存在的会话**生效（可选面文件对一个不在
// 库里的会话没有意义，硬写进去就是孤儿行）。
//
// 清单/文件读不懂时记进 Failed 而不是让整次导入失败：一个坏掉的附加文件不该把
// 已经导好的历史作废。
func importExportedExtras(ctx context.Context, dir string, target *Store, report *ImportReport) {
	manifest, err := readExportManifest(dir)
	if err != nil {
		report.Failed = append(report.Failed, fmt.Sprintf("%s: %v", exportManifestFile, err))
	} else {
		ids := make([]string, 0, len(manifest))
		for id := range manifest {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			record := manifest[id]
			want := Meta{ID: id, Title: record.Title, Workspace: record.Workspace}
			if _, err := applyImportedMeta(ctx, target, id, want, true); err != nil {
				report.Failed = append(report.Failed, fmt.Sprintf("%s: %v", id, err))
			}
		}
	}
	// 三个可选面：目录名 → 读取器 → 导入器。
	for _, face := range []struct {
		dir   string
		label string
	}{
		{exportEventsDir, exportFaceEvents},
		{exportCostsDir, exportFaceCosts},
		{exportCheckpointsDir, exportFaceCheckpoints},
	} {
		files, globErr := filepath.Glob(filepath.Join(dir, face.dir, "*"+exportFaceExt))
		if globErr != nil {
			report.Failed = append(report.Failed, fmt.Sprintf("%s: %v", face.dir, globErr))
			continue
		}
		sort.Strings(files)
		for _, file := range files {
			if err := importExportedFaceFile(ctx, target, face.dir, file, report); err != nil {
				report.Failed = append(report.Failed, fmt.Sprintf("%s: %v", filepath.Base(file), err))
			}
		}
	}
}

// importExportedFaceFile 导入一个可选面文件（事件 / 花费 / 检查点）。
func importExportedFaceFile(ctx context.Context, target *Store, face, file string, report *ImportReport) error {
	var (
		id  string
		err error
	)
	switch face {
	case exportEventsDir:
		var events []rig.SessionEvent
		id, events, err = readExportEvents(file)
		if err != nil {
			return err
		}
		if err := requireImportedSession(ctx, target, id); err != nil {
			return err
		}
		written, err := importEvents(ctx, target, id, events)
		if err != nil {
			return err
		}
		report.Events += written
	case exportCostsDir:
		var costs []CostEntry
		id, costs, err = readExportCosts(file)
		if err != nil {
			return err
		}
		if err := requireImportedSession(ctx, target, id); err != nil {
			return err
		}
		written, err := importCosts(ctx, target, id, costs)
		if err != nil {
			return err
		}
		report.Costs += written
	case exportCheckpointsDir:
		var checkpoint *rig.Checkpoint
		id, checkpoint, err = readExportCheckpoint(file)
		if err != nil {
			return err
		}
		if checkpoint == nil {
			return nil
		}
		if err := requireImportedSession(ctx, target, id); err != nil {
			return err
		}
		written, err := importCheckpoint(ctx, target, id, checkpoint)
		if err != nil {
			return err
		}
		report.Checkpoints += written
	default:
		return fmt.Errorf("unknown export face %q", face)
	}
	return nil
}

// requireImportedSession 确认可选面文件指向的会话已经在库里。
func requireImportedSession(ctx context.Context, target *Store, id string) error {
	if _, err := target.meta(ctx, id); err != nil {
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("会话 %q 不在库里（可选面文件没有对应的消息文件）", id)
		}
		return err
	}
	return nil
}
