package web

import (
	"html"
	"strings"
	"unicode"
)

// HTMLToText 把 HTML 文档转换为可读纯文本：
//
//   - script、style、noscript、template、svg 与 math 的内容被丢弃；
//   - 标签被移除，块级元素转换为换行，行内连续空白折叠为一个空格；
//   - 注释消失，实体被解码（stdlib html.UnescapeString），不间断空格转为普通空格；
//   - 保留锚文本（链接保留为文本；href 不展开）；
//   - 畸形标记不会 panic：EOF 处未闭合的标签按文本保留。
//
// 它是一个仅依赖标准库的小型提取器，不是 HTML5 解析器：目标是让页面变成模型可读的文本，
// 而不是重建文档。
func HTMLToText(source string) string {
	if source == "" {
		return ""
	}
	var output []rune
	pendingSpace := false
	pendingBreak := false

	flush := func() {
		for len(output) > 0 {
			last := output[len(output)-1]
			if last == ' ' || last == '\n' || last == '\t' || last == '\r' {
				output = output[:len(output)-1]
				continue
			}
			break
		}
		switch {
		case pendingBreak && len(output) > 0:
			output = append(output, '\n')
		case pendingSpace && len(output) > 0:
			output = append(output, ' ')
		}
		pendingSpace = false
		pendingBreak = false
	}
	appendText := func(text string) {
		for _, r := range text {
			if unicode.IsSpace(r) {
				pendingSpace = true
				continue
			}
			flush()
			output = append(output, r)
		}
	}

	for index := 0; index < len(source); {
		if source[index] != '<' {
			next := strings.IndexByte(source[index:], '<')
			if next < 0 {
				appendText(source[index:])
				break
			}
			appendText(source[index : index+next])
			index += next
			continue
		}
		// 注释。
		if strings.HasPrefix(source[index:], "<!--") {
			if end := strings.Index(source[index+4:], "-->"); end >= 0 {
				index += 4 + end + 3
			} else {
				break
			}
			continue
		}
		end := strings.IndexByte(source[index:], '>')
		if end < 0 {
			// 畸形：把剩余内容当作文本保留，而不是丢弃内容。
			appendText(source[index:])
			break
		}
		rawTag := source[index+1 : index+end]
		index += end + 1
		trimmed := strings.TrimSpace(rawTag)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "!") || strings.HasPrefix(trimmed, "?") {
			continue // doctype / 处理指令
		}
		name, closing := strings.CutPrefix(trimmed, "/")
		if cut := strings.IndexAny(name, " \t\r\n/"); cut >= 0 {
			name = name[:cut]
		}
		name = strings.ToLower(name)
		if skipContentTags[name] {
			if !closing {
				closeIndex := indexClosingTag(source, index, name)
				if closeIndex < 0 {
					break
				}
				endTag := strings.IndexByte(source[closeIndex:], '>')
				if endTag < 0 {
					break
				}
				index = closeIndex + endTag + 1
			}
			continue
		}
		if blockTags[name] {
			pendingSpace = false
			pendingBreak = true
		}
	}
	flush()

	text := html.UnescapeString(string(output))
	if text == "" {
		return ""
	}
	text = strings.ReplaceAll(text, "\u00a0", " ")
	lines := strings.Split(text, "\n")
	cleaned := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.Join(strings.Fields(line), " ")
		if line == "" {
			if len(cleaned) > 0 && cleaned[len(cleaned)-1] != "" {
				cleaned = append(cleaned, "")
			}
			continue
		}
		cleaned = append(cleaned, line)
	}
	for len(cleaned) > 0 && cleaned[len(cleaned)-1] == "" {
		cleaned = cleaned[:len(cleaned)-1]
	}
	return strings.Join(cleaned, "\n")
}

// indexClosingTag 在 from 之后查找结束标签 </name（大小写不敏感，并要求标签名边界）。
func indexClosingTag(source string, from int, name string) int {
	needle := "</" + name
	for index := from; index+len(needle) <= len(source); index++ {
		if source[index] != '<' {
			continue
		}
		if !strings.EqualFold(source[index:index+len(needle)], needle) {
			continue
		}
		next := index + len(needle)
		if next == len(source) {
			return index
		}
		switch source[next] {
		case '>', ' ', '\t', '\r', '\n', '/':
			return index
		}
	}
	return -1
}

// skipContentTags 是内容永不作为文本显示的元素。
var skipContentTags = map[string]bool{
	"script":   true,
	"style":    true,
	"noscript": true,
	"template": true,
	"svg":      true,
	"math":     true,
}

// blockTags 会在其内容前后产生换行。
var blockTags = map[string]bool{
	"address": true, "article": true, "aside": true, "blockquote": true,
	"br": true, "canvas": true, "dd": true, "details": true, "div": true,
	"dl": true, "dt": true, "fieldset": true, "figcaption": true,
	"figure": true, "footer": true, "form": true, "h1": true, "h2": true,
	"h3": true, "h4": true, "h5": true, "h6": true, "header": true,
	"hr": true, "li": true, "main": true, "nav": true, "ol": true,
	"p": true, "pre": true, "section": true, "summary": true, "table": true,
	"tbody": true, "td": true, "tfoot": true, "th": true, "thead": true,
	"tr": true, "ul": true, "video": true, "audio": true,
}
