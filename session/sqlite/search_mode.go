package sqlite

// 检索模式与 CJK 分词（见 search.go 的 SearchMessages）。
//
// 这里放三样东西：
//  1. `SearchMode`：substring（零值，既有行为）与 fts5（trigram + bm25）；
//  2. CJK 的 unigram/bigram 切分：trigram tokenizer 只索引连续 3 个字符的窗口，
//     而中文/日文/韩文不用空格分词，2 字词（"磁盘"）自身撑不满一个窗口，于是
//     fts 模式对最常见的 2 字查询零命中——必须在**写入时**另存一份可检索的词表；
//  3. FTS5 的 MATCH 表达式：把用户输入按空白切词项、每项翻译成**字面量短语**
//     （或 CJK 词表），词项之间 AND；纯 ASCII 查询保持与 substring 相同的命中
//     集合（见 ftsMatchExpression 的边界说明）。
//
// CJK 的切分规则**复制自** `tools/search/tokenize.go` 的 cjkTokens（unigram +
// 相邻 bigram，全部小写）。不能 import 那个包：`session/sqlite` 是独立子模块，
// 依赖主模块的实现会把"子模块能单独 go get"这条承诺打破（见 docs/sqlite.md）。

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// SearchMode 选择一次检索走哪条索引。
//
// 零值是 SearchSubstring：既有调用方（`SearchQuery{Text: ...}`）语义不变。
type SearchMode int

const (
	// SearchSubstring 用解码列上的 `LIKE '%x%'` 做子串匹配，任意长度都能命中
	// （包括单字、单字符），排序用"命中次数 + 出现位置"的轻量打分。
	SearchSubstring SearchMode = iota
	// SearchFTS 用 FTS5（trigram tokenizer + CJK 辅助列）做匹配，排序用 `bm25`。
	// 当前 SQLite 构建没有 FTS5 时**自动降级**为 SearchSubstring（见 FTS5Available）。
	SearchFTS
)

// String 让日志与错误信息读起来是人话。
func (m SearchMode) String() string {
	switch m {
	case SearchFTS:
		return "fts5"
	case SearchSubstring:
		return "substring"
	default:
		return fmt.Sprintf("unknown(%d)", int(m))
	}
}

// searchCJKMarker 是 CJK 辅助列的哨兵字符。
//
// trigram tokenizer 把文本切成连续 3 个字符的窗口；"磁盘" 只有 2 个字符，直接写进
// 索引等于没写（查询 "磁盘" 得到 0 条，而且不报错）。把每个词包成 `#磁盘#` 之后
// 它占满完整的窗口，"#磁盘#" 与 "#盘#"（单字）都能单独命中。
//
// 辅助列只存由本函数生成的词，不存原文，所以哨兵不会与用户文本混淆。
const searchCJKMarker = "#"

// isCJKRune 报告 rune 是否属于不用空格分词的书写体系（汉字 / 假名 / 谚文）。
//
// 与 tools/search/tokenize.go 的 isCJKRune 同源：拉丁语系仍按整词处理，这里不碰。
func isCJKRune(char rune) bool {
	return unicode.Is(unicode.Han, char) ||
		unicode.Is(unicode.Hiragana, char) ||
		unicode.Is(unicode.Katakana, char) ||
		unicode.Is(unicode.Hangul, char)
}

// cjkRuns 把文本里的每一个连续 CJK 段拆出来（段之间被非 CJK 字符隔开）。
func cjkRuns(text string) [][]rune {
	runs := make([][]rune, 0, 2)
	current := make([]rune, 0, 8)
	for _, char := range text {
		if isCJKRune(char) {
			current = append(current, char)
			continue
		}
		if len(current) > 0 {
			runs = append(runs, current)
			current = make([]rune, 0, 8)
		}
	}
	if len(current) > 0 {
		runs = append(runs, current)
	}
	return runs
}

