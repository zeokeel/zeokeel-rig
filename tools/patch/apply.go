package patch

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// 本文件是"补丁 → 新内容"的核心：行模型（保留每行原始行尾）、三级空白容错匹配、
// 逐 hunk 应用，以及**两阶段落盘**（先在内存里算出全部结果，再统一写）。
//
// 之所以逐行保留行尾而不是整文件统一换行：混用 CRLF/LF 的文件里，没被补丁碰到的
// 行必须逐字节保持不变；统一换行会把它改掉。

// textLine 是一行文本：text 不含行尾，eol 是行尾（"\n"、"\r\n" 或 "" 表示文件
// 末尾没有换行）。
type textLine struct {
	text string
	eol  string
}

// splitTextLines 把文件内容切成行，保留每行自己的行尾。
func splitTextLines(data []byte) []textLine {
	if len(data) == 0 {
		return nil
	}
	lines := make([]textLine, 0, bytes.Count(data, []byte{'\n'})+1)
	start := 0
	for index := 0; index < len(data); index++ {
		if data[index] != '\n' {
			continue
		}
		text := data[start:index]
		eol := "\n"
		if len(text) > 0 && text[len(text)-1] == '\r' {
			text = text[:len(text)-1]
			eol = "\r\n"
		}
		lines = append(lines, textLine{text: string(text), eol: eol})
		start = index + 1
	}
	if start < len(data) {
		lines = append(lines, textLine{text: string(data[start:])})
	}
	return lines
}

// joinTextLines 把行模型拼回字节。
func joinTextLines(lines []textLine) []byte {
	var buffer bytes.Buffer
	for _, line := range lines {
		buffer.WriteString(line.text)
		buffer.WriteString(line.eol)
	}
	return buffer.Bytes()
}

// preferredEOL 返回文件的主导行尾：出现过 CRLF 且不少于纯 LF 时用 "\r\n"。
func preferredEOL(lines []textLine) string {
	crlf, lf := 0, 0
	for _, line := range lines {
		switch line.eol {
		case "\r\n":
			crlf++
		case "\n":
			lf++
		}
	}
	if crlf > 0 && crlf >= lf {
		return "\r\n"
	}
	return "\n"
}

// hasBinaryContent 报告内容里是否有 NUL 字节（我们用它判断二进制）。
func hasBinaryContent(data []byte) bool {
	return bytes.IndexByte(data, 0) >= 0
}

