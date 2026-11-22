// Package sqlsearch holds the storage-independent halves of full-text search
// over a session database: how a message/event becomes searchable text, how a
// query is escaped for LIKE, and how a readable snippet is cut around a match.
//
// Hosts keep their own schema and SQL (table and column names differ); these
// helpers exist so the part that is easy to get wrong — JSON escaping, quotes in
// user content, rune-safe snippets — is shared and tested once.
package sqlsearch

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/zeokeel/zeokeel-rig"
)

const (
	// SnippetContext is how many runes are kept on each side of a match.
	SnippetContext = 60
	// SnippetFallback is the snippet length when the query only matched JSON
	// fields other than the message text.
	SnippetFallback = 120
)

// MessageColumns returns the snippet text (the message's plain text) and the
// decoded searchable aggregate for one message.
//
// Tool-call argument JSON is decoded recursively so its values are searchable
// without JSON escaping; reasoning and non-text content metadata are searchable
// too, matching the raw-payload surface minus structural punctuation.
func MessageColumns(message rig.Message) (text, searchText string) {
	text = message.Text()
	var builder strings.Builder
	appendToken(&builder, string(message.Role))
	appendToken(&builder, text)
	appendToken(&builder, message.Reasoning)
	appendToken(&builder, message.Name)
	appendToken(&builder, message.ToolCallID)
	for _, part := range message.Content {
		appendToken(&builder, part.ImageURL)
		appendToken(&builder, part.Detail)
	}
	for _, call := range message.ToolCalls {
		appendToken(&builder, call.ID)
		appendToken(&builder, call.Name)
		if arguments := strings.TrimSpace(call.Arguments); arguments != "" {
			var decoded any
			if err := json.Unmarshal([]byte(arguments), &decoded); err == nil {
				appendJSONText(&builder, decoded)
			}
		}
	}
	return text, strings.TrimSpace(builder.String())
}

// EventText decodes one raw JSON event payload into searchable text. The event
// type is matched separately by the caller; this is the payload half.
func EventText(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	var decoded any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return ""
	}
	var builder strings.Builder
	appendJSONText(&builder, decoded)
	return strings.TrimSpace(builder.String())
}

// EscapeLike escapes the LIKE wildcards (and the escape character) so a query is
// matched literally; the caller must use ESCAPE '\'.
func EscapeLike(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(value)
}

// Snippet returns a whitespace-collapsed excerpt around the first occurrence of
// query in text (rune safe, with … markers), or the first SnippetFallback runes
// when the match is not in the plain text.
func Snippet(text, query string) string {
	runes := []rune(text)
	if len(runes) == 0 {
		return ""
	}
	lowerText := strings.ToLower(text)
	lowerQuery := strings.ToLower(query)
	byteIndex := strings.Index(lowerText, lowerQuery)
	if byteIndex < 0 {
		return PlainText(string(runes[:minInt(len(runes), SnippetFallback)]))
	}
	start := utf8.RuneCountInString(lowerText[:byteIndex])
	matchRunes := utf8.RuneCountInString(lowerText[byteIndex : byteIndex+len(lowerQuery)])
	windowStart := start - SnippetContext
	if windowStart < 0 {
		windowStart = 0
	}
	windowEnd := start + matchRunes + SnippetContext
	if windowEnd > len(runes) {
		windowEnd = len(runes)
	}
	snippet := PlainText(string(runes[windowStart:windowEnd]))
	if windowStart > 0 {
		snippet = "…" + snippet
	}
	if windowEnd < len(runes) {
		snippet += "…"
	}
	return snippet
}

// PlainText makes a transcript excerpt readable in a search result row:
// whitespace collapsed, Markdown code fences/backticks and heading markers
// dropped (the stored message text is Markdown).
func PlainText(raw string) string {
	collapsed := collapseWhitespace(raw)
	collapsed = strings.ReplaceAll(collapsed, "```", "")
	collapsed = strings.ReplaceAll(collapsed, "`", "")
	runes := []rune(collapsed)
	out := make([]rune, 0, len(runes))
	for index := 0; index < len(runes); index++ {
		if runes[index] == '#' {
			headingStart := index == 0 || runes[index-1] == ' '
			if headingStart {
				end := index
				for end < len(runes) && runes[end] == '#' {
					end++
				}
				if end < len(runes) && runes[end] == ' ' {
					index = end
					continue
				}
			}
		}
		out = append(out, runes[index])
	}
	return strings.TrimSpace(string(out))
}

// appendJSONText appends every scalar in a decoded JSON value (object keys
// included, so field names remain searchable) without structural punctuation.
func appendJSONText(builder *strings.Builder, value any) {
	switch typed := value.(type) {
	case nil:
		return
	case string:
		appendToken(builder, typed)
	case bool:
		appendToken(builder, strconv.FormatBool(typed))
	case float64:
		appendToken(builder, strconv.FormatFloat(typed, 'g', -1, 64))
	case json.Number:
		appendToken(builder, typed.String())
	case []any:
		for _, entry := range typed {
			appendJSONText(builder, entry)
		}
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			appendToken(builder, key)
			appendJSONText(builder, typed[key])
		}
	default:
		appendToken(builder, fmt.Sprint(typed))
	}
}

func appendToken(builder *strings.Builder, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	if builder.Len() > 0 {
		builder.WriteByte(' ')
	}
	builder.WriteString(value)
}

func collapseWhitespace(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}