// searchCJKFor 生成一条消息的 CJK 辅助文本（写入 `rig_messages.search_cjk`）。
//
// 每个连续 CJK 段展开成 unigram + 相邻 bigram（顺序固定：先单字，再二元组），
// 每个词用哨兵包住、直接拼接（不加分隔符，双字词因此能作为**相邻**窗口匹配到）：
//
//	"磁盘上" → #磁##盘##上##磁盘##盘上#
//
// 非 CJK 文本不进辅助列：它由 search_text 的 trigram 覆盖，重复存只会让 bm25 里
// 同一份内容被算两次。
func searchCJKFor(text string) string {
	var builder strings.Builder
	for _, run := range cjkRuns(text) {
		for _, char := range run {
			builder.WriteString(searchCJKMarker)
			builder.WriteRune(unicode.ToLower(char))
			builder.WriteString(searchCJKMarker)
		}
		for index := 0; index+1 < len(run); index++ {
			builder.WriteString(searchCJKMarker)
			builder.WriteRune(unicode.ToLower(run[index]))
			builder.WriteRune(unicode.ToLower(run[index+1]))
			builder.WriteString(searchCJKMarker)
		}
	}
	return builder.String()
}

// searchCJKQuery 把查询里的 CJK 段展开成**要匹配的词**（已包哨兵）。
//
// 单字段只给 unigram；两字及以上给 bigram——与写入侧同源，所以查询 "磁盘" 与
// 文档里的 "我们把文件写到磁盘上" 共享 bigram `磁盘`。
func searchCJKQuery(text string) []string {
	var terms []string
	seen := make(map[string]bool, 4)
	add := func(token string) {
		term := searchCJKMarker + token + searchCJKMarker
		if seen[term] {
			return
		}
		seen[term] = true
		terms = append(terms, term)
	}
	for _, run := range cjkRuns(text) {
		lowered := make([]rune, 0, len(run))
		for _, char := range run {
			lowered = append(lowered, unicode.ToLower(char))
		}
		if len(lowered) == 1 {
			add(string(lowered))
			continue
		}
		for index := 0; index+1 < len(lowered); index++ {
			add(string(lowered[index]) + string(lowered[index+1]))
		}
	}
	return terms
}

// quoteFTS 把一段文本变成 FTS5 的双引号短语，内部的 `"` 按 FTS5 规则双写转义。
func quoteFTS(text string) string {
	return `"` + strings.ReplaceAll(text, `"`, `""`) + `"`
}

