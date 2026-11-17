package patch

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// 本文件把补丁文本解析成与格式无关的操作列表：两种语法（统一 diff、Codex 风格）
// 在这里汇合，之后的匹配与落盘完全不关心补丁长什么样。
//
// 解析只做语法与计数校验，不做任何文件 I/O：真正的"能不能应用"在 apply.go 里
// 判断。两段式的好处是语法错误永远不会碰到磁盘。

// opKind 是一次文件操作的类别。
type opKind string

const (
	// opAdd 新增文件（内容全部来自 '+' 行）。
	opAdd opKind = "add"
	// opUpdate 修改文件（可选地移动到新路径）。
	opUpdate opKind = "update"
	// opDelete 删除文件（内容必须与补丁里的 '-' 行一致）。
	opDelete opKind = "delete"
)

// hunkLine 是 hunk 里的一行改动。
type hunkLine struct {
	// kind 是 ' '（上下文）、'-'（删除）或 '+'（新增）。
	kind byte
	// text 是不含行尾的文本（行尾统一在 apply 阶段处理）。
	text string
}

// hunk 是一个连续改动块。
type hunk struct {
	// anchor 是 Codex 风格的 "@@ 上下文锚点"（可为空）。它只用于缩小搜索起点。
	anchor string
	// line 是补丁文本里该 hunk 起始处的行号（1 基），用于错误定位。
	line int
	// hasCounts 表示这份 hunk 来自统一 diff 并带有行数声明。
	hasCounts bool
	// oldStart/newStart 是统一 diff 声明的起始行号（1 基；oldStart 可为 0）。
	oldStart int
	newStart int
	// oldCount/newCount 是统一 diff 声明的行数。
	oldCount int
	newCount int
	// lines 是 hunk 正文。
	lines []hunkLine
}

// fileOp 是一个文件级操作。
type fileOp struct {
	kind opKind
	// path 是补丁里写的路径（未规范化）。
	path string
	// moveTo 是 Codex 风格 "@@ Move to" 的目标路径（仅 opUpdate 可能非空）。
	moveTo string
	// hunks 只对 opUpdate 有意义。
	hunks []hunk
	// line 是补丁文本里该操作起始处的行号（1 基）。
	line int
}

// expected 返回 hunk 里必须与文件匹配的行（上下文 + 删除行），顺序不变。
func (h hunk) expected() []string {
	out := make([]string, 0, len(h.lines))
	for _, line := range h.lines {
		if line.kind == '+' {
			continue
		}
		out = append(out, line.text)
	}
	return out
}

// counts 返回该 hunk 的 +N/-M 统计。
func (h hunk) counts() (added, removed int) {
	for _, line := range h.lines {
		switch line.kind {
		case '+':
			added++
		case '-':
			removed++
		}
	}
	return added, removed
}

// patchLine 把补丁文本切成行（去掉行尾的 "\r"，兼容 CRLF 传输的补丁），并保留
// 1 基行号。末尾的空行不产生额外元素。
func patchLines(text string) []string {
	text = strings.TrimPrefix(text, "\ufeff")
	if text == "" {
		return nil
	}
	raw := strings.Split(text, "\n")
	// 以换行结尾时 Split 会多出一个空串，它不是一行内容。
	if len(raw) > 0 && raw[len(raw)-1] == "" {
		raw = raw[:len(raw)-1]
	}
	out := make([]string, len(raw))
	for index, line := range raw {
		out[index] = strings.TrimSuffix(line, "\r")
	}
	return out
}

