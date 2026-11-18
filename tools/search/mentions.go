package search

// 本文件是 @提及 的解析：从一段自然语言里抽出
// `@path`、`@"path with space"`、`@path:12`、`@path:12-20`。
//
// 解析是纯文本的：不做任何文件系统访问，因此可以在任意宿主上单测。存在性由调用方
// （mentions 模式）标注到 Mention.Missing 上。

import (
	"strconv"
	"strings"
	"unicode"
)

// Mention 是一条解析出来的 @提及。
type Mention struct {
	// Path 是提及的路径文本（引号已去掉、尾部标点已清理，保留原样大小写与分隔符）。
	Path string
	// Line 是起始行号（1 起算）；0 表示没有指定行。
	Line int
	// EndLine 是结束行号；与 Line 相等表示单行；0 表示没有指定行。
	EndLine int
	// Missing 由调用方在检查存在性后填写：true 表示这个提及在根目录里找不到。
	Missing bool
}

// ParseMentions 解析文本里的 @提及，按出现顺序返回（相同「路径 + 行范围」只保留
// 第一次出现）。
//
// 识别的语法：
//
//	@path            普通路径（遇到空白或 ,;)]}>" 等终止符结束）
//	@"path with space"  引号包裹（可跟在引号后写 :12 或 :12-20）
//	@path:12         单行
//	@path:12-20      行范围（逆序会规范成升序）
//
// 只有位于文本开头，或前一个字符是空白/左括号/引号/逗号/分号/冒号/等号的 '@' 才算
// 提及，因此 `user@example.com` 不会被误判。
func ParseMentions(text string) []Mention {
	runes := []rune(text)
	mentions := make([]Mention, 0, 4)
	seen := make(map[string]struct{}, 4)
	appendMention := func(mention Mention) {
		if mention.Path == "" {
			return
		}
		key := mention.Path + "|" + strconv.Itoa(mention.Line) + "|" + strconv.Itoa(mention.EndLine)
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		mentions = append(mentions, mention)
	}
	for index := 0; index < len(runes); index++ {
		if runes[index] != '@' {
			continue
		}
		if index > 0 && !isMentionBoundary(runes[index-1]) {
			continue
		}
		cursor := index + 1
		if cursor >= len(runes) {
			break
		}
		if runes[cursor] == '"' {
			closing := -1
			for scan := cursor + 1; scan < len(runes); scan++ {
				if runes[scan] == '"' {
					closing = scan
					break
				}
			}
			if closing < 0 {
				// 未闭合引号：整段都不构成提及。
				break
			}
			rawPath := string(runes[cursor+1 : closing])
			line, endLine, _ := parseMentionLineSuffix(runes[closing+1:])
			appendMention(Mention{Path: strings.TrimSpace(rawPath), Line: line, EndLine: endLine})
			index = closing
			continue
		}
		scan := cursor
		for scan < len(runes) && !isMentionTerminator(runes[scan]) {
			scan++
		}
		raw := trimMentionPunctuation(string(runes[cursor:scan]))
		if raw == "" {
			continue
		}
		rawPath, line, endLine := splitMentionPathLine(raw)
		appendMention(Mention{Path: rawPath, Line: line, EndLine: endLine})
		index = scan - 1
	}
	return mentions
}

// isMentionBoundary 报告某个字符是否允许出现在 '@' 之前。
func isMentionBoundary(previous rune) bool {
	if unicode.IsSpace(previous) {
		return true
	}
	switch previous {
	case '(', '[', '{', '<', '"', '\'', '`', ',', ';', ':', '=', '，', '。', '、', '：':
		return true
	}
	return false
}

// isMentionTerminator 报告某个字符是否结束一个裸路径。
//
// 注意 ':'、'.'、'/'、'\'、'@' 都不是终止符：它们要么是行号分隔符，要么是文件名
// 的一部分（`@src/a.go:12`、`@C:\a\b.txt`）。
func isMentionTerminator(char rune) bool {
	if unicode.IsSpace(char) {
		return true
	}
	switch char {
	case ',', ';', ')', ']', '}', '>', '"', '\'', '`', '，', '。', '、', '！', '？', '（', '）':
		return true
	}
	return false
}

// trimMentionPunctuation 去掉裸路径尾部的句读（`@a.go,` → `a.go`）。
func trimMentionPunctuation(raw string) string {
	trimmed := strings.TrimRight(raw, ".,;:!?'\"")
	trimmed = strings.TrimRight(trimmed, "。，、！？；：")
	return trimmed
}

// splitMentionPathLine 从裸路径里切出可选的行号/行范围。
//
// 只有当最后一个 ':' 之后是 `数字` 或 `数字-数字`（且行号 >= 1）时才切分，因此
// `@C:\work\a.txt` 的盘符冒号不会被误当成行号。
func splitMentionPathLine(raw string) (string, int, int) {
	colon := strings.LastIndex(raw, ":")
	if colon < 0 || colon == len(raw)-1 {
		return raw, 0, 0
	}
	suffix := raw[colon+1:]
	line, endLine, ok := parseMentionLineRange(suffix)
	if !ok {
		return raw, 0, 0
	}
	return raw[:colon], line, endLine
}

// parseMentionLineSuffix 解析引号提及之后紧跟的 `:12` / `:12-20`。
func parseMentionLineSuffix(rest []rune) (int, int, bool) {
	if len(rest) == 0 || rest[0] != ':' {
		return 0, 0, false
	}
	end := 1
	for end < len(rest) && !unicode.IsSpace(rest[end]) {
		end++
	}
	suffix := string(rest[1:end])
	line, endLine, ok := parseMentionLineRange(strings.TrimRight(suffix, ".,;!?"))
	if !ok {
		return 0, 0, false
	}
	return line, endLine, true
}

// parseMentionLineRange 解析 `12` 或 `12-20`（行号从 1 开始，逆序会规范成升序）。
func parseMentionLineRange(suffix string) (int, int, bool) {
	startText, endText, isRange := strings.Cut(suffix, "-")
	start, err := strconv.Atoi(strings.TrimSpace(startText))
	if err != nil || start < 1 {
		return 0, 0, false
	}
	if !isRange {
		return start, start, true
	}
	end, err := strconv.Atoi(strings.TrimSpace(endText))
	if err != nil || end < 1 {
		return 0, 0, false
	}
	if end < start {
		start, end = end, start
	}
	return start, end, true
}
