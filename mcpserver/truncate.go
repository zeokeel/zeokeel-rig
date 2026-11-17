package mcpserver

// 本文件是 tools/call 结果的输出上限：防止一个巨型工具结果把对外的 MCP 会话
// （以及客户端的上下文窗口）撑爆。
//
// 语义（重要）：
//   - 超限是**正常结果**，不是 JSON-RPC error：模型需要看到"这里被截断了"，
//     而不是一个调用失败；
//   - 只截断**最长**的那个 text 块，其它块保持原样；
//   - 被截断的块末尾追加 "\n…[truncated: N bytes omitted]"，并带上
//     `_meta: {"truncated": true}`，N 是**实际被丢弃的内容字节数**（含为了给
//     注释腾地方而多丢的部分）；
//   - 非 text 块（image/audio/resource）不占预算、绝不修改；
//   - 截断点回退到 UTF-8 rune 边界：宁可少留 1-3 字节，也不产出半个汉字。

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

const (
	// ClientReadLimit 是**对端**（tools/mcp 客户端）单条 JSON-RPC 消息的读取上限，
	// 也是本包输出必须遵守的外部约束：tools/mcp 用
	// bufio.Scanner.Buffer(..., mcp.MaxMessageBytes) 读行，超过就是
	// "bufio.Scanner: token too long"，连接直接不可用（不是优雅报错）。
	//
	// 这里刻意复制常量而不是 import tools/mcp：server 不应该依赖 client 包。
	// tools/mcp/mcp.go:28 的 MaxMessageBytes 必须与它保持一致。
	ClientReadLimit = 8 << 20

	// DefaultMaxResultBytes 是未配置 WithMaxResultBytes 时的单次工具结果文本上限。
	//
	// 取客户端读取上限的 **一半**：一条响应行还要加上 JSON-RPC 信封、content 数组
	// 结构与字符串转义（`"`/`\` 变 2 字节，控制符与 `<`/`>`/`&` 变 6 字节），
	// 8 MiB 的内容上限会直接把对端撑爆。留一半余量是最省心的安全边界。
	DefaultMaxResultBytes = ClientReadLimit / 2

	// maxEncodedContentBytes 是 content 数组 JSON 编码后的硬上限（transport 保证）。
	//
	// 它独立于 WithMaxResultBytes：即使调用方把内容上限调到 100 MiB、或者内容里
	// 全是需要转义的字符（`"` 翻倍、控制符 6 倍），也不会写出超过客户端读取能力
	// 的行。留 2 MiB 给信封与 id/method 等字段。
	maxEncodedContentBytes = ClientReadLimit - 2<<20

	// MaxResultBytesUnlimited 是 WithMaxResultBytes 的"显式不限"取值。
	//
	// 注意 0 与其它非正数都回落到 DefaultMaxResultBytes：一个拼错的参数不应该
	// 悄悄关掉保护，"我要不限"必须写成 -1。-1 只关闭**内容**上限；单行响应仍然
	// 受 maxEncodedContentBytes 约束（那是传输层不变量，不能关）。
	MaxResultBytesUnlimited = -1
)

// truncateResult 按上限截断一次调用结果，并**始终**保证编码后的 content 不超过
// transport 硬上限。limit <= 0 表示内容不限（内部哨兵，仅由 -1 产生）。
func truncateResult(result wireCallResult, limit int) wireCallResult {
	if len(result.Content) == 0 {
		return result
	}
	if limit > 0 {
		result.Content = truncateContent(result.Content, limit)
	}
	result.Content = fitEncodedContent(result.Content, maxEncodedContentBytes)
	return result
}

// fitEncodedContent 是最后一道防线：把最长 text 块再收紧，使整个 content 数组的
// JSON 编码不超过 budget。
//
// 只按"内容字节"限制是不够的：JSON 转义会让 `"` 翻倍、控制符与 `<`/`>`/`&` 变 6
// 倍，一份 4 MiB 的引号内容编码后能到 8 MiB 以上，正好撞上客户端的读取上限。
// 这里按**转义后的真实字节数**反解保留长度（单遍扫描，O(n)），因此不会有逃逸。
func fitEncodedContent(blocks []wireContent, budget int) []wireContent {
	if budget <= 0 || len(blocks) == 0 {
		return blocks
	}
	encoded, err := json.Marshal(blocks)
	if err != nil || len(encoded) <= budget {
		return blocks
	}
	longest := longestTextIndex(blocks)
	if longest < 0 || blocks[longest].Text == "" {
		return blocks
	}
	// 其它块 + 信封结构 + 转义引号的开销：把最长块清空后测一次。
	probe := make([]wireContent, len(blocks))
	copy(probe, blocks)
	saved := probe[longest].Text
	probe[longest].Text = ""
	base, err := json.Marshal(probe)
	if err != nil {
		return blocks
	}
	remaining := budget - len(base) - 64 // 64 字节给注释本身留位
	if remaining <= 0 {
		remaining = 0
	}
	// 注释自己也要占字节（且数字位数会变），收紧两次直到收敛。
	noteCost := escapedLen(truncationNote(len(saved)))
	for attempt := 0; attempt < 2 && noteCost > 0; attempt++ {
		keep := prefixForEscapedBudget(saved, remaining-noteCost)
		keep = trimToRuneBoundary(saved, keep)
		noteCost = escapedLen(truncationNote(len(saved) - keep))
	}
	keep := prefixForEscapedBudget(saved, remaining-noteCost)
	keep = trimToRuneBoundary(saved, keep)

	out := make([]wireContent, len(blocks))
	copy(out, blocks)
	out[longest].Text = saved[:keep] + truncationNote(len(saved)-keep)
	out[longest].Meta = map[string]any{"truncated": true}
	return out
}