// applyHunks 把一组 hunk 应用到文件行模型上，返回新行、+N/-M 与错误。
//
// 每个 hunk 都从上一次匹配结束的位置开始搜索（找不到时退回文首），因此同一份
// 补丁里的多个 hunk 按补丁顺序单调前进；单个 hunk 的上下文在搜索窗口内出现多次
// 即判为歧义并报错，绝不在多个候选里猜一个。
func applyHunks(lines []textLine, hunks []hunk, rel string) (out []textLine, added, removed int, err error) {
	current := append([]textLine(nil), lines...)
	// UTF-8 BOM 是编码标记而不是内容：匹配时先摘掉（补丁里的上下文行不会带 BOM，
	// 模型也看不到它），写回时再原样加回首行。这样既不会让首行上下文匹配失败，
	// 也不会在改写首行时把 BOM 弄丢。
	bom := ""
	if len(current) > 0 && strings.HasPrefix(current[0].text, "\ufeff") {
		bom = "\ufeff"
		current[0].text = strings.TrimPrefix(current[0].text, "\ufeff")
	}
	noTrailingNewline := len(current) > 0 && current[len(current)-1].eol == ""
	eol := preferredEOL(lines)
	cursor := 0
	endsAtEOF := false
	// insertedEOL 是新增行优先使用的行尾：继承本 hunk 最近消费掉的那一行的行尾，
	// 这样混用 CRLF/LF 的文件里插入行与邻居保持一致，而不是被整体风格改写。
	insertedEOL := ""

	for _, h := range hunks {
		from := cursor
		if h.anchor != "" {
			anchorIndex, anchorErr := findAnchor(current, h.anchor, rel, h)
			if anchorErr != nil {
				return nil, 0, 0, anchorErr
			}
			from = anchorIndex
		}
		expected := h.expected()
		hunkAdded, hunkRemoved := h.counts()
		added += hunkAdded
		removed += hunkRemoved

		if len(expected) == 0 {
			// 纯插入：位置由统一 diff 的行号决定，没有行号时插在游标处。
			position := from
			if h.hasCounts {
				position = h.oldStart
			}
			if position < 0 {
				position = 0
			}
			if position > len(current) {
				position = len(current)
			}
			inserted := make([]textLine, 0, len(h.lines))
			for _, line := range h.lines {
				if line.kind == '+' {
					inserted = append(inserted, textLine{text: line.text, eol: eol})
				}
			}
			current = spliceLines(current, position, 0, inserted)
			cursor = position + len(inserted)
			endsAtEOF = position+len(inserted) == len(current)
			continue
		}

		start, matchErr := findContext(current, expected, from, rel, h)
		if matchErr != nil {
			return nil, 0, 0, matchErr
		}
		replacement := make([]textLine, 0, len(h.lines))
		source := start
		insertedEOL = ""
		for _, line := range h.lines {
			switch line.kind {
			case ' ':
				// 上下文行按**文件里的原始字节**保留，包括空白差异。
				if source < len(current) {
					if current[source].eol != "" {
						insertedEOL = current[source].eol
					}
					replacement = append(replacement, textLine{text: current[source].text, eol: current[source].eol})
					source++
				}
			case '-':
				if source < len(current) && current[source].eol != "" {
					insertedEOL = current[source].eol
				}
				source++
			case '+':
				lineEOL := insertedEOL
				if lineEOL == "" {
					lineEOL = eol
				}
				replacement = append(replacement, textLine{text: line.text, eol: lineEOL})
			}
		}
		touchesEOF := start+len(expected) == len(current)
		current = spliceLines(current, start, len(expected), replacement)
		cursor = start + len(replacement)
		endsAtEOF = touchesEOF
	}

	if noTrailingNewline {
		// 原始文件末尾没有换行：把这一属性转移到新的最后一行，但只在补丁确实替换
		// 掉了原来的最后一行时。若原最后一行只是变成了中间行（后面追加了内容），
		// 它必须补上行尾，文件也就以换行结束。
		missing := -1
		for index, line := range current {
			if line.eol == "" {
				missing = index
				break
			}
		}
		switch {
		case missing >= 0 && missing < len(current)-1:
			current[missing].eol = eol
		case missing < 0 && endsAtEOF && len(current) > 0:
			current[len(current)-1].eol = ""
		}
	}
	if bom != "" && len(current) > 0 {
		current[0].text = bom + current[0].text
	}
	return current, added, removed, nil
}

// spliceLines 返回把 [start, start+remove) 替换为 insert 之后的新切片。
func spliceLines(lines []textLine, start, remove int, insert []textLine) []textLine {
	out := make([]textLine, 0, len(lines)-remove+len(insert))
	out = append(out, lines[:start]...)
	out = append(out, insert...)
	out = append(out, lines[start+remove:]...)
	return out
}

// 上下文匹配的三个级别：精确、忽略行尾空白、忽略所有空白。
const (
	matchExact = iota
	matchTrailingSpace
	matchAnySpace
)

// matchLevelName 返回级别的可读名字，用于错误文案。
func matchLevelName(level int) string {
	switch level {
	case matchExact:
		return "exact"
	case matchTrailingSpace:
		return "ignoring trailing whitespace"
	default:
		return "ignoring all whitespace"
	}
}

// lineMatches 按级别比较两行文本。
func lineMatches(level int, expected, actual string) bool {
	switch level {
	case matchExact:
		return expected == actual
	case matchTrailingSpace:
		return strings.TrimRight(expected, " \t") == strings.TrimRight(actual, " \t")
	default:
		return stripAllSpace(expected) == stripAllSpace(actual)
	}
}

// stripAllSpace 删除所有空白字符（空格、制表符、回车）。
func stripAllSpace(text string) string {
	var builder strings.Builder
	builder.Grow(len(text))
	for _, r := range text {
		switch r {
		case ' ', '\t', '\r', '\v', '\f':
			continue
		}
		builder.WriteRune(r)
	}
	return builder.String()
}

