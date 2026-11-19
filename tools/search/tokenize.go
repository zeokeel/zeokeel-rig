package search

// 本文件是 BM25 与 mentions 共用的确定性分词器：按非字母数字拆分、camelCase 与
// snake_case 再拆、全小写。导出一份是为了让其它包（以及测试）与索引侧对齐同一套
// 规则——分词一旦不一致，检索结果就会变得无法解释。
//
// 实现是单遍字节扫描（索引侧直接吃 []byte，省掉整块复制）：边扫边判定 camel
// 边界（只在大写/数字处做一次前瞻），小写化对 ASCII 走字节快路径。

import (
	"unicode"
	"unicode/utf8"
)

// Tokenize 把文本切成词项。
//
// 规则（确定性、与大小写无关）：
//   - 按非字母数字字符拆分（空白、标点、下划线、连字符、路径分隔符都是分隔符）；
//   - 在每个字母数字串内部按 camelCase / PascalCase 边界再拆，例如 "SearchIndex"
//     → search, index；缩写按"最后一个大写字母"切开，例如 "HTTPServer" →
//     http, server；数字粘在前面的词里，例如 "http2"；
//   - **CJK（汉字/假名/谚文）连续段生成 unigram + 相邻 bigram**（见下）；
//   - 全部转小写、丢弃空项（因此空串返回 nil）。
//
// # 为什么 CJK 要 unigram + bigram
//
// CJK 不用空格分词。旧实现把一段连续汉字当成**一个**词项，于是分词边界完全由文本
// 决定：查询 "门禁" 与文档里的 "审批门禁" 是两个不同的词项，检索**系统性零命中**，
// 而且静默——用户以为没有这段内容（审计 P1）。
//
// 现在的折中（不引入词典/分词器，保持确定性）：
//   - 每个 CJK 字单独成词项（unigram）→ 单字查询、生僻组合仍能命中；
//   - 每对相邻 CJK 字成词项（bigram）→ 提供"近似短语"的精度，避免单字把
//     无关文档全拉进来；
//   - 查询侧与索引侧**同一个函数**，所以查询 "审批门禁" 与文档 "实现审批门禁
//     组合器" 共享 bigram 审批/批门/门禁。
//
// 拉丁/数字/标识符规则完全不变（英文的行为与排序不受影响）。
func Tokenize(s string) []string {
	return tokenizeBytes([]byte(s))
}

// tokenizeBytes 是 Tokenize 的字节版本（索引路径直接复用，避免复制整份文件）。
func tokenizeBytes(content []byte) []string {
	var tokens []string
	var buffer []byte
	appendPart := func(part []byte) {
		if len(part) == 0 {
			return
		}
		// ASCII 快路径：整段都是 ASCII 时不可能是 CJK，行为与历史实现完全一致。
		if isASCII(part) {
			buffer = buffer[:0]
			for _, char := range part {
				if 'A' <= char && char <= 'Z' {
					char += 'a' - 'A'
				}
				buffer = append(buffer, char)
			}
			if len(buffer) > 0 {
				tokens = append(tokens, string(buffer))
			}
			return
		}
		// 含非 ASCII：按"CJK 段 / 非 CJK 段"切开，两类各自处理。
		// 例如 "gate审批" 会被拆成 "gate" 与 "审批"，两种分词规则互不污染。
		for _, segment := range splitCJKSegments(part) {
			buffer = buffer[:0]
			if segment.cjk {
				tokens = append(tokens, cjkTokens(segment.runes)...)
				continue
			}
			for _, char := range segment.runes {
				buffer = utf8.AppendRune(buffer, unicode.ToLower(char))
			}
			if len(buffer) > 0 {
				tokens = append(tokens, string(buffer))
			}
		}
	}
	// wordStart 是当前 camel 词的起始字节偏移；negative 表示还没进入词。
	wordStart := -1
	previous := rune(-1)
	for offset := 0; offset < len(content); {
		char, size := decodeRune(content, offset)
		if !isWordRune(char) {
			if wordStart >= 0 {
				appendPart(content[wordStart:offset])
				wordStart = -1
				previous = -1
			}
			offset += size
			continue
		}
		if wordStart < 0 {
			wordStart = offset
			previous = char
			offset += size
			continue
		}
		if isCamelBoundary(content, offset, size, previous, char) {
			appendPart(content[wordStart:offset])
			wordStart = offset
		}
		previous = char
		offset += size
	}
	if wordStart >= 0 {
		appendPart(content[wordStart:])
	}
	return tokens
}