// longestTextIndex 返回最长的 text 块下标（没有则 -1）。
func longestTextIndex(blocks []wireContent) int {
	longest := -1
	for index, block := range blocks {
		if block.Type != "text" {
			continue
		}
		if longest < 0 || len(block.Text) > len(blocks[longest].Text) {
			longest = index
		}
	}
	return longest
}

// escapedLen 返回 s 作为 JSON 字符串内容时的字节数（不含两侧引号）。
//
// 与 encoding/json 的默认行为对齐（它会转义 HTML 敏感字符）：
// `"` 与 `\` → 2 字节；控制符、`<`、`>`、`&`、U+2028/U+2029 → 6 字节；
// 非法 UTF-8 字节 → \ufffd 3 字节；其余按原字节数。
func escapedLen(s string) int {
	total := 0
	for index := 0; index < len(s); {
		size, cost := escapedSize(s[index:])
		total += cost
		index += size
	}
	return total
}

// prefixForEscapedBudget 返回满足 escapedLen(s[:keep]) <= budget 的最大 rune 边界
// keep（单遍扫描，不会切出半个 rune）。
func prefixForEscapedBudget(s string, budget int) int {
	if budget <= 0 {
		return 0
	}
	used := 0
	for index := 0; index < len(s); {
		size, cost := escapedSize(s[index:])
		if used+cost > budget {
			return index
		}
		used += cost
		index += size
	}
	return len(s)
}

// escapedSize 返回 s 首个字符的 (原字节数, 转义后字节数)。
func escapedSize(s string) (int, int) {
	first := s[0]
	switch {
	case first == '"' || first == '\\':
		return 1, 2
	case first < 0x20, first == '<', first == '>', first == '&':
		return 1, 6
	case first < utf8.RuneSelf:
		return 1, 1
	}
	char, size := utf8.DecodeRuneInString(s)
	if char == utf8.RuneError && size == 1 {
		return 1, 3 // 非法字节 → �
	}
	if char == 0x2028 || char == 0x2029 {
		return size, 6
	}
	return size, size
}

// truncateContent 截断 blocks 中最长的 text 块，使 text 块总字节数不超过 limit。
//
// 返回的切片可能是同一份数据的拷贝；调用方传入的 blocks 不会被修改。
func truncateContent(blocks []wireContent, limit int) []wireContent {
	total := 0
	longest := -1
	for index, block := range blocks {
		if block.Type != "text" {
			continue
		}
		total += len(block.Text)
		if longest < 0 || len(block.Text) > len(blocks[longest].Text) {
			longest = index
		}
	}
	if longest < 0 || total <= limit {
		return blocks
	}
	text := blocks[longest].Text
	// 其它 text 块先占预算，剩下的才是最长块（含注释）能用的空间。
	budget := limit - (total - len(text))

	drop := total - limit // 初值：至少丢这么多
	keep, note := truncationPoint(text, budget, drop)
	if budget <= 0 {
		// 上限比其它块还小：只能把最长块压成注释本身。
		keep, note = 0, truncationNote(len(text))
	}

	out := make([]wireContent, len(blocks))
	copy(out, blocks)
	out[longest].Text = text[:keep] + note
	out[longest].Meta = map[string]any{"truncated": true}
	return out
}

// truncationPoint 求解"保留多少字节 + 注释写多少"，使 keep+len(note) <= budget。
//
// 注释里要写丢弃的字节数，而注释本身也占字节，所以是个不动点：迭代 4 次足够
// （每次只会把 drop 增大到刚好够，数字位数变化最多再修正一次）。
func truncationPoint(text string, budget, drop int) (int, string) {
	if drop < 0 {
		drop = 0
	}
	keep := len(text) - drop
	var note string
	for attempt := 0; attempt < 4; attempt++ {
		if keep < 0 {
			keep = 0
		}
		keep = trimToRuneBoundary(text, keep)
		note = truncationNote(len(text) - keep)
		if keep+len(note) <= budget || keep == 0 {
			break
		}
		keep -= keep + len(note) - budget
	}
	if keep < 0 {
		keep = 0
	}
	keep = trimToRuneBoundary(text, keep)
	if note == "" {
		note = truncationNote(len(text) - keep)
	}
	return keep, note
}

// truncationNote 渲染截断注释。前导换行让"原内容的最后一行"和注释分开。
func truncationNote(omitted int) string {
	return fmt.Sprintf("\n…[truncated: %d bytes omitted]", omitted)
}

// trimToRuneBoundary 把 keep 回退到 UTF-8 rune 边界（不会超过 len(text)）。
func trimToRuneBoundary(text string, keep int) int {
	if keep <= 0 {
		return 0
	}
	if keep >= len(text) {
		return len(text)
	}
	for keep > 0 && !utf8.RuneStart(text[keep]) {
		keep--
	}
	return keep
}