// parsePatch 自动识别补丁格式并返回操作列表。
func parsePatch(text string) ([]fileOp, error) {
	if strings.TrimSpace(text) == "" {
		return nil, errors.New("patch is required and must not be empty")
	}
	lines := patchLines(text)
	first := firstNonBlank(lines)
	if first < 0 {
		return nil, errors.New("patch is required and must not be empty")
	}
	if strings.TrimSpace(lines[first]) == "*** Begin Patch" {
		return parseCodexPatch(lines[first:], first+1)
	}
	// 统一 diff 可能前面还有 git 的元数据行（diff --git / index ...）：跳过它们再
	// 判断第一条真正的补丁行。
	for first < len(lines) && (strings.TrimSpace(lines[first]) == "" || isGitDiffMetadata(lines[first])) {
		first++
	}
	if first >= len(lines) {
		return nil, errors.New("patch contains no file operations")
	}
	for _, line := range lines[first:] {
		if strings.HasPrefix(strings.TrimSpace(line), "*** ") {
			return nil, errors.New("patch uses Codex directives but does not start with *** Begin Patch")
		}
	}
	if strings.HasPrefix(strings.TrimSpace(lines[first]), "--- ") {
		return parseUnifiedPatch(lines[first:], first+1)
	}
	return nil, fmt.Errorf("unrecognized patch format on line %d: expected a unified diff (--- / +++ headers) or a Codex patch (*** Begin Patch)", first+1)
}

