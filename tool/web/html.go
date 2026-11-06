package web

import (
	"html"
	"strings"
	"unicode"
)

// HTMLToText converts an HTML document to readable plain text:
//
//   - script, style, noscript, template, svg and math contents are dropped;
//   - tags are removed, block-level elements become line breaks and inline
//     whitespace runs collapse to one space;
//   - comments disappear, entities are decoded (stdlib html.UnescapeString),
//     non-breaking spaces become normal spaces;
//   - anchor text is kept (links stay as their text; hrefs are not expanded);
//   - malformed markup never panics: an unterminated tag at EOF is kept as text.
//
// It is a small stdlib-only extractor, not an HTML5 parser: it is meant for
// turning a page into model-readable text, not for reconstructing a document.
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
		// Comment.
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
			// Malformed: keep the rest as text rather than dropping content.
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
			continue // doctype / processing instruction
		}
		closing := strings.HasPrefix(trimmed, "/")
		name := strings.TrimPrefix(trimmed, "/")
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

// indexClosingTag finds the closing tag </name (case-insensitive, with a tag
// name boundary) at or after from.
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

// skipContentTags are elements whose content is never shown as text.
var skipContentTags = map[string]bool{
	"script":   true,
	"style":    true,
	"noscript": true,
	"template": true,
	"svg":      true,
	"math":     true,
}

// blockTags produce a line break around their content.
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
