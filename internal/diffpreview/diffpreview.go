// Package diffpreview 生成"给人看"的统一 diff 预览：纯函数、无 IO、输出稳定。
//
// 它服务于产品里最重要的一条交互：**先给我看 diff，再决定要不要落盘**。审批界面
// 需要把"这次工具调用会改什么"渲染成一段可读文本，而这段文本必须：
//
//   - 可断言：同一输入永远产生同一字节序列（golden 快照钉死）；
//   - 有界：二进制与超大文件不尝试逐行比较，直接给摘要，避免审批时先卡十秒；
//   - git 兼容：`--- a/path` / `+++ b/path` / `@@ -l,n +l,n @@`、3 行上下文、
//     无尾换行的 `\ No newline at end of file` 标记——用户能直接把它存成 .patch
//     交给 `git apply`。
//
// 本包只依赖标准库；不读文件、不碰 globals，因此可以在任何 goroutine 里并发调用。
package diffpreview

import (
	"bytes"
	"fmt"
	"strings"
)

const (
	// ContextLines 是每个 hunk 两侧保留的上下文行数（与 git 默认一致）。
	ContextLines = 3
	// MaxBytes 是参与逐行比较的单侧内容上限（1 MiB）。超过则只给摘要。
	//
	// 为什么是 1 MiB：审批是交互式的，把 100 MB 的文件 diff 出来既没人看也拖慢
	// 弹窗；而"改了什么"对超大文件用字节数摘要已经足够做决定。
	MaxBytes = 1 << 20
	// MaxEditDistance 是 Myers 算法允许的最大编辑距离；超过后退化为"整段替换"。
	//
	// 它存在的意义是内存/时间上界：回溯用的 trace 是 O(D·(N+M))，而 1 MiB 文件
	// 有 ~26k 行；D 取到 4096 时光是 trace 就要 270 MB（实测）。512 的取值保证
	// 病态输入（整文件重写）在 ~10 ms、~4 MB 内退化成"整段替换"，而正常编辑
	// （几十处改动）的 D 远小于它，仍然是最小 diff。
	MaxEditDistance = 512
)

// 变更类型（与工具层的 add/modify/delete 同名）。
const (
	// OpAdd 表示新增文件。
	OpAdd = "add"
	// OpModify 表示修改文件内容。
	OpModify = "modify"
	// OpDelete 表示删除文件。
	OpDelete = "delete"
)

// Change 是一次"将要发生"的文件变更。
//
// Old/New 是变更前后的完整内容；Add 时 Old 为空，Delete 时 New 为空。
// Added/Removed 可以预先给出行数统计（工具已经算过时直接用），为 0 且内容不同时
// 由本包按 diff 计算。
type Change struct {
	// Path 是展示用路径（斜杠分隔，通常相对工作区根）。
	Path string
	// Op 取 OpAdd / OpModify / OpDelete。
	Op string
	// Old 是变更前内容（新增时为 nil）。
	Old []byte
	// New 是变更后内容（删除时为 nil）。
	New []byte
	// Added/Removed 是可选的预计算行数统计。
	Added, Removed int
}

// Preview 返回一个文件的统一 diff 文本。
//
// 规则：
//   - 内容相同 → 空字符串（没有变更，不伪造空 hunk）；
//   - 任一侧是二进制（含 NUL）或超过 MaxBytes → `binary file changed (N bytes → M bytes)`；
//   - 其余情况返回 git 兼容的 unified diff（含 `---`/`+++` 头与 3 行上下文）。
//
// path 只影响头部的 `a/` `b/` 前缀，不参与任何文件系统操作。
func Preview(path string, old, new []byte) (string, error) {
	text, err := previewText(path, old, new)
	if err != nil {
		return "", err
	}
	return text, nil
}

