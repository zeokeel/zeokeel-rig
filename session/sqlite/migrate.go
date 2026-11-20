package sqlite

// 把已有宿主从"目录存储"搬到"一个数据库"。导入逐条校验，并且**可以重复执行**：
// 已经导完的会话会被跳过，所以一次中断的迁移能接着跑，而不是把数据导两遍。

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/session/store"
)

// ImportOptions 配置 ImportDir。
type ImportOptions struct {
	// Dir 是目录存储的根（宿主传给 store.NewDir 的那个值）。
	Dir string
	// DryRun 只读只数、不写库：宿主先用它给用户看预览。
	DryRun bool
}

// ImportReport 说明这次导入发现了什么、做了什么。
type ImportReport struct {
	// Sessions 是目录里一共有多少个会话。
	Sessions int
	// Messages 是读到的消息总数（试运行也计入）。
	Messages int
	// Imported 是本次真正写入的会话数；试运行、以及库里已有的会话都不计入。
	Imported int
	// Skipped 列出库里**已经完整**、因而被跳过的会话。
	Skipped []string
	// Failed 列出读不出来的会话及原因。一个会话坏掉不会中断整次迁移。
	Failed []string
	// Conflicts 列出"库内条数多于源"的会话及原因：源目录被截短、或者库与源不是
	// 同一份数据。这种情况下**不**追加（否则会产生重复），也**不**当作"已完整"静默
	// 跳过，而是留一条可诊断记录让宿主决定是否人工处理。
	Conflicts []string
}