// firstNonBlank 返回第一行非空白行的下标，没有则 -1。
func firstNonBlank(lines []string) int {
	for index, line := range lines {
		if strings.TrimSpace(line) != "" {
			return index
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// 统一 diff
// ---------------------------------------------------------------------------

// parseUnifiedPatch 解析统一 diff。offset 是 lines[0] 在原补丁文本里的 1 基行号，
// 用于错误信息。
func parseUnifiedPatch(lines []string, offset int) ([]fileOp, error) {
	var ops []fileOp
	index := 0
	for index < len(lines) {
		if strings.TrimSpace(lines[index]) == "" {
			index++
			continue
		}
		if isGitDiffMetadata(lines[index]) {
			// git diff 的元数据行（diff --git / index / mode / similarity）不影响语义：
			// 真正的路径与内容是随后的 --- / +++ 头与 hunk。
			index++
			continue
		}
		if !strings.HasPrefix(lines[index], "--- ") {
			return nil, fmt.Errorf("line %d: expected a \"--- \" file header, got %q", offset+index, lines[index])
		}
		opLine := offset + index
		source := unifiedHeaderPath(lines[index][len("--- "):])
		index++
		if index >= len(lines) || !strings.HasPrefix(lines[index], "+++ ") {
			return nil, fmt.Errorf("line %d: the \"--- \" header must be followed by a \"+++ \" header", opLine)
		}
		target := unifiedHeaderPath(lines[index][len("+++ "):])
		index++

		op := fileOp{kind: opUpdate, path: target, line: opLine}
		switch {
		case source == "" || target == "":
			return nil, fmt.Errorf("line %d: file header is missing a path", opLine)
		case source == devNull && target == devNull:
			return nil, fmt.Errorf("line %d: both sides of the diff are /dev/null", opLine)
		case source == devNull:
			op.kind = opAdd
			op.path = target
		case target == devNull:
			op.kind = opDelete
			op.path = source
		case source != target:
			// git 的 rename diff：内容来自 source，移动到 target。
			op.path = source
			op.moveTo = target
		}

		for index < len(lines) && strings.HasPrefix(lines[index], "@@") {
			h, next, err := parseUnifiedHunk(lines, index, offset)
			if err != nil {
				return nil, err
			}
			op.hunks = append(op.hunks, h)
			index = next
		}
		if op.kind == opAdd && len(op.hunks) == 0 {
			return nil, fmt.Errorf("line %d: add-file diff has no hunks", opLine)
		}
		ops = append(ops, op)
	}
	if len(ops) == 0 {
		return nil, errors.New("patch contains no file operations")
	}
	return ops, nil
}

// isGitDiffMetadata 报告一行是否是 git 生成的、对补丁语义没有影响的头部元数据。
//
// 注意 "rename from/to" 被忽略：真正的重命名由 "--- a/旧" 与 "+++ b/新" 表达；
// 只改名字、没有内容变化的 rename（git 不会给出 ---/+++ 与 hunk）无法被本工具
// 表达，会在最后以"patch contains no file operations"报错。
func isGitDiffMetadata(line string) bool {
	for _, prefix := range []string{
		"diff --git ",
		"index ",
		"old mode ",
		"new mode ",
		"new file mode ",
		"deleted file mode ",
		"similarity index ",
		"dissimilarity index ",
		"rename from ",
		"rename to ",
	} {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

// devNull 是统一 diff 里表示"这一侧不存在"的特殊路径。
const devNull = "/dev/null"

// unifiedHeaderPath 从 "--- " / "+++ " 头部取路径：还原 git 的 C 风格引号、剥掉
// a/ b/ 前缀与尾部的时间戳/制表符信息。
func unifiedHeaderPath(rest string) string {
	path := strings.TrimSpace(rest)
	if tab := strings.IndexByte(path, '\t'); tab >= 0 {
		path = strings.TrimSpace(path[:tab])
	}
	if path == devNull {
		return devNull
	}
	if unquoted, ok := unquoteGitPath(path); ok {
		path = unquoted
	}
	if strings.HasPrefix(path, "a/") || strings.HasPrefix(path, "b/") {
		return path[2:]
	}
	return path
}

// unquoteGitPath 还原 git 对特殊路径加的 C 风格引号。
//
// core.quotePath 默认开启：含非 ASCII 字节、引号、反斜杠或控制字符的路径会被写成
// "a/\344\270\255\346\226\207.md" 这种形态（八进制转义是**原始字节**）。不还原
// 的话，真实 git diff 里的中文文件名会被当成名字里带引号和反斜杠的文件。
//
// 只在确实出现转义时才还原：形态像带引号、但内部没有反斜杠的路径按字面处理，
// 避免把文件名本来就带引号的情况改错。
func unquoteGitPath(path string) (string, bool) {
	if len(path) < 3 || path[0] != '"' || path[len(path)-1] != '"' {
		return path, false
	}
	body := path[1 : len(path)-1]
	if !strings.ContainsRune(body, '\\') {
		return path, false
	}
	var builder strings.Builder
	builder.Grow(len(body))
	for index := 0; index < len(body); index++ {
		char := body[index]
		if char != '\\' {
			builder.WriteByte(char)
			continue
		}
		index++
		if index >= len(body) {
			return path, false
		}
		switch escaped := body[index]; escaped {
		case 'a':
			builder.WriteByte('\a')
		case 'b':
			builder.WriteByte('\b')
		case 'f':
			builder.WriteByte('\f')
		case 'n':
			builder.WriteByte('\n')
		case 'r':
			builder.WriteByte('\r')
		case 't':
			builder.WriteByte('\t')
		case 'v':
			builder.WriteByte('\v')
		case '\\', '"':
			builder.WriteByte(escaped)
		case '0', '1', '2', '3', '4', '5', '6', '7':
			// git 固定用三位八进制表示一个原始字节。
			if index+2 >= len(body) {
				return path, false
			}
			value := 0
			for offset := 0; offset < 3; offset++ {
				digit := body[index+offset]
				if digit < '0' || digit > '7' {
					return path, false
				}
				value = value*8 + int(digit-'0')
			}
			builder.WriteByte(byte(value))
			index += 2
		default:
			// 不认识的转义：整体按字面路径处理（fail closed，不猜）。
			return path, false
		}
	}
	return builder.String(), true
}

// parseUnifiedHunk 从 lines[start]（"@@ ..."）开始解析一个 hunk，返回 hunk 与下
// 一个未消费的下标。
func parseUnifiedHunk(lines []string, start, offset int) (hunk, int, error) {
	header := lines[start]
	oldStart, oldCount, newStart, newCount, err := parseHunkHeader(header)
	if err != nil {
		return hunk{}, start, fmt.Errorf("line %d: %w", offset+start, err)
	}
	h := hunk{line: offset + start, hasCounts: true, oldStart: oldStart, oldCount: oldCount, newStart: newStart, newCount: newCount}
	index := start + 1
	for index < len(lines) {
		line := lines[index]
		if strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "@@") {
			break
		}
		if strings.HasPrefix(line, "\\") {
			// "\ No newline at end of file"：忽略。实际换行状态以文件为准。
			index++
			continue
		}
		if line == "" {
			// 空行在 unified diff 里表示"一个空的上下文行"的唯一合法写法是 " "，
			// 但很多生成器会输出真正的空行。按上下文行处理后由计数校验兜底。
			h.lines = append(h.lines, hunkLine{kind: ' ', text: ""})
			index++
			continue
		}
		switch line[0] {
		case ' ', '-', '+':
			h.lines = append(h.lines, hunkLine{kind: line[0], text: line[1:]})
			index++
			continue
		}
		break
	}
	var oldLines, newLines int
	for _, line := range h.lines {
		switch line.kind {
		case ' ':
			oldLines++
			newLines++
		case '-':
			oldLines++
		case '+':
			newLines++
		}
	}
	if oldLines != h.oldCount || newLines != h.newCount {
		return hunk{}, start, fmt.Errorf("line %d: hunk declares -%d,+%d line(s) but its body has -%d,+%d", offset+start, h.oldCount, h.newCount, oldLines, newLines)
	}
	return h, index, nil
}

// parseHunkHeader 解析 "@@ -oldStart,oldCount +newStart,newCount @@ [anchor]"。
func parseHunkHeader(header string) (oldStart, oldCount, newStart, newCount int, err error) {
	text := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(header), "@@"))
	if close := strings.Index(text, "@@"); close >= 0 {
		text = strings.TrimSpace(text[:close])
	}
	fields := strings.Fields(text)
	if len(fields) != 2 {
		return 0, 0, 0, 0, fmt.Errorf("malformed hunk header %q: expected \"@@ -l,n +l,n @@\"", header)
	}
	oldStart, oldCount, err = parseHunkRange(fields[0], "-")
	if err != nil {
		return 0, 0, 0, 0, fmt.Errorf("malformed hunk header %q: %w", header, err)
	}
	newStart, newCount, err = parseHunkRange(fields[1], "+")
	if err != nil {
		return 0, 0, 0, 0, fmt.Errorf("malformed hunk header %q: %w", header, err)
	}
	return oldStart, oldCount, newStart, newCount, nil
}

// parseHunkRange 解析 "-l"、"-l,n" 形式的范围（n 省略表示 1）。
func parseHunkRange(field, prefix string) (start, count int, err error) {
	if !strings.HasPrefix(field, prefix) {
		return 0, 0, fmt.Errorf("range %q must start with %q", field, prefix)
	}
	body := field[len(prefix):]
	parts := strings.SplitN(body, ",", 2)
	start, err = strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || start < 0 {
		return 0, 0, fmt.Errorf("invalid start line in %q", field)
	}
	count = 1
	if len(parts) == 2 {
		count, err = strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil || count < 0 {
			return 0, 0, fmt.Errorf("invalid line count in %q", field)
		}
	}
	return start, count, nil
}

// ---------------------------------------------------------------------------
// Codex 风格
// ---------------------------------------------------------------------------

// Codex 风格的固定指令前缀。
const (
	codexBegin     = "*** Begin Patch"
	codexEnd       = "*** End Patch"
	codexAddFile   = "*** Add File: "
	codexUpdate    = "*** Update File: "
	codexDelete    = "*** Delete File: "
	codexMoveTo    = "*** Move to: "
	codexEndOfFile = "*** End of File"
)

// parseCodexPatch 解析 "*** Begin Patch" ... "*** End Patch"。offset 是 lines[0]
// 在原补丁文本里的 1 基行号。
func parseCodexPatch(lines []string, offset int) ([]fileOp, error) {
	var ops []fileOp
	index := 1 // lines[0] 是 *** Begin Patch
	// current 指向正在填充的操作；nil 表示还没有遇到任何指令。
	var current *fileOp
	// hunkOpen 表示 current 里已经开了 hunk（后续 ' '/'-'/'+' 行继续填充它）。
	var hunkOpen bool

	finish := func() {
		if current != nil {
			ops = append(ops, *current)
			current = nil
		}
		hunkOpen = false
	}

	sawEnd := false
	for index < len(lines) {
		raw := lines[index]
		trimmed := strings.TrimSpace(raw)
		lineNo := offset + index

		if trimmed == codexEnd {
			finish()
			sawEnd = true
			index++
			// End 之后只允许空白行。
			for ; index < len(lines); index++ {
				if strings.TrimSpace(lines[index]) != "" {
					return nil, fmt.Errorf("line %d: unexpected content after *** End Patch", offset+index)
				}
			}
			break
		}
		if trimmed == codexEndOfFile {
			// Codex 的"文件结束"标记：对补丁结果没有额外约束，忽略。
			index++
			continue
		}
		switch {
		case strings.HasPrefix(raw, codexAddFile):
			finish()
			current = &fileOp{kind: opAdd, path: strings.TrimSpace(raw[len(codexAddFile):]), line: lineNo}
			index++
			for index < len(lines) && !isCodexDirective(lines[index]) {
				body := lines[index]
				if strings.TrimSpace(body) == "" {
					index++
					continue
				}
				if body[0] != '+' {
					return nil, fmt.Errorf("line %d: Add File lines must start with \"+\" (got %q)", offset+index, body)
				}
				current.hunks = append(current.hunks, hunk{line: offset + index, lines: []hunkLine{{kind: '+', text: body[1:]}}})
				index++
			}
		case strings.HasPrefix(raw, codexUpdate):
			finish()
			current = &fileOp{kind: opUpdate, path: strings.TrimSpace(raw[len(codexUpdate):]), line: lineNo}
			index++
		case strings.HasPrefix(raw, codexDelete):
			finish()
			current = &fileOp{kind: opDelete, path: strings.TrimSpace(raw[len(codexDelete):]), line: lineNo}
			index++
		case strings.HasPrefix(raw, codexMoveTo):
			if current == nil || current.kind != opUpdate {
				return nil, fmt.Errorf("line %d: \"*** Move to\" must follow an Update File section", lineNo)
			}
			if current.moveTo != "" {
				return nil, fmt.Errorf("line %d: duplicate \"*** Move to\" for %s", lineNo, current.path)
			}
			current.moveTo = strings.TrimSpace(raw[len(codexMoveTo):])
			hunkOpen = false
			index++
		case strings.HasPrefix(trimmed, "@@"):
			if current == nil || current.kind != opUpdate {
				return nil, fmt.Errorf("line %d: \"@@\" must appear inside an Update File section", lineNo)
			}
			anchor := strings.TrimSpace(strings.TrimPrefix(trimmed, "@@"))
			current.hunks = append(current.hunks, hunk{anchor: anchor, line: lineNo})
			hunkOpen = true
			index++
		case strings.HasPrefix(trimmed, "*** "):
			return nil, fmt.Errorf("line %d: unknown patch directive %q", lineNo, trimmed)
		case raw != "" && (raw[0] == ' ' || raw[0] == '-' || raw[0] == '+'):
			if current == nil || current.kind != opUpdate {
				return nil, fmt.Errorf("line %d: change lines must appear inside an Update File section", lineNo)
			}
			if !hunkOpen {
				// 允许省略 "@@ 锚点"的隐式 hunk。
				current.hunks = append(current.hunks, hunk{line: lineNo})
				hunkOpen = true
			}
			current.hunks[len(current.hunks)-1].lines = append(current.hunks[len(current.hunks)-1].lines,
				hunkLine{kind: raw[0], text: raw[1:]})
			index++
		case trimmed == "":
			index++
		default:
			return nil, fmt.Errorf("line %d: unexpected line %q", lineNo, raw)
		}
	}
	if !sawEnd {
		return nil, errors.New("patch is missing the closing *** End Patch")
	}
	finish()
	if len(ops) == 0 {
		return nil, errors.New("patch contains no file operations")
	}
	// Add File 的正文在解析时被拆成每行一个 hunk，这里合并回一个 hunk。
	for index := range ops {
		op := &ops[index]
		if op.kind != opAdd || len(op.hunks) <= 1 {
			continue
		}
		merged := hunk{line: op.hunks[0].line}
		for _, h := range op.hunks {
			merged.lines = append(merged.lines, h.lines...)
		}
		op.hunks = []hunk{merged}
	}
	return ops, nil
}

// isCodexDirective 报告一行是否是 Codex 指令（Add File 的正文在它之前结束）。
func isCodexDirective(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == codexEnd || trimmed == codexEndOfFile {
		return true
	}
	for _, prefix := range []string{codexBegin, codexAddFile, codexUpdate, codexDelete, codexMoveTo} {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	return strings.HasPrefix(trimmed, "@@") || strings.HasPrefix(trimmed, "*** ")
}
