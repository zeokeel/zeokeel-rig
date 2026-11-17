package instructions

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// @import 展开用到的模板。模型可见：失败处必须留下可读的痕迹，而不是静默消失。
const (
	// notFoundTemplate 对应"目标不存在/读不出来"。
	notFoundTemplate = "<!-- import not found: %s -->"
	// cycleTemplate 对应循环引用（A→B→A）。
	cycleTemplate = "<!-- import skipped: %s (cycle) -->"
	// depthTemplate 对应超过 WithMaxImportDepth。
	depthTemplate = "<!-- import skipped: %s (max depth) -->"
)

// expander 保存一次文档展开的可变状态。
//
// stack 是当前递归链上的绝对路径（用于循环检测），imports 记录出现过的 import
// 目标（按出现顺序去重），maxDepth 是允许的嵌套上限。
type expander struct {
	maxDepth int
	stack    []string
	imports  []string
	recorded map[string]struct{}
	// importRoot 是"仓库内文档"允许 import 的真实根目录；空串表示不限制。
	importRoot string
}

// expandDocument 展开 content 里的 @import 指令（不限制 import 根），返回展开后
// 的文本与 import 目标列表（含成功与失败）。path 是 content 所属文档的绝对路径，
// 用于解析相对 import。
func expandDocument(path, content string, maxDepth int) (string, []string) {
	return expandDocumentWithin(path, content, maxDepth, "")
}

// expandDocumentWithin 与 expandDocument 相同，但额外把 import 目标限制在
// importRoot 之内（空串表示不限制）。越界的 import 按"找不到"处理：既不读取，
// 也不暗示根外路径是否存在。
func expandDocumentWithin(path, content string, maxDepth int, importRoot string) (string, []string) {
	current := &expander{
		maxDepth:   maxDepth,
		stack:      []string{path},
		recorded:   make(map[string]struct{}),
		importRoot: importRoot,
	}
	expanded := current.expand(path, content, 0)
	return expanded, current.imports
}

// allows 报告一个已解析的 import 目标是否在 importRoot 之内。
//
// 判定用 EvalSymlinks 之后的真实路径：仓库内一个指向 /etc 的符号链接不能成为
// 逃逸通道。
func (e *expander) allows(resolved string) bool {
	if e.importRoot == "" {
		return true
	}
	real, err := filepath.EvalSymlinks(resolved)
	if err != nil {
		return false
	}
	return withinImportRoot(e.importRoot, filepath.Clean(real))
}

