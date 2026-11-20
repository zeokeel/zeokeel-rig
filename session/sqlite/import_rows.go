package sqlite

// 通用导入入口：不经过 `session/store` 的目录（那是 ImportDir 的来源）。
//
// 为什么需要它：ImportDir 只能吃 NDJSON 目录，于是"从另一个库、从宿主的备份格式、
// 从导出的可选面"这三条路都没有入口——导出加了事件/花费/检查点之后，导入端就只能
// 干看着。ImportRows 把"一个会话的行"作为**数据**接进来：元数据 + 消息，外加可选的
// 事件、花费、检查点。
//
// 幂等是硬要求（备份会被重复投递）：消息按 **ordinal** 对账——源行的 ordinal 与
// 库内已有行的 ordinal 做集合比较，源里有而库里没有的位置（删过消息留下的空洞）
// 补齐，库里多出来的位置保留、不算冲突；只有"同一个 ordinal 上内容不同"才判
// Conflicts（复用 ImportReport）。事件按 (session_id, seq) 幂等，花费按整行相等
// 去重，检查点按主键覆盖。

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
	// StartOrdinal 是 Messages[0] 在源历史里的真实 ordinal。
	//
	// 零值（省略）表示"源是从 ordinal 0 开始的完整历史"——宿主删过头部消息、
	// 或源本身是截短的尾部导出时，这个前提就不成立：切片位置不再等于源 ordinal，
	// 补洞会把同一内容插到新 ordinal 上。这种导出把首条消息的真实 ordinal 填进来，
	// ImportRows 就能按真实坐标对账；没填时走保守路径（见 ImportRows 的注释）。
	//
	// 带 omitempty：省略该字段的老载荷在 JSON 里与之前逐字节相同。
	StartOrdinal int `json:"start_ordinal,omitempty"`
	// Events / Costs / Checkpoint 是可选面。
	Events     []rig.SessionEvent
	Costs      []CostEntry
	Checkpoint *rig.Checkpoint
}