// previewText 是 Preview 的实现。
func previewText(path string, old, new []byte) (string, error) {
	if bytes.Equal(old, new) {
		return "", nil
	}
	if IsBinary(old) || IsBinary(new) || len(old) > MaxBytes || len(new) > MaxBytes {
		return fmt.Sprintf("binary file changed (%d bytes → %d bytes)", len(old), len(new)), nil
	}
	script := diffScript(splitLines(old), splitLines(new))
	if len(script) == 0 {
		return "", nil
	}
	hunks := buildHunks(script, ContextLines)
	var builder strings.Builder
	writeFileHeader(&builder, path, old, new)
	oldAt, newAt := prefixCounts(script)
	for _, hunk := range hunks {
		oldCount, newCount := hunk.oldCount, hunk.newCount
		fmt.Fprintf(&builder, "@@ -%s +%s @@\n",
			formatRange(oldAt[hunk.start], oldCount), formatRange(newAt[hunk.start], newCount))
		for index := hunk.start; index < hunk.end; index++ {
			item := script[index]
			builder.WriteByte(byte(item.kind))
			builder.WriteString(item.line.text)
			builder.WriteByte('\n')
			if !item.line.newline {
				// git 的标记行：说明这一行不是以换行结尾的。
				builder.WriteString("\\ No newline at end of file\n")
			}
		}
	}
	return builder.String(), nil
}

// PreviewChange 是 Change 的便捷入口：按 Op 决定哪一侧内容为空。
func PreviewChange(change Change) (string, error) {
	old, new := change.Old, change.New
	switch change.Op {
	case OpAdd:
		old = nil
	case OpDelete:
		new = nil
	}
	return Preview(change.Path, old, new)
}

// Summary 返回一行汇总，例如 `3 files changed, +12 -4`。
//
// 没有变更时返回 `no changes`。二进制/超大文件不贡献行数（与 git 的
// `Binary files differ` 一致）。
func Summary(changes []Change) string {
	if len(changes) == 0 {
		return "no changes"
	}
	added, removed := 0, 0
	for _, change := range changes {
		left, right := Stats(change)
		added += left
		removed += right
	}
	noun := "files"
	if len(changes) == 1 {
		noun = "file"
	}
	return fmt.Sprintf("%d %s changed, +%d -%d", len(changes), noun, added, removed)
}

// Stats 返回一处变更的增删行数：优先使用 Change 里预先给的计数。
func Stats(change Change) (added, removed int) {
	if change.Added != 0 || change.Removed != 0 {
		return change.Added, change.Removed
	}
	old, new := change.Old, change.New
	switch change.Op {
	case OpAdd:
		old = nil
	case OpDelete:
		new = nil
	}
	if IsBinary(old) || IsBinary(new) || len(old) > MaxBytes || len(new) > MaxBytes {
		return 0, 0
	}
	for _, item := range diffScript(splitLines(old), splitLines(new)) {
		switch item.kind {
		case opInsert:
			added++
		case opDelete:
			removed++
		}
	}
	return added, removed
}

// IsBinary 报告内容是否应当按二进制处理（含 NUL 字节）。
func IsBinary(content []byte) bool { return bytes.IndexByte(content, 0) >= 0 }

// LineCount 统计内容的行数：以换行分段，末尾没有换行也算一行。
func LineCount(content []byte) int {
	count := 0
	for _, item := range splitLines(content) {
		_ = item
		count++
	}
	return count
}

// --- 内部实现 -------------------------------------------------------------

// opKind 是脚本里的一步：保留、删除或插入。
type opKind byte

const (
	opEqual  opKind = ' '
	opDelete opKind = '-'
	opInsert opKind = '+'
)

// diffLine 是一行内容；newline 记录它是否以换行结尾（决定要不要打标记行）。
type diffLine struct {
	text    string
	newline bool
}

// lineStep 是 diff 脚本里的一步。
type lineStep struct {
	kind opKind
	line diffLine
}

// hunk 是一个输出块：脚本下标区间 [start, end) 与两边的行数。
type hunk struct {
	start, end         int
	oldCount, newCount int
}

// splitLines 把内容切成行；"\n" 结尾不产生空行，没有尾换行的最后一行也会保留。
func splitLines(content []byte) []diffLine {
	if len(content) == 0 {
		return nil
	}
	lines := make([]diffLine, 0, bytes.Count(content, []byte("\n"))+1)
	start := 0
	for index := 0; index < len(content); index++ {
		if content[index] != '\n' {
			continue
		}
		lines = append(lines, diffLine{text: string(content[start:index]), newline: true})
		start = index + 1
	}
	if start < len(content) {
		lines = append(lines, diffLine{text: string(content[start:]), newline: false})
	}
	return lines
}