// findAnchor 查找 Codex 风格的 "@@ 锚点"行，返回它的行下标。锚点只是搜索起点的
// 提示：匹配多行时取最靠前的一行，不做歧义报错（歧义由真正的上下文匹配负责）。
func findAnchor(lines []textLine, anchor, rel string, h hunk) (int, error) {
	for level := matchExact; level <= matchAnySpace; level++ {
		for index, line := range lines {
			if lineMatches(level, anchor, line.text) {
				return index, nil
			}
		}
	}
	return 0, fmt.Errorf("%s:%d: anchor %q not found in the file (searched with exact, trailing-whitespace and all-whitespace matching)", rel, hunkLineNumber(h, 0), anchor)
}

// findContext 在文件里定位 hunk 的上下文，返回起始行下标。
//
// 搜索窗口优先从 from 开始（游标语义）；在该窗口内没有候选时退回整个文件。
// 每个级别只认唯一匹配：同一级别出现多个候选即报歧义。
func findContext(lines []textLine, expected []string, from int, rel string, h hunk) (int, error) {
	if len(expected) == 0 {
		return from, nil
	}
	if from < 0 {
		from = 0
	}
	limit := len(lines) - len(expected)
	if limit < 0 {
		return 0, notFoundError(lines, expected, from, rel, h)
	}
	for level := matchExact; level <= matchAnySpace; level++ {
		var forward, backward []int
		for start := 0; start <= limit; start++ {
			if !hunkMatchesAt(lines, expected, start, level) {
				continue
			}
			if start >= from {
				forward = append(forward, start)
			} else {
				backward = append(backward, start)
			}
		}
		candidates := forward
		if len(candidates) == 0 {
			candidates = backward
		}
		if len(candidates) == 0 {
			continue
		}
		if len(candidates) > 1 {
			return 0, ambiguousError(rel, h, candidates, level)
		}
		return candidates[0], nil
	}
	return 0, notFoundError(lines, expected, from, rel, h)
}

// hunkMatchesAt 报告 expected 能否在 lines[start:] 处按给定级别整体匹配。
func hunkMatchesAt(lines []textLine, expected []string, start, level int) bool {
	for offset, want := range expected {
		if !lineMatches(level, want, lines[start+offset].text) {
			return false
		}
	}
	return true
}

// hunkLineNumber 返回错误文案里用的文件行号：统一 diff 用声明的起始行，Codex
// 风格用搜索起点（1 基）。
func hunkLineNumber(h hunk, from int) int {
	if h.hasCounts && h.oldStart > 0 {
		return h.oldStart
	}
	if h.hasCounts {
		return 1
	}
	return from + 1
}

// ambiguousError 构造歧义错误：列出候选位置并要求补充上下文。
func ambiguousError(rel string, h hunk, candidates []int, level int) error {
	shown := candidates
	if len(shown) > 4 {
		shown = shown[:4]
	}
	locations := make([]string, 0, len(shown))
	for _, candidate := range shown {
		locations = append(locations, fmt.Sprintf("%s:%d", rel, candidate+1))
	}
	more := ""
	if len(candidates) > len(shown) {
		more = fmt.Sprintf(", and %d more", len(candidates)-len(shown))
	}
	return fmt.Errorf("%s:%d: ambiguous context: %d locations match when matching %s (%s%s); add more context lines to disambiguate",
		rel, hunkLineNumber(h, candidates[0]), len(candidates), matchLevelName(level), strings.Join(locations, ", "), more)
}

// notFoundError 构造"上下文找不到"的错误：同时给出期望上下文与文件实际内容附近
// 的行号，让模型能自己判断是补丁过时还是文件被改过。
func notFoundError(lines []textLine, expected []string, from int, rel string, h hunk) error {
	lineNo := hunkLineNumber(h, from)
	var builder strings.Builder
	fmt.Fprintf(&builder, "%s:%d: hunk context not found (no exact, trailing-whitespace or all-whitespace match)", rel, lineNo)
	builder.WriteString("\nexpected context:")
	show := expected
	if len(show) > 8 {
		show = show[:8]
	}
	for index, line := range show {
		fmt.Fprintf(&builder, "\n  %3d | %s", lineNo+index, clipLine(line))
	}
	if len(expected) > len(show) {
		fmt.Fprintf(&builder, "\n  ... (%d more expected line(s))", len(expected)-len(show))
	}
	anchor := lineNo - 1
	if anchor > len(lines) {
		anchor = len(lines)
	}
	start := anchor - 2
	if start < 0 {
		start = 0
	}
	end := anchor + 3
	if end > len(lines) {
		end = len(lines)
	}
	fmt.Fprintf(&builder, "\nactual content around line %d (file has %d line(s)):", lineNo, len(lines))
	for index := start; index < end; index++ {
		fmt.Fprintf(&builder, "\n  %3d | %s", index+1, clipLine(lines[index].text))
	}
	return errors.New(builder.String())
}

