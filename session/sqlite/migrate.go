package sqlite

// 把已有宿主从"目录存储"搬到"一个数据库"。导入逐条校验，并且**可以重复执行**：
// 已经导完的会话会被跳过，所以一次中断的迁移能接着跑，而不是把数据导两遍。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

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
			report.Skipped = append(report.Skipped, meta.ID)
			continue
		}
		if opts.DryRun {
			continue
		}
		if inDatabase {
			// 空壳（或半截）会话：不重新 Create，只把库内**还没有**的那一段按
			// ordinal 续上。整份历史重放会把已导入的消息再写一遍（3 条源、2 条库
			// → 5 条 `m1 m2 m1 m2 m3`），这是上一版的实际缺陷。
			remaining := messages[have:]
			if len(remaining) == 0 {
				report.Imported++
				continue
			}
			if _, appendErr := target.AppendBatch(ctx, meta.ID, remaining...); appendErr != nil {
				report.Failed = append(report.Failed, fmt.Sprintf("%s: %v", meta.ID, appendErr))
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
		report.Imported++
	}
	return report, nil
}
