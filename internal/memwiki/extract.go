package memwiki

import (
	"strings"
	"time"
)

// 本文件处理页面文件的序列化/反序列化（front-matter + Markdown 正文）。
//
// front-matter 只用 `key: value` 单行形式（不引 YAML 库），因为它只需要承载
// title / summary / updated 三个字段；值里的换行会在写入前被折叠成空格。

const (
	frontMatterFence = "---"
	fieldTitle       = "title"
	fieldSummary     = "summary"
	fieldUpdated     = "updated"
	updatedLayout    = time.RFC3339
)

// renderPage 把一页渲染成文件内容（front-matter + 正文）。
func renderPage(page Page) []byte {
	title := normalizeTitle(page.Name, page.Title)
	summary := collapseWhitespace(page.Summary)
	updated := page.Updated.UTC().Format(updatedLayout)

	var builder strings.Builder
	builder.WriteString(frontMatterFence)
	builder.WriteString("\n")
	builder.WriteString(fieldTitle + ": " + title + "\n")
	builder.WriteString(fieldSummary + ": " + summary + "\n")
	builder.WriteString(fieldUpdated + ": " + updated + "\n")
	builder.WriteString(frontMatterFence)
	builder.WriteString("\n\n")
	builder.WriteString(strings.TrimRight(page.Content, "\n"))
	builder.WriteString("\n")
	return []byte(builder.String())
}

// parsePage 解析文件内容。front-matter 缺失或损坏时**不报错**：把整份文件当正文，
// 标题回退成 name——这样一页坏文件仍然能被 read/delete 修掉，而不是从 index 里消失。
func parsePage(name string, raw []byte) Page {
	page := Page{Name: name}
	text := string(raw)
	rest, fields, ok := splitFrontMatter(text)
	if !ok {
		page.Title = name
		page.Content = strings.TrimRight(text, "\n")
		page.Summary = ""
		return page
	}
	page.Title = normalizeTitle(name, fields[fieldTitle])
	page.Summary = collapseWhitespace(fields[fieldSummary])
	if updated := strings.TrimSpace(fields[fieldUpdated]); updated != "" {
		if parsed, err := time.Parse(updatedLayout, updated); err == nil {
			page.Updated = parsed.UTC()
		}
	}
	page.Content = strings.TrimRight(strings.TrimLeft(rest, "\r\n"), "\n")
	return page
}

// splitFrontMatter 把文本拆成 (正文, 字段表, 是否有合法 front-matter)。
func splitFrontMatter(text string) (string, map[string]string, bool) {
	trimmed := strings.TrimLeft(text, "\r\n")
	if !strings.HasPrefix(trimmed, frontMatterFence+"\n") && !strings.HasPrefix(trimmed, frontMatterFence+"\r\n") {
		return text, nil, false
	}
	// 越过第一行（fence）。
	newline := strings.IndexByte(trimmed, '\n')
	if newline < 0 {
		return text, nil, false
	}
	body := trimmed[newline+1:]
	fields := make(map[string]string, 3)
	for {
		lineEnd := strings.IndexByte(body, '\n')
		if lineEnd < 0 {
			return text, nil, false // 没有闭合 fence：当作没有 front-matter
		}
		line := strings.TrimRight(body[:lineEnd], "\r")
		body = body[lineEnd+1:]
		if line == frontMatterFence {
			return body, fields, true
		}
		colon := strings.IndexByte(line, ':')
		if colon <= 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(line[:colon]))
		value := strings.TrimSpace(line[colon+1:])
		fields[key] = value
		if len(fields) > 32 {
			return text, nil, false // 防御：畸形文件
		}
	}
}

// appendLogLine 把一行（或多行）追加到正文的 Log 段。
//
// 规则：
//   - 正文里已有 `## Log` → 追加到该段末尾（下一处 `## ` 之前）；
//   - 没有 → 在文末新建 `## Log` 段；
//   - text 的多行按缩进续行写入，保持 bullet 结构。
func appendLogLine(content, text string) string {
	entry := formatLogEntry(text)
	if entry == "" {
		return content
	}
	trimmed := strings.TrimRight(content, "\n")
	if trimmed == "" {
		return LogHeading + "\n\n" + entry + "\n"
	}
	lines := strings.Split(trimmed, "\n")
	heading := -1
	for index, line := range lines {
		if strings.TrimSpace(line) == LogHeading {
			heading = index
			break
		}
	}
	if heading < 0 {
		return trimmed + "\n\n" + LogHeading + "\n\n" + entry + "\n"
	}
	// 找到该段结束位置：下一条二级标题（## ）或文末。
	end := len(lines)
	for index := heading + 1; index < len(lines); index++ {
		line := lines[index]
		if strings.HasPrefix(strings.TrimSpace(line), "## ") {
			end = index
			break
		}
	}
	// 段尾空行不参与"末尾追加"，避免生成空行夹在中间。
	for end > heading+1 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	insert := make([]string, 0, len(lines)+2)
	insert = append(insert, lines[:end]...)
	insert = append(insert, entry)
	if end < len(lines) {
		insert = append(insert, "")
	}
	insert = append(insert, lines[end:]...)
	return strings.Join(insert, "\n") + "\n"
}

// formatLogEntry 把一段文本渲染成带日期前缀的 log 条目。
func formatLogEntry(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	date := time.Now().UTC().Format("2006-01-02")
	lines := strings.Split(text, "\n")
	var builder strings.Builder
	builder.WriteString("- " + date + " " + strings.TrimSpace(lines[0]))
	for _, line := range lines[1:] {
		line = strings.TrimRight(line, " \t\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		builder.WriteString("\n  " + line)
	}
	return builder.String()
}

// rewriteLogText 是"覆盖写"的审计文本（日期由 appendLogLine 统一加，避免出现两个日期）。
func rewriteLogText(oldBytes int) string {
	return "rewritten (was " + itoa(oldBytes) + " bytes)"
}

// itoa 是 strconv.Itoa 的小包装，避免本文件再引入一个 import。
func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var digits [20]byte
	index := len(digits)
	for value > 0 {
		index--
		digits[index] = byte('0' + value%10)
		value /= 10
	}
	if negative {
		index--
		digits[index] = '-'
	}
	return string(digits[index:])
}