// clipLine 把错误信息里引用的行裁剪到可读长度。
func clipLine(text string) string {
	const max = 160
	if len(text) <= max {
		return text
	}
	return text[:max] + "…"
}

// ---------------------------------------------------------------------------
// 计划与落盘
// ---------------------------------------------------------------------------

// limits 是单次调用的规模上限（见 WithMaxFileBytes / WithMaxFiles）。
type limits struct {
	// maxFileBytes 是单个目标文件的读取上限；<=0 表示不限制。
	maxFileBytes int64
	// maxFiles 是单补丁文件数上限；<=0 表示不限制。
	maxFiles int
}

// plan 是一个文件的最终写入计划（尚未落盘）。
type plan struct {
	op opKind
	// rel 是相对根的斜杠路径，用于文案与去重。
	rel string
	// writeRel 是实际写入的根相对路径（更新时是跟随最终符号链接后的落点；
	// 删除时不用它）。所有 I/O 都经 fsguard 用它定位。
	writeRel string
	// moveRel 是移动目标（根相对路径）；空表示不移动。
	moveRel string
	// renameOnly 表示"纯移动"（移动且没有任何 hunk）：直接 rename 源路径本身，
	// 因此符号链接移动的是**链接**（与 mv 一致），内容与 inode 都不变。
	renameOnly bool
	// existed 表示源文件在补丁应用前是否存在。
	existed bool
	// old 是源文件的原始内容，用于失败回滚。
	old []byte
	// mode 是写回时使用的权限位。
	mode os.FileMode
	// content 是算好的新内容（delete 为 nil）。
	content []byte
	// added/removed 是 +N/-M 统计。
	added, removed int
}