// ImportDir 把目录存储里的每个会话复制进数据库。
func ImportDir(ctx context.Context, target *Store, opts ImportOptions) (ImportReport, error) {
	var report ImportReport
	if target == nil {
		return report, errors.New("sqlite: import target is nil")
	}
	dir := strings.TrimSpace(opts.Dir)
	if dir == "" {
		return report, errors.New("sqlite: ImportOptions.Dir is required")
	}
	// 路径不存在，多半是宿主配置写错了；store.NewDir 会**乐于**把它建出来，然后
	// 报告一次"什么都没导入"的空迁移。
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return report, fmt.Errorf("sqlite: import source %q is not a directory", dir)
	}
	source, err := store.NewDir(dir)
	if err != nil {
		return report, fmt.Errorf("sqlite: open source directory: %w", err)
	}
	defer func() { _ = source.Close() }()
	metas, err := source.List(ctx, store.Filter{})
	if err != nil {
		return report, fmt.Errorf("sqlite: list source sessions: %w", err)
	}
	// 最旧的先导，让数据库里的排序与源目录保持一致。
	sort.SliceStable(metas, func(i, j int) bool { return metas[i].CreatedAt.Before(metas[j].CreatedAt) })
	// 续跑判据：**库内消息数 ≥ 源消息数**才跳过。
	//
	// 只看"会话 ID 已存在"是不够的：上一次导入可能在 Create 成功、AppendBatch 失败
	// 的位置中断，库里留下一个空壳会话；那个会话会被永久跳过，源里的消息永远进不来
	// （实测 0/2）。所以这里按消息数判断，不满就按 ordinal 续追加。
	existing := map[string]int{}
	if current, err := target.List(ctx, Filter{}); err == nil {
		for _, meta := range current {
			existing[meta.ID] = meta.Messages
		}
	}
	for _, meta := range metas {
		report.Sessions++
		loaded, messages, loadErr := source.Load(ctx, meta.ID)
		if loadErr != nil {
			report.Failed = append(report.Failed, fmt.Sprintf("%s: %v", meta.ID, loadErr))
			continue
		}
		report.Messages += len(messages)
		have, inDatabase := existing[meta.ID]
		switch {
		case inDatabase && have > len(messages):
			// 库内比源还多：源被截短了，或者库与源根本不是同一份数据。追加会产生
			// 重复、截断又会丢数据，所以两者都不做——留一条可诊断记录让宿主决定，
			// 不要静默当作"已完整"（那会把真实的不一致藏起来）。
			report.Conflicts = append(report.Conflicts, fmt.Sprintf(
				"%s: 库内已有 %d 条，源只有 %d 条；源可能被截短或不是同一个源，本次没有续跑",
				meta.ID, have, len(messages)))
			continue
		case inDatabase && have == len(messages):
			// 等长不等于一致：库里可能是 `m1,X,m3`、源是 `m1,m2,m3`——条数一样，中间
			// 那条却不是源的。旧行为在这里当作"已完整"跳过，把分叉静默留在库里。
			//
			// 但这一档是**重跑迁移**的常见路径，不能每次都把整份历史读进内存。所以：
			//   1. 先查导入时落下的源指纹（rig_import_state，一行，主键查找）；
			//   2. 指纹相同且条数相同 → 直接 Skipped，**完全不碰消息表**；
			//   3. 指纹缺失（v4 之前导入的老库）或不同 → 退回 Load 全量逐条比：
			//      一致就把指纹补写进去（下一次重跑就走快路径），不一致就报冲突并
			//      定位到具体第几条。
			digest := importDigestFor(messages)
			storedDigest, storedCount, found, stateErr := storedImportDigest(ctx, target, meta.ID)
			if stateErr != nil {
				report.Failed = append(report.Failed, fmt.Sprintf("%s: %v", meta.ID, stateErr))
				continue
			}
			if found && storedDigest == digest && storedCount == len(messages) {
				report.Skipped = append(report.Skipped, meta.ID)
				continue
			}
			_, storedMessages, loadErr := target.Load(ctx, meta.ID)
			if loadErr != nil {
				report.Failed = append(report.Failed, fmt.Sprintf("%s: %v", meta.ID, loadErr))
				continue
			}
			if position, diverged := firstDivergence(storedMessages, messages); diverged {
				report.Conflicts = append(report.Conflicts, fmt.Sprintf(
					"session %s: existing message #%d does not match source (existing=%s, source=%s)",
					meta.ID, position+1, describeMessage(storedMessages[position]), describeMessage(messages[position])))
				continue
			}
			if len(storedMessages) != len(messages) {
				// 前面 List 给的计数与真正读出来的条数不一致（索引漂移）。这种库不适合
				// 继续按"等长"处理：宁可留一条诊断，也不要写入或静默跳过。
				report.Conflicts = append(report.Conflicts, fmt.Sprintf(
					"%s: 库里实际读到 %d 条，但索引计数与源都是 %d 条；请先修索引（Load/Rebuild）再迁移",
					meta.ID, len(storedMessages), len(messages)))
				continue
			}
			// 逐条一致：把指纹补写/刷新，下一次重跑就能走不读消息表的快路径。
			// 试运行不写（它承诺零写入），所以这一步也要跟着 opts.DryRun 走。
			if !opts.DryRun {
				if recordErr := recordImportDigest(ctx, target, meta.ID, digest, len(messages)); recordErr != nil {
					report.Failed = append(report.Failed, fmt.Sprintf("%s: %v", meta.ID, recordErr))
					continue
				}
			}
			report.Skipped = append(report.Skipped, meta.ID)
			continue
		case inDatabase && have > 0:
			// 真要续写了：先确认库内那几条**真的是源的前几条**。
			//
			// 只看条数是不够的。库里是 `m1,X`、源是 `m1,m2,m3` 时，按条数续写会得到
			// `m1,X,m3`——丢掉源里的 `m2`、还留着不属于源的 `X`，而且完全没有告警。
			// 这里按 ordinal 逐条比身份（rig.Message 没有 ID 字段，身份 = role +
			// 内容分片 + 工具调用关联，见 messageIdentity），不一致就整条会话跳过。
			//
			// 只在这个分支 Load 一次：已导入完整的会话（上面两个 case）不付这份代价。
			_, stored, storedErr := target.Load(ctx, meta.ID)
			if storedErr != nil {
				report.Failed = append(report.Failed, fmt.Sprintf("%s: %v", meta.ID, storedErr))
				continue
			}
			// 以实际读出来的条数为准（List 给的只是索引里的计数，可能是旧的）。
			have = len(stored)
			if have > len(messages) {
				report.Conflicts = append(report.Conflicts, fmt.Sprintf(
					"%s: 库内已有 %d 条，源只有 %d 条；源可能被截短或不是同一个源，本次没有续跑",
					meta.ID, have, len(messages)))
				continue
			}
			if have == len(messages) {
				report.Skipped = append(report.Skipped, meta.ID)
				continue
			}
			if position, diverged := firstDivergence(stored, messages); diverged {
				report.Conflicts = append(report.Conflicts, fmt.Sprintf(
					"session %s: existing message #%d does not match source (existing=%s, source=%s)",
					meta.ID, position+1, describeMessage(stored[position]), describeMessage(messages[position])))
				continue
			}
		}
		if opts.DryRun {
			continue
		}
		if inDatabase {
			// 空壳（或半截）会话：不重新 Create，只把库内**还没有**的那一段按
			// ordinal 续上（前缀已在上面的分支里校验过）。整份历史重放会把已导入的
			// 消息再写一遍（3 条源、2 条库 → 5 条 `m1 m2 m1 m2 m3`），这是更早版本的缺陷。
			remaining := messages[have:]
			if len(remaining) > 0 {
				if _, appendErr := target.AppendBatch(ctx, meta.ID, remaining...); appendErr != nil {
					report.Failed = append(report.Failed, fmt.Sprintf("%s: %v", meta.ID, appendErr))
					continue
				}
			}
			// 续写完成 = 库内历史与源一致：记下源指纹，下一次重跑就能走"不读消息表"的快路径。
			if recordErr := recordImportDigest(ctx, target, meta.ID, importDigestFor(messages), len(messages)); recordErr != nil {
				report.Failed = append(report.Failed, fmt.Sprintf("%s: %v", meta.ID, recordErr))
				continue
			}
			report.Imported++
			continue
		}
		imported, importErr := target.Create(ctx, Meta{
			ID:        meta.ID,
			Title:     loaded.Title,
			Workspace: loaded.Workspace,
			CreatedAt: loaded.CreatedAt,
			UpdatedAt: loaded.UpdatedAt,
		})
		if importErr != nil {
			report.Failed = append(report.Failed, fmt.Sprintf("%s: %v", meta.ID, importErr))
			continue
		}
		if len(messages) > 0 {
			if _, appendErr := target.AppendBatch(ctx, imported.ID, messages...); appendErr != nil {
				report.Failed = append(report.Failed, fmt.Sprintf("%s: %v", meta.ID, appendErr))
				continue
			}
		}
		// 全新导入完成：同样记下源指纹（下一次它就走"等长 + 指纹一致"的快路径）。
		if recordErr := recordImportDigest(ctx, target, imported.ID, importDigestFor(messages), len(messages)); recordErr != nil {
			report.Failed = append(report.Failed, fmt.Sprintf("%s: %v", meta.ID, recordErr))
			continue
		}
		report.Imported++
	}
	return report, nil
}

