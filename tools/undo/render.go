package undo

import (
	"fmt"
	"strings"
)

// renderResult 生成模型可见的结果文本。
//
// 稳定的形状（逐字被测试钉住）：
//
//	undo: reverted 2 change(s) from turn 1
//	  M src/a.go (+1/-1)
//	  A docs/new.md (+4/-0)
//	warning: 1 change(s) were not backed up and were left as-is:
//	  ! data/blob.bin: not-backed-up: binary file (contains NUL)
//
// 三类结果分开呈现，避免把"改不动"伪装成"改好了"：
//
//   - 已回退（Reverted）→ 结果行；类型变化用 `(type: file->symlink)` 标注；
//   - not-backed-up（Reason 非空）→ warning 段，说明为什么没回退；
//   - 硬链接同组但未被回退（HardlinkOf 非空）→ 另一段 warning，说明硬链接已被断开。
//
// dry-run 用 would revert 开头并在末尾标注没有落盘；没有变更时只输出一行
// "undo: no changes from turn N"。
func renderResult(turn int, dryRun bool, path string, changes []Change) string {
	restorable := 0
	skipped := make([]Change, 0, len(changes))
	affected := make([]Change, 0, len(changes))
	for _, change := range changes {
		switch {
		case change.HardlinkOf != "":
			// 硬链接同组但未被回退的路径：是警告，不是"已回退"也不是"跳过"。
			affected = append(affected, change)
		case change.Reason != "":
			skipped = append(skipped, change)
		default:
			restorable++
		}
	}

	var builder strings.Builder
	switch {
	case len(changes) == 0:
		fmt.Fprintf(&builder, "undo: no changes from turn %d", turn)
	case dryRun:
		fmt.Fprintf(&builder, "undo: would revert %d change(s) from turn %d", restorable, turn)
	default:
		fmt.Fprintf(&builder, "undo: reverted %d change(s) from turn %d", restorable, turn)
	}
	if len(changes) > 0 && len(skipped) > 0 {
		fmt.Fprintf(&builder, ", skipped %d", len(skipped))
	}
	if len(changes) > 0 && len(affected) > 0 {
		fmt.Fprintf(&builder, ", affected %d", len(affected))
	}
	if path != "" {
		builder.WriteString(" for path " + sanitizeText(path))
	}
	if dryRun {
		builder.WriteString(" (dry run; nothing was written)")
	}
	builder.WriteString("\n")

	for _, change := range changes {
		if change.Reason != "" || change.HardlinkOf != "" {
			continue
		}
		if change.Op == "rename" && change.OldPath != "" {
			fmt.Fprintf(&builder, "  R %s -> %s\n",
				sanitizeText(change.OldPath), sanitizeText(change.Path))
			continue
		}
		if change.TypeChange != "" {
			fmt.Fprintf(&builder, "  %s %s (type: %s)\n",
				opMarker(change.Op), sanitizeText(change.Path), sanitizeText(change.TypeChange))
			continue
		}
		fmt.Fprintf(&builder, "  %s %s (+%d/-%d)\n", opMarker(change.Op), sanitizeText(change.Path), change.Added, change.Removed)
	}
	if len(skipped) > 0 {
		fmt.Fprintf(&builder, "warning: %d change(s) could not be reverted and were left as-is:\n", len(skipped))
		for _, change := range skipped {
			fmt.Fprintf(&builder, "  ! %s: %s\n", sanitizeText(change.Path), change.Reason)
		}
	}
	if len(affected) > 0 {
		fmt.Fprintf(&builder, "warning: %d path(s) share an inode with a rewritten path and were left as-is:\n", len(affected))
		for _, change := range affected {
			fmt.Fprintf(&builder, "  ! %s: shares an inode with %s; the hard link was broken by the revert\n",
				sanitizeText(change.Path), sanitizeText(change.HardlinkOf))
		}
	}
	return builder.String()
}

// sanitizeText 把可能伪造结果行的控制字符替换成空格。
//
// 路径来自文件系统，而文件名是 agent 自己可以创造的（POSIX 允许换行）：不处理的话，
// 一个叫 "evil\n  A main.go (+99)" 的文件就能在模型可见的结果里伪造出一条
// "新增了 main.go" 的假变更。这只影响文案，但文案是模型判断"撤销了什么"的依据。
func sanitizeText(s string) string {
	if !strings.ContainsFunc(s, isControl) {
		return s
	}
	return strings.Map(func(r rune) rune {
		if isControl(r) {
			return ' '
		}
		return r
	}, s)
}

// isControl 报告一个 rune 是否是应当被抹平的控制字符。
func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

// opMarker 返回变更类型的单字母标记（与 patch 工具的结果风格一致）。
func opMarker(op string) string {
	switch op {
	case "add":
		return "A"
	case "delete":
		return "D"
	case "modify":
		return "M"
	case "rename":
		return "R"
	}
	return "?"
}