// diffScript 返回把 old 变成 new 的编辑脚本。
//
// 先剥掉公共前后缀（绝大多数编辑只需要比较"中间那一小段"），再对中间部分跑
// Myers；编辑距离超上限时退化为"先全删再全插"——仍然是合法 diff，只是不最小。
func diffScript(old, new []diffLine) []lineStep {
	prefix := 0
	for prefix < len(old) && prefix < len(new) && old[prefix] == new[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(old)-prefix && suffix < len(new)-prefix &&
		old[len(old)-1-suffix] == new[len(new)-1-suffix] {
		suffix++
	}

	script := make([]lineStep, 0, len(old)+len(new))
	for index := 0; index < prefix; index++ {
		script = append(script, lineStep{kind: opEqual, line: old[index]})
	}
	midOld := old[prefix : len(old)-suffix]
	midNew := new[prefix : len(new)-suffix]
	script = append(script, diffMiddle(midOld, midNew)...)
	for index := len(old) - suffix; index < len(old); index++ {
		script = append(script, lineStep{kind: opEqual, line: old[index]})
	}
	return script
}

// diffMiddle 比较去掉公共前后缀之后的中段。
func diffMiddle(old, new []diffLine) []lineStep {
	switch {
	case len(old) == 0 && len(new) == 0:
		return nil
	case len(old) == 0:
		return insertAll(new)
	case len(new) == 0:
		return deleteAll(old)
	}
	if script, ok := myersScript(old, new); ok {
		return script
	}
	degraded := deleteAll(old)
	return append(degraded, insertAll(new)...)
}

// insertAll / deleteAll 是退化路径与空侧路径。
func insertAll(lines []diffLine) []lineStep {
	script := make([]lineStep, 0, len(lines))
	for _, line := range lines {
		script = append(script, lineStep{kind: opInsert, line: line})
	}
	return script
}

func deleteAll(lines []diffLine) []lineStep {
	script := make([]lineStep, 0, len(lines))
	for _, line := range lines {
		script = append(script, lineStep{kind: opDelete, line: line})
	}
	return script
}

// myersScript 是 Myers 差分算法（带 trace 回溯，返回完整编辑脚本）。
//
// 返回 false 表示编辑距离超过 MaxEditDistance（内存/时间上界），调用方退化。
func myersScript(old, new []diffLine) ([]lineStep, bool) {
	n, m := len(old), len(new)
	maxD := n + m
	if maxD > MaxEditDistance {
		maxD = MaxEditDistance
	}
	offset := maxD + 1
	size := 2*maxD + 3
	v := make([]int, size)
	trace := make([][]int, 0, maxD+1)

	for depth := 0; depth <= maxD; depth++ {
		for k := -depth; k <= depth; k += 2 {
			index := k + offset
			var x int
			if k == -depth || (k != depth && v[index-1] < v[index+1]) {
				x = v[index+1]
			} else {
				x = v[index-1] + 1
			}
			y := x - k
			for x < n && y < m && x >= 0 && y >= 0 && old[x] == new[y] {
				x++
				y++
			}
			v[index] = x
			if x >= n && y >= m {
				return backtrack(trace, old, new, depth, offset), true
			}
		}
		// 记录本轮结束后的 V，供回溯使用。
		snapshot := make([]int, size)
		copy(snapshot, v)
		trace = append(trace, snapshot)
	}
	return nil, false
}

// backtrack 从 trace 里恢复编辑脚本。
func backtrack(trace [][]int, old, new []diffLine, depth, offset int) []lineStep {
	script := make([]lineStep, 0, len(old)+len(new))
	x, y := len(old), len(new)
	for d := depth; d > 0; d-- {
		if d-1 >= len(trace) {
			break
		}
		previous := trace[d-1]
		k := x - y
		var previousK int
		if k == -d || (k != d && previous[k-1+offset] < previous[k+1+offset]) {
			previousK = k + 1
		} else {
			previousK = k - 1
		}
		previousX := previous[previousK+offset]
		previousY := previousX - previousK
		for x > previousX && y > previousY {
			script = append(script, lineStep{kind: opEqual, line: old[x-1]})
			x--
			y--
		}
		switch {
		case x == previousX:
			if y > 0 {
				script = append(script, lineStep{kind: opInsert, line: new[y-1]})
				y--
			}
		default:
			if x > 0 {
				script = append(script, lineStep{kind: opDelete, line: old[x-1]})
				x--
			}
		}
	}
	for x > 0 && y > 0 {
		script = append(script, lineStep{kind: opEqual, line: old[x-1]})
		x--
		y--
	}
	for x > 0 {
		script = append(script, lineStep{kind: opDelete, line: old[x-1]})
		x--
	}
	for y > 0 {
		script = append(script, lineStep{kind: opInsert, line: new[y-1]})
		y--
	}
	// 回溯是逆序的，翻转回来。
	for left, right := 0, len(script)-1; left < right; left, right = left+1, right-1 {
		script[left], script[right] = script[right], script[left]
	}
	return script
}

// buildHunks 把脚本切成 hunk：改动之间若只隔着 ≤2*context 行就合并。
func buildHunks(script []lineStep, context int) []hunk {
	if context < 0 {
		context = 0
	}
	changed := make([]int, 0, len(script))
	for index, item := range script {
		if item.kind != opEqual {
			changed = append(changed, index)
		}
	}
	if len(changed) == 0 {
		return nil
	}
	oldAt, newAt := prefixCounts(script)
	hunks := make([]hunk, 0, len(changed))
	first, last := changed[0], changed[0]
	flush := func() {
		start := first - context
		if start < 0 {
			start = 0
		}
		end := last + 1 + context
		if end > len(script) {
			end = len(script)
		}
		hunks = append(hunks, hunk{
			start: start, end: end,
			oldCount: oldAt[end] - oldAt[start],
			newCount: newAt[end] - newAt[start],
		})
	}
	for _, index := range changed[1:] {
		if index-last-1 <= 2*context {
			last = index
			continue
		}
		flush()
		first, last = index, index
	}
	flush()
	return hunks
}

// prefixCounts 返回脚本每个位置之前"已经消费的旧行数/新行数"。
func prefixCounts(script []lineStep) (oldAt, newAt []int) {
	oldAt = make([]int, len(script)+1)
	newAt = make([]int, len(script)+1)
	for index, item := range script {
		oldAt[index+1] = oldAt[index]
		newAt[index+1] = newAt[index]
		switch item.kind {
		case opEqual:
			oldAt[index+1]++
			newAt[index+1]++
		case opDelete:
			oldAt[index+1]++
		case opInsert:
			newAt[index+1]++
		}
	}
	return oldAt, newAt
}

// writeFileHeader 写 `---` / `+++` 两行；空侧用 /dev/null（新增/删除）。
func writeFileHeader(builder *strings.Builder, path string, old, new []byte) {
	display := strings.TrimSpace(path)
	if display == "" {
		display = "file"
	}
	display = strings.ReplaceAll(display, "\\", "/")
	display = strings.TrimPrefix(display, "./")
	if len(old) == 0 {
		builder.WriteString("--- /dev/null\n")
	} else {
		builder.WriteString("--- a/" + display + "\n")
	}
	if len(new) == 0 {
		builder.WriteString("+++ /dev/null\n")
	} else {
		builder.WriteString("+++ b/" + display + "\n")
	}
}

// formatRange 格式化 hunk 的一侧范围：行数为 1 时省略 ",1"（与 git 一致），
// 为 0 时起点是"前一行的行号"。
func formatRange(start, count int) string {
	switch count {
	case 0:
		return fmt.Sprintf("%d,0", start)
	case 1:
		return fmt.Sprintf("%d", start+1)
	default:
		return fmt.Sprintf("%d,%d", start+1, count)
	}
}
