package builtin

// 本文件生成 write_file / edit_file 结果里的**一行紧凑摘要**：
//
//	wrote 42 bytes to src/a.txt
//	write_file: src/a.txt (2 lines → 3 lines, +2 -1)
//
// 动机：模型看不到"改了多少"时无法自查（"我以为只改了一行"），而给它一个 dry_run
// 参数要多一整个模型轮次。把统计放进**结果**里是零参数、零往返的做法。
//
// 逐行比较统一交给 internal/diffpreview（与审批弹窗、apply_patch 的 dry_run 预览
// 用同一份实现）；二进制或超大文件不比较，只报字节数。

import (
	"fmt"

	"github.com/zeokeel/zeokeel-rig/internal/diffpreview"
)

// writeFileSummary 生成 write_file 的摘要行。
//
// oldKnown 为 false 表示旧内容没有被读进内存（文件太大），此时只能报字节数；
// oldSize 是旧文件的字节数。existed 为 false 表示这次是新建。
func writeFileSummary(relative string, old []byte, oldSize int64, existed, oldKnown bool, content []byte) string {
	if !existed {
		return fmt.Sprintf("%s: %s (created, %s)", ToolWriteFile, relative, lineCountWords(content))
	}
	if !oldKnown || !comparable(old, content) {
		return fmt.Sprintf("%s: %s (%d bytes → %d bytes)", ToolWriteFile, relative, oldSize, len(content))
	}
	added, removed := lineStats(old, content)
	return fmt.Sprintf("%s: %s (%d lines → %d lines, +%d -%d)",
		ToolWriteFile, relative, diffpreview.LineCount(old), diffpreview.LineCount(content), added, removed)
}

// editFileSummary 生成 edit_file 的摘要行。
func editFileSummary(relative string, old, updated []byte, occurrences int) string {
	replaced := replaceWords(occurrences)
	if !comparable(old, updated) {
		return fmt.Sprintf("%s: %s (%s, %d bytes → %d bytes)",
			ToolEditFile, relative, replaced, len(old), len(updated))
	}
	added, removed := lineStats(old, updated)
	return fmt.Sprintf("%s: %s (%s, +%d -%d)", ToolEditFile, relative, replaced, added, removed)
}

// comparable 报告两侧内容能否逐行比较（二进制或超过 1 MiB 就不比）。
func comparable(old, new []byte) bool {
	if diffpreview.IsBinary(old) || diffpreview.IsBinary(new) {
		return false
	}
	return len(old) <= diffpreview.MaxBytes && len(new) <= diffpreview.MaxBytes
}

// lineStats 返回增删行数。
func lineStats(old, new []byte) (added, removed int) {
	return diffpreview.Stats(diffpreview.Change{Old: old, New: new})
}

// lineCountWords 把内容描述成 "0 lines" / "1 line" / "N lines"。
func lineCountWords(content []byte) string {
	lines := diffpreview.LineCount(content)
	if lines == 1 {
		return "1 line"
	}
	return fmt.Sprintf("%d lines", lines)
}

// replaceWords 返回 "replace 1 occurrence" / "replace 3 occurrences"。
func replaceWords(occurrences int) string {
	if occurrences == 1 {
		return "replace 1 occurrence"
	}
	return fmt.Sprintf("replace %d occurrences", occurrences)
}