// buildPlans 把操作列表变成写入计划：这里完成全部校验与"新内容"计算，磁盘只会
// 在 commit 阶段被修改，因此任何错误都保证零改动。
func buildPlans(root *rootFS, limits limits, policy *sandbox.Policy, ops []fileOp) ([]plan, error) {
	// 文件数上限：补丁文本可以很小却写很多文件（每行一个单行文件），而每个文件
	// 都要一次暂存 + 一次 rename。这里在碰磁盘之前整体拒绝。
	if limits.maxFiles > 0 && len(ops) > limits.maxFiles {
		return nil, fmt.Errorf("patch touches %d files (limit %d); split it into smaller patches", len(ops), limits.maxFiles)
	}
	plans := make([]plan, 0, len(ops))
	// occupied 用**规范化键**（大小写不敏感平台转小写）记录已占用的路径，用于拒绝
	// 重复操作。键与展示用的 rel 分开，保证错误文案里仍是用户写的路径。
	occupied := make(map[string]int, len(ops))
	// claimed 按补丁顺序记录已占用的路径，保证冲突错误文案是确定的。
	claimed := make([]claimedPath, 0, len(ops))
	// writtenTargets 记录每个物理写入目标（已解析符号链接、已 case-fold）的占用者。
	writtenTargets := make(map[string]claimedTarget, len(ops))

	// claim 拒绝"同一路径出现两次"以及"同一路径既当文件又当目录"。
	//
	// 后者的必要性：补丁里先 Add File: x，再 Add File: x/y.md——落盘时会先在暂存
	// 阶段 MkdirAll 出目录 x，随后 rename(temp, x) 失败，留下一个空目录并给出难懂的
	// "file exists"。这类冲突必须在碰磁盘之前就判掉。
	claim := func(rel, display, key string, line int) error {
		if first, exists := occupied[key]; exists {
			return fmt.Errorf("%s: appears twice in the same patch (first at line %d); split the changes into separate patches", display, first)
		}
		for _, existing := range claimed {
			if strings.HasPrefix(key, existing.key+"/") || strings.HasPrefix(existing.key, key+"/") {
				return fmt.Errorf("%s: path conflict with %s (line %d): a patch cannot treat one path as both a file and a directory", display, existing.rel, occupied[existing.key])
			}
		}
		return nil
	}

	for _, op := range ops {
		if strings.TrimSpace(op.path) == "" {
			return nil, fmt.Errorf("line %d: file operation is missing a path", op.line)
		}
		rel, abs, err := root.resolve(op.path)
		if err != nil {
			return nil, err
		}
		if policyErr := checkPolicyPath(policy, rel, abs); policyErr != nil {
			return nil, policyErr
		}
		key := dedupeKey(rel)
		if err := claim(rel, rel, key, op.line); err != nil {
			return nil, err
		}
		occupied[key] = op.line
		claimed = append(claimed, claimedPath{rel: rel, key: key})

		item := plan{op: op.kind, rel: rel, writeRel: rel, mode: 0o644}

		switch op.kind {
		case opAdd:
			if _, statErr := root.guard.Lstat(guardPath(rel)); statErr == nil {
				return nil, fmt.Errorf("%s: already exists; use Update File to modify it", rel)
			} else if !isNotExist(statErr) {
				return nil, leakedPathError(root, rel, statErr)
			}
		case opUpdate, opDelete:
			// 先看元信息：目录、大小上限都能在整读之前判定，避免把一个 GB 级文件
			// 读进内存。
			info, statErr := root.guard.Stat(guardPath(rel))
			if statErr != nil {
				if isNotExist(statErr) {
					return nil, fmt.Errorf("%s: does not exist; only Add File can create a file", rel)
				}
				return nil, leakedPathError(root, rel, statErr)
			}
			if info.IsDir() {
				return nil, fmt.Errorf("%s: is a directory, not a file", rel)
			}
			// 非普通文件必须在这里拒掉：fsguard 的 ReadFileLimit 用 O_RDONLY 打开，
			// 打开 FIFO 会**永久阻塞**在没有写端的 open 上（工具没有超时），一个补丁
			// 就能把整个回合挂死。设备/套接字同理不属于"文本文件"。
			// 非普通文件必须在这里拒掉：fsguard 的 ReadFileLimit 用 O_RDONLY 打开，
			// 打开 FIFO 会**永久阻塞**在没有写端的 open 上（工具没有超时），一个补丁
			// 就能把整个回合挂死。设备/套接字同理不属于"文本文件"。
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("%s: not a regular file (FIFO/socket/device); apply_patch only edits regular text files", rel)
			}
			if limits.maxFileBytes > 0 && info.Size() > limits.maxFileBytes {
				return nil, fmt.Errorf("%s: file too large to patch (%d bytes > limit %d); rewrite it with write_file", rel, info.Size(), limits.maxFileBytes)
			}
			data, readErr := root.guard.ReadFileLimit(guardPath(rel), limits.maxFileBytes)
			if readErr != nil {
				if isNotExist(readErr) {
					return nil, fmt.Errorf("%s: does not exist; only Add File can create a file", rel)
				}
				if isTooLarge(readErr) {
					return nil, fmt.Errorf("%s: file too large to patch (%d bytes > limit %d); rewrite it with write_file", rel, info.Size(), limits.maxFileBytes)
				}
				return nil, leakedPathError(root, rel, readErr)
			}
			if hasBinaryContent(data) {
				return nil, fmt.Errorf("%s: binary file (contains a NUL byte); apply_patch only edits text files", rel)
			}
			item.existed = true
			item.old = data
			item.mode = info.Mode().Perm()
			if item.mode == 0 {
				item.mode = 0o644
			}
			// 更新要**写穿**符号链接：fsguard 的 WriteFileAtomic 会跟随最终链接，
			// 但去重键与沙箱策略校验都需要知道真实落点，所以先解析一次。
			// 删除与移动则作用于链接本身（与 rm/mv 一致），不做解析。
			if op.kind == opUpdate && op.moveTo == "" {
				target, followed := root.finalLinkTarget(rel)
				if followed {
					// 链接落点也要过策略：否则 `link -> .env` 能绕开拒绝 glob。
					if policyErr := checkPolicyPath(policy, rel, root.absFor(target)); policyErr != nil {
						return nil, policyErr
					}
					item.writeRel = target
				}
			}
		}

		switch op.kind {
		case opAdd:
			lines := make([]textLine, 0, len(op.hunks))
			for _, h := range op.hunks {
				for _, line := range h.lines {
					if line.kind != '+' {
						return nil, fmt.Errorf("%s:%d: an added file may only contain \"+\" lines (got %q)", rel, h.line, string(line.kind))
					}
					lines = append(lines, textLine{text: line.text, eol: "\n"})
				}
			}
			// Add File 的每个 hunk 在解析时按行拆开，这里已合并；行尾统一 LF。
			added, removed := 0, 0
			for _, h := range op.hunks {
				a, r := h.counts()
				added += a
				removed += r
			}
			item.added, item.removed = added, removed
			item.content = joinTextLines(lines)
			if len(item.content) == 0 {
				item.content = []byte{}
			}
			if hasBinaryContent(item.content) {
				return nil, fmt.Errorf("%s: refusing to add a binary file (the content contains a NUL byte); apply_patch only edits text files", rel)
			}
		case opUpdate:
			source := splitTextLines(item.old)
			applied, added, removed, applyErr := applyHunks(source, op.hunks, rel)
			if applyErr != nil {
				return nil, applyErr
			}
			item.added, item.removed = added, removed
			item.content = joinTextLines(applied)
		case opDelete:
			source := splitTextLines(item.old)
			item.removed = len(source)
			if len(op.hunks) > 0 {
				// 删除 diff 携带了被删内容：先按上下文规则校验，确认"删掉的正是这份内容"，
				// 否则拒绝删除（与 update 同样的容错与歧义判定）。
				applied, _, _, applyErr := applyHunks(source, op.hunks, rel)
				if applyErr != nil {
					return nil, applyErr
				}
				if len(applied) > 0 {
					return nil, fmt.Errorf("%s: delete hunk does not remove the whole file (%d line(s) would remain)", rel, len(applied))
				}
			}
		}

		if op.moveTo != "" {
			moveRel, _, moveErr := root.resolve(op.moveTo)
			if moveErr != nil {
				return nil, moveErr
			}
			moveKey := dedupeKey(moveRel)
			if moveKey == key {
				return nil, fmt.Errorf("%s: move destination is the same as the source", rel)
			}
			if err := claim(moveRel, moveRel, moveKey, op.line); err != nil {
				return nil, err
			}
			if policyErr := checkPolicyPath(policy, moveRel, root.absFor(moveRel)); policyErr != nil {
				return nil, policyErr
			}
			if _, statErr := root.guard.Lstat(guardPath(moveRel)); statErr == nil {
				return nil, fmt.Errorf("%s: move destination already exists", moveRel)
			} else if !isNotExist(statErr) {
				return nil, leakedPathError(root, moveRel, statErr)
			}
			occupied[moveKey] = op.line
			claimed = append(claimed, claimedPath{rel: moveRel, key: moveKey})
			item.moveRel = moveRel
			item.renameOnly = len(op.hunks) == 0
		}
		// 同一物理文件不能被两个操作分别写入：符号链接与它的目标会在上面的
		// followFinalLink 之后落到同一个绝对路径，若不拦住就会"后写者胜"，
		// 而结果文案谎称改了两个文件。
		targetKey := dedupeKey(item.writeTargetRel())
		if previous, exists := writtenTargets[targetKey]; exists && previous.line != op.line {
			return nil, fmt.Errorf("%s: writes the same file as %s (line %d); a patch cannot modify one file twice", rel, previous.rel, previous.line)
		}
		writtenTargets[targetKey] = claimedTarget{rel: rel, line: op.line}

		plans = append(plans, item)
	}
	return plans, nil
}