// isWordRune 报告 rune 是否属于词项字符（字母或数字）。
//
// ASCII 走位运算快路径：索引一份仓库时这个判定会被调用上千万次，unicode 函数
// 的区间表查表开销在这里是可见的（下划线仍然算分隔符）。
func isWordRune(char rune) bool {
	if char < utf8.RuneSelf {
		return 'a' <= char && char <= 'z' || 'A' <= char && char <= 'Z' || '0' <= char && char <= '9'
	}
	return unicode.IsLetter(char) || unicode.IsDigit(char)
}

// decodeRune 解码 offset 处的 rune；ASCII 走快路径（绝大多数字节都是 ASCII）。
func decodeRune(content []byte, offset int) (rune, int) {
	if char := content[offset]; char < utf8.RuneSelf {
		return rune(char), 1
	}
	return utf8.DecodeRune(content[offset:])
}

// isASCII 报告字节串是否全是 ASCII。
func isASCII(value []byte) bool {
	for _, char := range value {
		if char >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// isCamelBoundary 报告当前位置是否开启一个新的 camel 词。
//
// offset 是当前字符在 content 里的偏移，previous 是同一个词里前一个字符。
// 只有缩写（大写后跟大写）与数字后跟大写这两种情况需要向前看一个字符。
func isCamelBoundary(content []byte, offset, size int, previous, current rune) bool {
	switch {
	case unicode.IsLower(previous) && unicode.IsUpper(current):
		// searchIndex → search | Index
		return true
	case unicode.IsUpper(previous) && unicode.IsUpper(current):
		// HTTPServer → HTTP | Server（下一个字符必须还在词内且为小写）
		next, _ := utf8.DecodeRune(content[offset+size:])
		return isWordRune(next) && unicode.IsLower(next)
	case unicode.IsDigit(previous) && unicode.IsUpper(current):
		// HTTP2Token → HTTP2 | Token；词尾的大写也单独成词（HTTP2T → http2|t）
		next, _ := utf8.DecodeRune(content[offset+size:])
		return !isWordRune(next) || unicode.IsLower(next)
	}
	return false
}

// cjkSegment 是一段"同类"字符：cjk=true 表示全是 CJK 词字符。
type cjkSegment struct {
	runes []rune
	cjk   bool
}

// splitCJKSegments 把一段词字符切成"CJK / 非 CJK"交替的子段。
//
// 输入已经过 camelCase 拆分，所以这里只关心字符类别；CJK 与非 CJK 直接相邻时
// （"gate审批"、"审批2"）也必须切开，否则整段会退化成历史实现里的那个"巨型词项"。
func splitCJKSegments(part []byte) []cjkSegment {
	segments := make([]cjkSegment, 0, 2)
	for len(part) > 0 {
		char, size := utf8.DecodeRune(part)
		part = part[size:]
		isCJK := isCJKRune(char)
		if count := len(segments); count > 0 && segments[count-1].cjk == isCJK {
			segments[count-1].runes = append(segments[count-1].runes, char)
			continue
		}
		segments = append(segments, cjkSegment{runes: []rune{char}, cjk: isCJK})
	}
	return segments
}

// cjkTokens 把一个 CJK 段展开成 unigram + 相邻 bigram（顺序固定：先单字，再二元组）。
//
// 单字段只产出一个 unigram；两字及以上才产出 bigram。这样：
//
//	"门"       → 门
//	"审批门禁" → 审 批 门 禁 审批 批门 门禁
func cjkTokens(runes []rune) []string {
	if len(runes) == 0 {
		return nil
	}
	out := make([]string, 0, 2*len(runes))
	for _, char := range runes {
		out = append(out, string(unicode.ToLower(char)))
	}
	for index := 0; index+1 < len(runes); index++ {
		out = append(out, string(unicode.ToLower(runes[index]))+string(unicode.ToLower(runes[index+1])))
	}
	return out
}

// isCJKRune 报告 rune 是否属于需要 bigram 的分词体系（汉字 / 假名 / 谚文）。
//
// 只覆盖"不用空格分词"的书写系统：拉丁语系与其它字母文字仍按整词处理，行为不变。
func isCJKRune(char rune) bool {
	return unicode.Is(unicode.Han, char) ||
		unicode.Is(unicode.Hiragana, char) ||
		unicode.Is(unicode.Katakana, char) ||
		unicode.Is(unicode.Hangul, char)
}
