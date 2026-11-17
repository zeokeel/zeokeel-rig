package patch

// 本文件把 dry_run 的结果从"+N/-M 汇总"升级成"汇总 + 统一 diff 预览"。
//
// 为什么只加在 dry_run 上：模型看不到 diff 时只能猜自己改了什么，个人用户的真实
// 体验是"模型改完我才看到"。让 dry_run 顺带返回 diff，模型就能自查一次再决定要
// 不要真落盘——不需要给工具加参数，也不增加往返。
//
// 生成 diff 的逻辑不在这里：统一交给 internal/diffpreview（与审批弹窗用的是同一
// 份实现），本文件只负责排版、阈值与降级。

import (
	"fmt"
	"strings"

	"github.com/zeokeel/zeokeel-rig/internal/diffpreview"
)

// maxDryRunPreviewLines 是 dry_run 结果里 diff 预览的行数上限。
//
// 与审批弹窗的默认值（gate.DefaultDiffMaxLines = 200）保持一致：超过就只给
// 逐文件统计，避免把一次预览变成几 MB 的工具结果塞回模型上下文。
const maxDryRunPreviewLines = 200

// dryRunPreviewHeader 是 diff 预览段的小标题。
const dryRunPreviewHeader = "[dry-run] diff preview:"

// dryRunPreviewOmitted 是超阈值时的说明前缀（后面接原因）。
const dryRunPreviewOmitted = "[dry-run] diff preview omitted: "

// withDryRunPreview 在 dry_run 的汇总文本里插入 diff 预览。
//
// 插入位置是"汇总行之后、'no files were modified' 之前"，这样读起来是
// "将要做什么 → 具体 diff → 什么都没写"。
func withDryRunPreview(summary string, plans []plan, limit int) string {
	block := renderDryRunPreview(plans, limit)
	if block == "" {
		return summary
	}
	if index := strings.LastIndex(summary, dryRunTailLine); index >= 0 {
		return summary[:index] + block + summary[index:]
	}
	// 兜底：尾部约定变了也照样附上，而不是悄悄丢掉预览。
	return summary + block
}

// renderDryRunPreview 生成预览段（含尾部换行）；没有可展示内容时返回空串。
func renderDryRunPreview(plans []plan, limit int) string {
	changes := planChanges(plans)
	if len(changes) == 0 {
		return ""
	}
	if limit <= 0 {
		limit = maxDryRunPreviewLines
	}

	// 先把每个文件的展示文本准备好，同时统计行数。
	type item struct {
		change Change
		text   string
		note   string
	}
	items := make([]item, 0, len(changes))
	total := 0
	for _, change := range changes {
		text, err := diffpreview.PreviewChange(diffpreview.Change{
			Path:    change.Path,
			Op:      change.Op,
			Old:     change.Old,
			New:     change.New,
			Added:   change.Added,
			Removed: change.Removed,
		})
		if err != nil {
			text = ""
		}
		note := ""
		if change.MoveTo != "" {
			note = fmt.Sprintf("move %s -> %s", change.Path, change.MoveTo)
		}
		if text != "" && !strings.HasPrefix(text, "binary file changed") {
			total += strings.Count(text, "\n")
		}
		items = append(items, item{change: change, text: text, note: note})
	}

	var builder strings.Builder
	if total > limit {
		// 超阈值：只给逐文件统计，不展示 diff 正文。
		fmt.Fprintf(&builder, "%s%d lines exceeds the %d line limit:\n", dryRunPreviewOmitted, total, limit)
		for _, entry := range items {
			fmt.Fprintf(&builder, "  %-6s %s (+%d -%d)\n",
				entry.change.Op, entry.change.Path, entry.change.Added, entry.change.Removed)
			if entry.note != "" {
				fmt.Fprintf(&builder, "         %s\n", entry.note)
			}
		}
		return builder.String()
	}

	builder.WriteString(dryRunPreviewHeader)
	builder.WriteString("\n")
	wrote := false
	for _, entry := range items {
		switch {
		case entry.text == "" && entry.note == "":
			continue // 纯移动且内容未变之外的"无变化"：不占篇幅
		case entry.text == "":
			builder.WriteString(entry.note)
			builder.WriteString("\n")
		case strings.HasPrefix(entry.text, "binary file changed"):
			// diffpreview 的降级摘要不带路径，这里补上，否则模型不知道是哪个文件。
			fmt.Fprintf(&builder, "%s: %s", entry.change.Path, entry.text)
			if !strings.HasSuffix(entry.text, "\n") {
				builder.WriteString("\n")
			}
		default:
			builder.WriteString(entry.text)
		}
		if entry.note != "" && entry.text != "" {
			builder.WriteString(entry.note)
			builder.WriteString("\n")
		}
		wrote = true
	}
	if !wrote {
		return ""
	}
	if !strings.HasSuffix(builder.String(), "\n") {
		builder.WriteString("\n")
	}
	return builder.String()
}

// dryRunTailLine 是 dry_run 汇总的收尾行。
//
// summarize 与 withDryRunPreview 共用它：一个负责写、一个负责在它之前插入预览，
// 因此两边不可能因为文案改动而错位。
const dryRunTailLine = "[dry-run] no files were modified\n"