// writeTargetRel 返回该计划最终写入的根相对路径（移动时是移动目标）。
func (p plan) writeTargetRel() string {
	if p.moveRel != "" {
		return p.moveRel
	}
	return p.writeRel
}

// commit 把计划落盘。它是唯一的磁盘写入点：全部 I/O 都经 fsguard 的遍历安全句柄
// （os.Root），因此"校验"与"落盘"之间不存在把目录换成符号链接的 TOCTOU 窗口——
// 补丁层不再自己拼绝对路径。
//
// 顺序：先做所有写入/移动，再执行删除；任何一步失败都按逆序回滚已生效的改动，并
// 清掉本次新建的空目录。dryRun 为 true 时直接返回，不产生任何文件。
func commit(root *rootFS, plans []plan, dryRun bool) error {
	if dryRun || len(plans) == 0 {
		return nil
	}

	// createdDirs 记录本次新建的目录（根相对路径）：失败时必须一并清掉，否则
	// "零改动"就成了空话（补丁会在工作区里留下空目录树）。
	var createdDirs []string
	// undo 按执行顺序记录"怎么撤销"，失败时逆序执行。
	var undo []func() error
	rollback := func() {
		for index := len(undo) - 1; index >= 0; index-- {
			_ = undo[index]()
		}
		// 目录最后清：回滚出来的文件都落在原本就存在的目录里，不会挡住删除。
		removeCreatedDirs(root, createdDirs)
	}

	for _, item := range plans {
		if item.op == opDelete {
			continue
		}
		target := item.writeTargetRel()
		if item.renameOnly {
			// 纯移动：rename 源路径本身——符号链接移动的是链接（mv 语义），
			// 内容/inode/权限都不变，也不需要先读后写。
			dirs, mkErr := mkdirAllTracked(root, path.Dir(target))
			if mkErr != nil {
				rollback()
				return fmt.Errorf("%s: create parent directory: %w", item.rel, mkErr)
			}
			createdDirs = append(createdDirs, dirs...)
			if err := root.guard.Rename(guardPath(item.rel), guardPath(target)); err != nil {
				rollback()
				return fmt.Errorf("%s: move to %s: %w", item.rel, target, userFacingWriteError(root, item.rel, err))
			}
			from, to := item.rel, target
			undo = append(undo, func() error { return root.guard.Rename(guardPath(to), guardPath(from)) })
			continue
		}
		dirs, mkErr := mkdirAllTracked(root, path.Dir(target))
		if mkErr != nil {
			rollback()
			return fmt.Errorf("%s: create parent directory: %w", item.rel, mkErr)
		}
		createdDirs = append(createdDirs, dirs...)

		if err := root.guard.WriteFileAtomic(guardPath(target), item.content, item.mode); err != nil {
			rollback()
			return fmt.Errorf("%s: write: %w", item.rel, userFacingWriteError(root, item.rel, err))
		}
		if item.existed && item.moveRel == "" {
			rel, old, mode := item.rel, item.old, item.mode
			undo = append(undo, func() error { return root.guard.WriteFileAtomic(guardPath(rel), old, mode) })
		} else {
			writeRel := target
			undo = append(undo, func() error { return removeIfExists(root, writeRel) })
		}
		if item.moveRel == "" {
			continue
		}
		// 移动：目标写好之后删源。删的是**源路径本身**，源是符号链接时删链不删目标
		// （与 mv 一致）。
		if err := removeIfExists(root, item.rel); err != nil {
			rollback()
			return fmt.Errorf("%s: remove the source of a move: %w", item.rel, userFacingWriteError(root, item.rel, err))
		}
		rel, old, mode := item.rel, item.old, item.mode
		undo = append(undo, func() error { return root.guard.WriteFileAtomic(guardPath(rel), old, mode) })
	}
	for _, item := range plans {
		if item.op != opDelete {
			continue
		}
		if err := removeIfExists(root, item.rel); err != nil {
			rollback()
			return fmt.Errorf("%s: delete: %w", item.rel, userFacingWriteError(root, item.rel, err))
		}
		rel, old, mode := item.rel, item.old, item.mode
		undo = append(undo, func() error { return root.guard.WriteFileAtomic(guardPath(rel), old, mode) })
	}
	return nil
}