// firstDivergence 找出"库内消息"与"源消息"在 ordinal 上第一条不一致的位置。
//
// 只比较 existing 覆盖到的那一段（调用方保证 len(existing) < len(source)）。
// 返回的 index 是 0 基；日志里要加 1 才是人读的"第 n 条"。
func firstDivergence(existing, source []rig.Message) (int, bool) {
	limit := len(existing)
	if len(source) < limit {
		limit = len(source)
	}
	for index := 0; index < limit; index++ {
		if messageIdentity(existing[index]) != messageIdentity(source[index]) {
			return index, true
		}
	}
	return 0, false
}

// messageIdentity 返回一条消息的身份，用于判断"库里第 n 条"与"源第 n 条"是不是同一条。
//
// rig.Message 没有稳定 ID 字段，因此身份由 role + 内容分片 + 工具调用关联关系
// （ToolCalls 的 ID/Name/Arguments、tool 消息的 ToolCallID/Name）拼成。分隔符取不可打印
// 字节，避免 `a|b` 与 `a` + `b` 这类拼接歧义。
//
// Reasoning 刻意不参与：它只供展示、永不回放（见 rig.Message 的注释），不同版本
// 丢掉它不该让续跑失败。将来若 Message 有了稳定 ID，只需要改这一个函数。
func messageIdentity(message rig.Message) string {
	var builder strings.Builder
	builder.WriteString(string(message.Role))
	builder.WriteByte(0)
	for _, part := range message.Content {
		builder.WriteString(string(part.Type))
		builder.WriteByte(1)
		builder.WriteString(part.Text)
		builder.WriteByte(1)
		builder.WriteString(part.ImageURL)
		builder.WriteByte(1)
		builder.WriteString(part.Detail)
		builder.WriteByte(2)
	}
	builder.WriteString(message.ToolCallID)
	builder.WriteByte(2)
	builder.WriteString(message.Name)
	builder.WriteByte(2)
	for _, call := range message.ToolCalls {
		builder.WriteString(call.ID)
		builder.WriteByte(1)
		builder.WriteString(call.Name)
		builder.WriteByte(1)
		builder.WriteString(call.Arguments)
		builder.WriteByte(2)
	}
	return builder.String()
}