// withinImportRoot 报告 path 是否等于 root 或位于 root 之下（两者都应是真实绝对
// 路径）。
func withinImportRoot(root, path string) bool {
	if root == path {
		return true
	}
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	if relative == "." {
		return true
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// expand 处理一层文档：逐行扫描，把独占一行的 @path 替换为被引入文件的展开内容。
//
// 被引入内容原样内联：若它不以换行结尾，补一个换行，保证原文档后续行仍从行首
// 开始；指令行本身（含它的换行）被完全替换掉。
//
// 围栏代码块（``` 或 ~~~）内部不做展开：AGENTS.md 里常有"如何写 import"的示例
// 代码块，把示例里的 @path 真展开会凭空多出内容。围栏状态按文件独立维护
// （被 import 的文件拥有自己的围栏状态）。
func (e *expander) expand(path, content string, depth int) string {
	var builder strings.Builder
	var current fence
	for _, line := range splitLinesKeepingEnds(content) {
		text := strings.TrimSuffix(line, "\n")
		trimmed := strings.TrimSpace(text)
		if current.marker != 0 {
			// 围栏内：除了"纯围栏且不短于开启围栏"的行，其余都是内容。
			if current.closes(trimmed) {
				current = fence{}
			}
			builder.WriteString(line)
			continue
		}
		if opened, ok := openFence(text); ok {
			current = opened
			builder.WriteString(line)
			continue
		}
		target, ok := parseImport(text)
		if !ok {
			builder.WriteString(line)
			continue
		}
		resolved, found := resolveImport(path, target)
		if !found && !looksLikeImportPath(target) && !importTargetExists(path, target) {
			// 既不是路径形状、也不指向任何存在的对象：这是正文（例如 npm 包名
			// "@types/node"），不是 import 指令。把它替换成 not-found 注释会静默
			// 丢掉文档内容，因此原样保留。
			builder.WriteString(line)
			continue
		}
		if found && !e.allows(resolved) {
			// 越界的 import 与"找不到"同样处理：不读文件，也不告诉调用方根外是否存在。
			e.record(target)
			writeComment(&builder, fmt.Sprintf(notFoundTemplate, target))
			continue
		}
		e.record(target)
		switch {
		case !found:
			writeComment(&builder, fmt.Sprintf(notFoundTemplate, target))
		case depth >= e.maxDepth:
			// 顶层文档是第 0 层；depth == maxDepth 时不再展开下一层。
			writeComment(&builder, fmt.Sprintf(depthTemplate, target))
		case e.onStack(resolved):
			writeComment(&builder, fmt.Sprintf(cycleTemplate, target))
		default:
			data, err := os.ReadFile(resolved)
			if err != nil {
				writeComment(&builder, fmt.Sprintf(notFoundTemplate, target))
				continue
			}
			e.stack = append(e.stack, resolved)
			child := e.expand(resolved, sanitizeText(string(data)), depth+1)
			e.stack = e.stack[:len(e.stack)-1]
			builder.WriteString(child)
			if child != "" && !strings.HasSuffix(child, "\n") {
				builder.WriteString("\n")
			}
		}
	}
	return builder.String()
}

// record 记录一个 import 目标（按出现顺序去重）。
func (e *expander) record(target string) {
	if _, exists := e.recorded[target]; exists {
		return
	}
	e.recorded[target] = struct{}{}
	e.imports = append(e.imports, target)
}

// onStack 报告路径是否已在当前递归链上（含顶层文档自身，因此自引用也被拦截）。
func (e *expander) onStack(path string) bool {
	for _, candidate := range e.stack {
		if candidate == path {
			return true
		}
	}
	return false
}

// resolveImport 把 import 目标解析成存在的绝对文件路径。相对路径相对**当前文档
// 所在目录**；目录不算命中（import 只能指向文件）。
func resolveImport(documentPath, target string) (string, bool) {
	candidate := target
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(filepath.Dir(documentPath), candidate)
	}
	absolute, err := filepath.Abs(candidate)
	if err != nil {
		return "", false
	}
	absolute = filepath.Clean(absolute)
	info, err := os.Stat(absolute)
	if err != nil || !info.Mode().IsRegular() {
		// 目录不算命中；FIFO/socket/设备文件更不算——对它们 ReadFile 会永久阻塞。
		return "", false
	}
	return absolute, true
}

// fence 描述一个打开的围栏代码块。零值表示"不在围栏内"。
type fence struct {
	// marker 是围栏字符：'`'（反引号）或 '~'（波浪号）。
	marker byte
	// length 是开启围栏的字符数（>= 3）；关闭围栏不得短于它。
	length int
}

// openFence 判断一行（不含行尾换行、保留前导缩进）是否开启围栏代码块。
//
// 识别规则按 CommonMark 的最小集合：最多 3 列缩进，然后是至少 3 个同一种字符
// （` 或 ~），其后是可选 info string。反引号围栏的 info string 不得再含反引号
// （否则它只是普通文本，而不是围栏）。
//
// 4 列及以上缩进是**缩进代码块**而不是围栏（CommonMark）：把它当围栏会让其后
// 真正的 @import 被静默跳过，直到某个"看起来像结束围栏"的行为止。
func openFence(line string) (fence, bool) {
	indent := 0
	for indent < len(line) {
		switch line[indent] {
		case ' ':
			indent++
			continue
		case '\t':
			// 制表符至少推进到第 4 列：按缩进代码块处理。
			return fence{}, false
		}
		break
	}
	if indent > 3 {
		return fence{}, false
	}
	trimmed := strings.TrimSpace(line)
	if len(trimmed) < 3 {
		return fence{}, false
	}
	marker := trimmed[0]
	if marker != '`' && marker != '~' {
		return fence{}, false
	}
	length := 0
	for length < len(trimmed) && trimmed[length] == marker {
		length++
	}
	if length < 3 {
		return fence{}, false
	}
	if marker == '`' && strings.ContainsRune(trimmed[length:], '`') {
		return fence{}, false
	}
	return fence{marker: marker, length: length}, true
}

// closes 判断一行（已 TrimSpace）是否关闭当前围栏：必须由同一种字符组成、长度
// 不小于开启围栏，且其后只有空白（CommonMark：关闭围栏不能带 info string）。
func (f fence) closes(trimmed string) bool {
	if f.marker == 0 {
		return false
	}
	length := 0
	for length < len(trimmed) && trimmed[length] == f.marker {
		length++
	}
	if length < f.length {
		return false
	}
	return strings.TrimSpace(trimmed[length:]) == ""
}

// parseImport 判断一行（不含行尾换行）是否为 @import 指令，并返回目标路径。
//
// 规则刻意严格：
//   - 整行（忽略首尾空白）必须以 '@' 开头，且 '@' 之后必须有内容；
//   - 带引号的形式 `@"a b.md"` / `@'a b.md'` 允许路径含空格；
//   - 不带引号时路径不得含空白，否则视为普通文本（避免 "见 @foo bar" 之类误判）；
//   - 只接受独占一行的指令，行内出现的 '@' 不展开。
func parseImport(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if len(trimmed) < 2 || trimmed[0] != '@' {
		return "", false
	}
	rest := strings.TrimSpace(trimmed[1:])
	if rest == "" {
		return "", false
	}
	if len(rest) >= 2 {
		first, last := rest[0], rest[len(rest)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			inner := strings.TrimSpace(rest[1 : len(rest)-1])
			if inner == "" {
				return "", false
			}
			return inner, true
		}
	}
	if strings.ContainsAny(rest, " \t") {
		return "", false
	}
	return rest, true
}

// looksLikeImportPath 报告一个 @ 目标是否"看起来是路径"，用于区分真正的 import
// 与正文里的 @mention（例如一行一个包名 "@types/node"）。
//
// 判定刻意保守：显式相对/绝对路径，或以常见文档扩展名结尾，才算路径。
func looksLikeImportPath(target string) bool {
	if target == "" {
		return false
	}
	if filepath.IsAbs(target) {
		return true
	}
	for _, prefix := range []string{"./", "../", "/", "~/", `.
`, `..\`} {
		if prefix != "" && strings.HasPrefix(target, prefix) {
			return true
		}
	}
	lowered := strings.ToLower(target)
	for _, suffix := range []string{".md", ".markdown", ".mdown", ".txt", ".rst", ".adoc"} {
		if strings.HasSuffix(lowered, suffix) {
			return true
		}
	}
	return false
}

// importTargetExists 报告 import 目标是否解析到任何存在的对象（文件或目录）。
// 目录不算可 import 的文件，但"存在"说明这一行确实是 import 意图，应留下
// not-found 注释而不是当成正文。
func importTargetExists(documentPath, target string) bool {
	candidate := target
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(filepath.Dir(documentPath), candidate)
	}
	absolute, err := filepath.Abs(candidate)
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Clean(absolute))
	return err == nil
}

// writeComment 写入一行失败/跳过注释。
func writeComment(builder *strings.Builder, comment string) {
	builder.WriteString(comment)
	builder.WriteString("\n")
}

// splitLinesKeepingEnds 按 '\n' 切分，并保留每行末尾的换行符（最后一行可能没有）。
// 保留换行符是为了让未命中的行原样写回，从而保证"没有 @import 的文档内容逐字节
// 不变"。
func splitLinesKeepingEnds(content string) []string {
	if content == "" {
		return nil
	}
	lines := make([]string, 0, strings.Count(content, "\n")+1)
	for {
		index := strings.IndexByte(content, '\n')
		if index < 0 {
			lines = append(lines, content)
			return lines
		}
		lines = append(lines, content[:index+1])
		content = content[index+1:]
		if content == "" {
			return lines
		}
	}
}

// sanitizeText 把文件内容里的非法字节替换掉：NUL 与非法 UTF-8 序列都变成 U+FFFD。
//
// 指令文档最终会进入 prompt（通常还要过一次 JSON 编码）。二进制文件、latin-1 文本
// 或含 NUL 的"文本"原样内联会让 prompt 里出现非法 UTF-8/控制字节——不同宿主对
// 它们的处理不一致（JSON 编码会静默替换），因此这里统一成确定的形式。
func sanitizeText(text string) string {
	if strings.IndexByte(text, 0) >= 0 {
		text = strings.ReplaceAll(text, "\x00", "\uFFFD")
	}
	if !utf8.ValidString(text) {
		text = strings.ToValidUTF8(text, "\uFFFD")
	}
	return text
}

// truncate 把展开后的内容限制在 max 字节内：保留头部、按 rune 边界回退，并追加
// 形如 `\n…[truncated: N bytes omitted]` 的尾注（N 是被丢弃的字节数）。
//
// max <= 0 表示不截断（调用方负责解释"零上限"的语义；Load 用默认值填充）。
func truncate(content string, max int) string {
	if max <= 0 || len(content) <= max {
		return content
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(content[cut]) {
		cut--
	}
	omitted := len(content) - cut
	return content[:cut] + fmt.Sprintf("\n…[truncated: %d bytes omitted]", omitted)
}