// mkdirAllTracked 创建目标目录并返回**本次真正新建**的目录（根相对路径，由深到浅），
// 供失败回滚清掉。存在性判断同样走 fsguard，不用 filepath 去猜。
func mkdirAllTracked(root *rootFS, dir string) ([]string, error) {
	dir = path.Clean(filepath.ToSlash(dir))
	if dir == "." || dir == "/" || dir == "" {
		return nil, nil
	}
	var created []string
	for current := dir; current != "." && current != "/" && current != ""; current = path.Dir(current) {
		if _, err := root.guard.Lstat(guardPath(current)); err == nil {
			break
		} else if !isNotExist(err) {
			return nil, userFacingWriteError(root, dir, err)
		}
		created = append(created, current)
	}
	if err := root.guard.MkdirAll(guardPath(dir), 0o755); err != nil {
		return nil, userFacingWriteError(root, dir, err)
	}
	return created, nil
}

// removeCreatedDirs 删除回滚时要清理的目录：由深到浅（长路径优先），只删空目录。
func removeCreatedDirs(root *rootFS, dirs []string) {
	if len(dirs) == 0 {
		return
	}
	ordered := append([]string(nil), dirs...)
	sort.SliceStable(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })
	for _, dir := range ordered {
		_ = root.guard.Remove(guardPath(dir))
	}
}