// ImportRows 把一个会话的行导入 target。
//
// 消息这一面的对账键是 **ordinal**（源行的位置 → 库内已有的 ordinal），不是切片
// 位置：
//   - 库里没有这个会话：Create + 按 ordinal 落库（StartOrdinal 为 0 时就是
//     AppendBatch 的 0..n-1；非 0 时直接写到那些序号上）；
//   - 源里有、库里没有的 ordinal（宿主删过消息留下的空洞）：按该 ordinal 补齐，
//     高水位经 nextOrdinal 推到 > 最大值，续写不会复用序号；
//   - 库里有、源里没有的 ordinal（库里多出来的行）：保留，不算冲突——宿主自己的
//     删除/追加不是导入方要抹平的事；
//   - 同一个 ordinal 上内容不同：记进 Conflicts，**一条都不写**；
//   - 源里每一条都已在库内且一致：Skipped。
//
// 源 ordinal 从哪来：SessionRows.StartOrdinal（首条消息的真实序号），省略时默认 0，
// 也就是"源是从 ordinal 0 开始的完整历史"。这个前提无法验证，所以没给
// StartOrdinal 的载荷走**保守路径**：源里出现的空洞只有在"源的序号范围能覆盖到
// 库内最高的 ordinal"（len(源) > 库内最大 ordinal）时才补，否则无法确定源的位置
// 对应哪个库内坐标（典型：宿主删过头部消息，源只剩尾部那条），一律记
// Conflicts（`source history does not start at ordinal 0; cannot place messages
// into holes`）并零写入——写进去只会产生重复内容。给出 StartOrdinal 时不受这条
// 限制：坐标是源自己声明的，投递多少条都能对账。
//
// 可选面在消息这一段被接受之后才写（冲突时连事件都不落地，否则库会处在一个
// "历史是旧的、事件是新的"的半截状态）。
//
// 返回的 error 只表示"这次调用本身没法进行"（target 为 nil、已关闭、没有 ID、
// StartOrdinal 为负）；单个会话的问题（冲突、写失败）都在 ImportReport 里。
func ImportRows(ctx context.Context, target *Store, rows SessionRows) (ImportReport, error) {
	var report ImportReport
	if target == nil {
		return report, errors.New("sqlite: import target is nil")
	}
	if err := target.usable(); err != nil {
		return report, err
	}
	if rows.StartOrdinal < 0 {
		return report, fmt.Errorf("sqlite: ImportRows: SessionRows.StartOrdinal is %d; it must be >= 0", rows.StartOrdinal)
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

	stored, exists, err := storedMessagesWithOrdinals(ctx, target, id)
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
			// 源声明的起始序号不是 0 时不能 AppendBatch：那会从 ordinal 0 顺延，
			// 把"源里第一条是 #6"改写成 #1。按真实坐标落库，库里没有的那些
			// 更小的 ordinal 就是源自己缺的（不是这次导入要补的洞）。
			if rows.StartOrdinal == 0 {
				if _, err := target.AppendBatch(ctx, id, rows.Messages...); err != nil {
					report.Failed = append(report.Failed, fmt.Sprintf("%s: %v", id, err))
					return report, nil
				}
			} else {
				entries := make([]ordinalMessage, 0, len(rows.Messages))
				for index, message := range rows.Messages {
					entries = append(entries, ordinalMessage{ordinal: rows.StartOrdinal + index, message: message})
				}
				if _, err := insertMessagesAtOrdinals(ctx, target, id, entries); err != nil {
					report.Failed = append(report.Failed, fmt.Sprintf("%s: %v", id, err))
					return report, nil
				}
			}
		}
		report.Imported++
	default:
		byOrdinal := make(map[int]rig.Message, len(stored))
		maxStored := -1
		for _, row := range stored {
			byOrdinal[row.ordinal] = row.message
			if row.ordinal > maxStored {
				maxStored = row.ordinal
			}
		}
		missing := make([]ordinalMessage, 0, len(rows.Messages))
		for index, message := range rows.Messages {
			// 源坐标 = 起始序号 + 切片位置。StartOrdinal 缺省为 0，于是退化成
			// "第 i 条就是 ordinal i"——只在这种情况下切片位置才等于源 ordinal。
			ordinal := rows.StartOrdinal + index
			existing, present := byOrdinal[ordinal]
			if !present {
				// 源里有、库里没有：删过消息留下的空洞，补齐（不算冲突）。
				missing = append(missing, ordinalMessage{ordinal: ordinal, message: message})
				continue
			}
			if messageIdentity(existing) != messageIdentity(message) {
				// 同一个 ordinal 上内容不同：真正的分叉。既不覆盖也不跳过，留一条
				// 可诊断记录，且一条都不写。
				report.Conflicts = append(report.Conflicts, fmt.Sprintf(
					"session %s: existing message #%d does not match source (existing=%s, source=%s)",
					id, ordinal+1, describeMessage(existing), describeMessage(message)))
				return report, nil
			}
		}
		if len(missing) > 0 && rows.StartOrdinal == 0 && len(rows.Messages) <= maxStored {
			// 保守路径：源没有声明起始序号（= 声称自己是"从 ordinal 0 开始的完整
			// 历史"），但它的条数够不到库内最高的 ordinal。这说明源不是从 0 开始
			// 的（宿主删过头部消息、或是截短的尾部导出）：切片位置与源 ordinal 已经
			// 脱钩，补洞会把同一条内容插到新 ordinal 上，历史出现重复。
			//
			// 这时无法确定"源的第 i 条"该落在哪个库内坐标，所以只报冲突、零写入。
			// 源能给出真实序号时用 SessionRows.StartOrdinal，那条路不受这条限制。
			report.Conflicts = append(report.Conflicts, fmt.Sprintf(
				"session %s: source history does not start at ordinal 0; cannot place messages into holes (source has %d messages but the database already reaches ordinal %d); set SessionRows.StartOrdinal to the source's first ordinal if the export starts after 0",
				id, len(rows.Messages), maxStored))
			return report, nil
		}
		if len(missing) == 0 {
			report.Skipped = append(report.Skipped, id)
		} else {
			if _, err := insertMessagesAtOrdinals(ctx, target, id, missing); err != nil {
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

// storedMessage 是库里的一条消息与它的 ordinal。`Load` 只返回内容，而按 ordinal
// 对账必须看序号列，所以这里直接读消息表。
type storedMessage struct {
	ordinal int
	message rig.Message
}

// ordinalMessage 是一条"要落到指定 ordinal 的"源消息（补洞专用）。
type ordinalMessage struct {
	ordinal int
	message rig.Message
}

// storedMessagesWithOrdinals 读库内某个会话的消息与各自 ordinal（按 ordinal 升序）；
// exists=false 表示库里没有这个会话。
func storedMessagesWithOrdinals(ctx context.Context, target *Store, id string) ([]storedMessage, bool, error) {
	if _, err := target.meta(ctx, id); errors.Is(err, ErrNotFound) {
		return nil, false, nil
	} else if err != nil {
		return nil, false, err
	}
	rows, err := target.db.QueryContext(ctx,
		`SELECT ordinal, payload FROM rig_messages WHERE session_id = ? ORDER BY ordinal`, id)
	if err != nil {
		return nil, false, fmt.Errorf("sqlite: load ordinals of %q: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	var stored []storedMessage
	for rows.Next() {
		var (
			ordinal int
			payload []byte
		)
		if err := rows.Scan(&ordinal, &payload); err != nil {
			return nil, false, fmt.Errorf("sqlite: load ordinals of %q: %w", id, err)
		}
		var message rig.Message
		if err := json.Unmarshal(payload, &message); err != nil {
			return nil, false, fmt.Errorf("sqlite: load ordinals of %q: decode: %w", id, err)
		}
		stored = append(stored, storedMessage{ordinal: ordinal, message: message})
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("sqlite: load ordinals of %q: %w", id, err)
	}
	return stored, true, nil
}

// insertMessagesAtOrdinals 把源消息按各自的 ordinal 写进消息表（补洞专用）。
//
// 与 AppendBatch 的区别：AppendBatch 从高水位往后顺延，删过消息留下的空洞只能被
// 跳过去；这里的 ordinal 来自源坐标（`StartOrdinal + 切片位置`，缺省即为位置），
// 空洞正是要填的位置。高水位仍由 nextOrdinal 统一推进
// （`max(next_ordinal, MAX(ordinal)+1)`），所以补洞之后续写不会复用序号。
func insertMessagesAtOrdinals(ctx context.Context, target *Store, id string, entries []ordinalMessage) (int, error) {
	if len(entries) == 0 {
		return 0, nil
	}
	err := retryBusy(ctx, func() error {
		tx, err := target.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		now := time.Now().UTC()
		messages := make([]rig.Message, 0, len(entries))
		for _, entry := range entries {
			payload, marshalErr := json.Marshal(entry.message)
			if marshalErr != nil {
				return fmt.Errorf("encode message: %w", marshalErr)
			}
			searchText, searchCJK := searchColumnsFor(entry.message)
			if _, execErr := tx.ExecContext(ctx,
				`INSERT INTO rig_messages(session_id, ordinal, payload, search_text, search_cjk)
				 VALUES(?, ?, ?, ?, ?)`,
				id, entry.ordinal, payload, searchText, searchCJK); execErr != nil {
				return fmt.Errorf("insert message #%d: %w", entry.ordinal, execErr)
			}
			messages = append(messages, entry.message)
		}
		next, err := nextOrdinal(ctx, tx, id)
		if err != nil {
			return err
		}
		// 自动标题只落在**还没有消息**的会话上，与 AppendBatch 同一个条件。
		if _, err := tx.ExecContext(ctx,
			`UPDATE rig_sessions SET title = ? WHERE id = ? AND title = '' AND title_cleared = 0`,
			firstUserTitle(messages), id); err != nil {
			return fmt.Errorf("title: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE rig_sessions SET updated_at = ?, revision = revision + 1,
			        message_count = message_count + ?, next_ordinal = ? WHERE id = ?`,
			now.UnixMilli(), len(entries), next, id); err != nil {
			return fmt.Errorf("update session: %w", err)
		}
		return tx.Commit()
	})
	if err != nil {
		return 0, fmt.Errorf("sqlite: fill message holes: %w", err)
	}
	return len(entries), nil
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
			// 没有 seq 时按 (type, payload) 去重：重复导入不该把同一条事件追加第二遍。
			// 时间不参与判据（导出方没记时间时它会被填成"现在"）。
			var existing int
			if err := target.db.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM rig_events WHERE session_id = ? AND kind = ? AND payload = ?`,
				id, event.Type, []byte(payload)).Scan(&existing); err != nil {
				return written, fmt.Errorf("probe event %q: %w", event.Type, err)
			}
			if existing > 0 {
				continue
			}
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
		// 幂等判据：At 有值就按整行（含时间）比对；At 为零值（导出方没记时间）时
		// 只比对其余字段——否则每次导入都用"现在"填一个不同的时间，同一笔花费会被
		// 写进第二行，重复导入等于把账翻倍。
		at := entry.At
		var probeAt *time.Time
		if at.IsZero() {
			at = time.Now().UTC()
		} else {
			at = at.UTC()
			probeAt = &at
		}
		exists, err := costRowExists(ctx, target, id, entry, probeAt)
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
func costRowExists(ctx context.Context, target *Store, id string, entry CostEntry, at *time.Time) (bool, error) {
	query := `SELECT COUNT(*) FROM rig_costs
		 WHERE session_id = ? AND model = ? AND workspace_id = ? AND run_id = ?
		   AND prompt_tokens = ? AND completion_tokens = ? AND cached_tokens = ?
		   AND cost_usd = ?`
	args := []any{id, entry.Model, entry.WorkspaceID, entry.RunID, entry.PromptTokens,
		entry.CompletionTokens, entry.CachedTokens, entry.CostUSD}
	if at != nil {
		query += ` AND at = ?`
		args = append(args, at.UnixMilli())
	}
	var count int
	err := target.db.QueryRowContext(ctx, query, args...).Scan(&count)
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