// describeMessage 给冲突日志用的短描述：单行、可读，够宿主判断"库里那条是什么"。
func describeMessage(message rig.Message) string {
	text := strings.ReplaceAll(strings.TrimSpace(message.Text()), "\n", "\\n")
	if runes := []rune(text); len(runes) > 40 {
		text = string(runes[:40]) + "…"
	}
	description := fmt.Sprintf("%s:%q", message.Role, text)
	if len(message.ToolCalls) > 0 {
		description += fmt.Sprintf(" +%d tool call(s)", len(message.ToolCalls))
	}
	if message.ToolCallID != "" {
		description += " tool_call_id=" + message.ToolCallID
	}
	return description
}

// importDigestFor 计算一份历史的内容指纹：对每条的 messageIdentity 依次喂进 sha256，
// **顺序敏感**（换序得到不同指纹）。
//
// 它给 ImportDir 的"等长档"用：库里只需存一行指纹，重跑迁移时比一行就够了，不必把
// 整份历史读进内存。每个身份前加长度前缀，避免"两条消息拼接"与"一条更长的消息"撞上。
func importDigestFor(messages []rig.Message) string {
	hash := sha256.New()
	for _, message := range messages {
		identity := messageIdentity(message)
		fmt.Fprintf(hash, "%d:", len(identity))
		hash.Write([]byte(identity))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// storedImportDigest 读取某个会话在导入时记录的源指纹。
//
// found=false 表示旧库（v4 之前）还没有这份记录，调用方应回退到"Load 全量逐条比"
// 并顺手补写。
func storedImportDigest(ctx context.Context, target *Store, id string) (digest string, count int, found bool, err error) {
	if err := target.usable(); err != nil {
		return "", 0, false, err
	}
	scanErr := target.db.QueryRowContext(ctx,
		`SELECT source_digest, message_count FROM rig_import_state WHERE session_id = ?`, id).
		Scan(&digest, &count)
	switch {
	case errors.Is(scanErr, sql.ErrNoRows):
		return "", 0, false, nil
	case scanErr != nil:
		return "", 0, false, fmt.Errorf("sqlite: read import state of %q: %w", id, scanErr)
	}
	return digest, count, true, nil
}

// recordImportDigest 记录/刷新"这个会话导入时源的指纹"。
//
// 写入发生在消息写入**之后**：中间崩溃只会留下"没有指纹"（下次回退全量比对，自愈），
// 不会留下"指纹说一致、实际不一致"的假阳性。
func recordImportDigest(ctx context.Context, target *Store, id, digest string, count int) error {
	if err := target.usable(); err != nil {
		return err
	}
	if _, err := target.db.ExecContext(ctx,
		`INSERT INTO rig_import_state(session_id, source_digest, message_count) VALUES(?, ?, ?)
		 ON CONFLICT(session_id) DO UPDATE SET source_digest = excluded.source_digest,
		                                      message_count = excluded.message_count`,
		id, digest, count); err != nil {
		return fmt.Errorf("sqlite: record import state of %q: %w", id, err)
	}
	return nil
}