// removeIfExists 删除一个根相对路径；不存在视为成功（幂等）。
func removeIfExists(root *rootFS, rel string) error {
	if err := root.guard.Remove(guardPath(rel)); err != nil && !isNotExist(err) {
		return err
	}
	return nil
}

// leakedPathError 统一"元信息查询失败"的文案：逃逸错误翻译成根内视角，
// 其余错误补上根相对路径。绝不把绝对路径或临时文件名透给模型。
func leakedPathError(root *rootFS, rel string, err error) error {
	if isEscape(err) {
		if target, isAbsLink := root.finalAbsoluteLink(rel); isAbsLink {
			return fmt.Errorf("%s: symbolic link points to the absolute path %q; only relative link targets inside the workspace root are supported", rel, target)
		}
		return fmt.Errorf("%s: resolves to %q which is outside the workspace root %q", rel, root.absFor(rel), root.root)
	}
	return fmt.Errorf("%s: %w", rel, err)
}

// userFacingWriteError 把落盘阶段的错误翻译成面向模型的文案。
func userFacingWriteError(root *rootFS, rel string, err error) error {
	if isEscape(err) {
		return fmt.Errorf("%s: resolves outside the workspace root %q", rel, root.root)
	}
	return err
}

// summarize 渲染模型可见的成功结果：每个文件的 Op 与 +N/-M，加总计。
func summarize(plans []plan, dryRun bool) string {
	added, removed := 0, 0
	var builder strings.Builder
	for _, item := range plans {
		added += item.added
		removed += item.removed
	}
	prefix := ""
	if dryRun {
		prefix = "[dry-run] "
	}
	fmt.Fprintf(&builder, "%sapplied patch: %d file(s), +%d -%d lines\n", prefix, len(plans), added, removed)
	for _, item := range plans {
		switch {
		case item.moveRel != "":
			fmt.Fprintf(&builder, "  move %s -> %s (+%d -%d)\n", item.rel, item.moveRel, item.added, item.removed)
		case item.op == opAdd:
			fmt.Fprintf(&builder, "  add %s (+%d -%d)\n", item.rel, item.added, item.removed)
		case item.op == opDelete:
			fmt.Fprintf(&builder, "  delete %s (+%d -%d)\n", item.rel, item.added, item.removed)
		default:
			fmt.Fprintf(&builder, "  update %s (+%d -%d)\n", item.rel, item.added, item.removed)
		}
	}
	if dryRun {
		builder.WriteString(dryRunTailLine)
	}
	return builder.String()
}

// claimedPath 是"已占用路径"的展示名与去重键（大小写不敏感平台上键是小写）。
type claimedPath struct {
	rel string
	key string
}

// claimedTarget 记录某个物理路径被哪个补丁操作占用，用于"同一文件被写两次"的报错。
type claimedTarget struct {
	rel  string
	line int
}

// checkPolicyPath 校验一个绝对写入路径是否被沙箱策略允许。
//
// **abs 必须是解析后的落点**（符号链接目录已展开），不能是补丁里写的词法路径：
// 策略的 deny glob 与保护区都是字符串匹配，词法路径 `root/shared/x` 匹配不上
// `secrets/**`，会变成一条绕过通道。调用方见 buildPlans（先用 root.resolve 解析、
// 再用 finalLinkTarget 覆盖最后一段链接）。
//
// policy 为 nil 时直接放行（保持"只靠 workspace root 围栏"的旧行为）。非 nil 时
// 拦截三类情况：只读模式、策略的拒绝 glob（如 **/.env）、保护区（ProtectedPaths）。
func checkPolicyPath(policy *sandbox.Policy, rel, abs string) error {
	if policy == nil {
		return nil
	}
	if policy.ReadOnly() {
		return fmt.Errorf("%s: sandbox policy is read-only; no file can be modified", rel)
	}
	// 真正的判定交给策略：CheckWritablePath 会先把 abs 解析成**真实落点**再判
	// 拒绝 glob 与保护路径。词法前缀比较挡不住"末段链接指向经链接的路径"，例如
	// alias -> .git 且 link -> alias/config：词法 abs 是 root/link 或
	// root/alias/config，都不以 root/.git 开头，而实际写入落在 .git/config。
	if err := policy.CheckWritablePath(abs); err != nil {
		return fmt.Errorf("%s: %w", rel, err)
	}
	return nil
}