// ftsMatchExpression 组装 FTS5 的 MATCH 表达式；返回空串表示"fts 表达不了这段
// 查询"，调用方据此降级为 substring。
//
// # 每一种形状都只产生字面量短语
//
// 进 MATCH 的每一段都被 quoteFTS 包成双引号短语（内部 `"` 双写成 `""`），所以
// `-`、`#`、`.`、`/`、`(`、`:` 都不再是语法：`-磁盘` 是"一个减号加磁盘"、`#磁盘#`
// 是"一个带井号的字面串"，而不是"排除"或"哨兵"。**查询侧不剥任何字符**——剥了
// 会改变字面量语义（`#磁盘#` 会退化成 `磁盘`，反而多召回）。哨兵 `#` 只属于写入
// 侧的 `search_cjk`，见 ftsTermExpression：只有"整段都是 CJK"的词项才会展开词表，
// 带 `#` 的词项永远走短语。
//
// # 词项之间 AND
//
// 查询先按空白切成词项，词项之间是 **AND**（`(t1) AND (t2)`）：每个词项都必须
// 出现。旧实现把 CJK 词项与整串短语用 OR 连起来，于是 `error 磁盘` 命中了不含
// `error` 的消息（查询里的 ASCII 部分被整个丢掉）。
//
// # 边界（与 substring 的差异）
//
//   - **纯 ASCII（不含 CJK）**：整串一个短语。trigram 的短语匹配就是**子串**，
//     因此这种查询的命中集合与 substring 模式**完全一致**；不足 3 个字符撑不满
//     trigram 窗口，返回空串降级。
//   - **含 CJK 且只有一个词项**：同样按子串语义（纯 CJK 段里 ≥3 字走 search_text
//     短语，1~2 字走 search_cjk 词表）。
//   - **含 CJK 且有多个词项**：每个词项各自成立即可，**不要求它们在原文里相邻**。
//     这是与 substring 的有意差异：`error 磁盘` 在 substring 里要求连续的
//     "error 磁盘"，在 fts 里只要求两个词都出现——召回更大，排序交给 bm25。
//     含 CJK 的多词项查询无法用"整串短语"表达（2 字 CJK 词撑不满 trigram 窗口），
//     所以这里选了"全都要有"的放宽，而不是旧实现的"有任一个就行"。
func ftsMatchExpression(text string) string {
	cleaned := strings.TrimSpace(text)
	if cleaned == "" {
		return ""
	}
	if len(cjkRuns(cleaned)) == 0 {
		// 纯 ASCII / 标点：整串短语 == 子串语义，命中集合与 substring 完全一致。
		if utf8.RuneCountInString(cleaned) < 3 {
			return ""
		}
		return `{search_text} : ` + quoteFTS(cleaned)
	}
	terms := strings.Fields(cleaned)
	expressions := make([]string, 0, len(terms))
	for _, term := range terms {
		expression := ftsTermExpression(term)
		if expression == "" {
			// 有一个词项 fts 表达不了（例如 1~2 个字符的混排）：整条查询退回
			// substring。fts 模式不能比 substring 少命中。
			return ""
		}
		expressions = append(expressions, expression)
	}
	if len(expressions) == 0 {
		return ""
	}
	if len(expressions) == 1 {
		return expressions[0]
	}
	return "(" + strings.Join(expressions, ") AND (") + ")"
}

// ftsTermExpression 把一个词项翻译成"它成立"的 fts 表达。
//
// 三种形状：
//   - 整段都是 CJK、1~2 字：走 `search_cjk` 的 unigram/bigram 词表。2 字词撑不满
//     trigram 窗口，词表是唯一能命中它的路径；词表里的词按"相邻"生成，所以命中
//     与"子串出现"等价。
//   - 其它（≥3 个字符，含标点/ASCII 的混排、或 ≥3 字的纯 CJK）：整串短语走
//     `search_text`，即子串语义。这里**不**额外 OR 词表：`磁盘上我们` 只有整串
//     出现才算命中，不会被拆成"任一二元组命中"放大。
//   - 含 CJK 但不足 3 个字符、又不是纯 CJK（如 `#磁`）：没有忠实的 fts 表达，
//     返回空串让整条查询降级。
func ftsTermExpression(term string) string {
	// 词表只接"整段 CJK 且 1~2 字"；其它形状（≥3 字符的纯 CJK、或混了标点/ASCII）
	// 一律整串短语——短语是子串语义，标点按字面量。
	if runes := utf8.RuneCountInString(term); !onlyCJKRunes(term) || runes >= 3 {
		if runes < 3 {
			return ""
		}
		return `{search_text} : ` + quoteFTS(term)
	}
	tokens := searchCJKQuery(term)
	if len(tokens) == 0 {
		return ""
	}
	parts := make([]string, 0, len(tokens))
	for _, token := range tokens {
		parts = append(parts, `{search_cjk} : `+quoteFTS(token))
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return "(" + strings.Join(parts, " OR ") + ")"
}

// onlyCJKRunes 报告文本里每一个 rune 都是 CJK（空串返回 false）。
//
// 用来判断"能不能整段交给 CJK 词表"：混了 ASCII/标点的词项交给词表会丢字符，
// 那是 `磁盘"`、`#磁盘#` 被放大的根源。
func onlyCJKRunes(text string) bool {
	if text == "" {
		return false
	}
	for _, char := range text {
		if !isCJKRune(char) {
			return false
		}
	}
	return true
}
