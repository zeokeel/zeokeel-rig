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
//   - 全部转小写、丢弃空项（因此空串返回 nil）。
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
		buffer = buffer[:0]
		if isASCII(part) {
			// ASCII 快路径：直接逐字节小写，避免 DecodeRune/AppendRune 的开销。
			for _, char := range part {
				if 'A' <= char && char <= 'Z' {
					char += 'a' - 'A'
				}
				buffer = append(buffer, char)
			}
		} else {
			for len(part) > 0 {
				char, size := utf8.DecodeRune(part)
				part = part[size:]
				buffer = utf8.AppendRune(buffer, unicode.ToLower(char))
			}
		}
		if len(buffer) > 0 {
			tokens = append(tokens, string(buffer))
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
