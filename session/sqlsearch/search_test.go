package sqlsearch_test

// The search helpers are shared by every host with a session database: JSON is
// decoded before matching (quotes in user content are content, not escaping),
// queries are escaped for LIKE, and snippets are cut rune-safely around the match.

import (
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/session/sqlsearch"
)

func TestMessageColumnsDecodeToolArguments(t *testing.T) {
	message := rig.NewMessage(rig.RoleAssistant, `he said "hi"`)
	message.ToolCalls = []rig.ToolCall{{
		ID: "c1", Name: "write_file", Arguments: `{"path":"docs/a.md","content":"quoted \"text\" and \\ backslash"}`,
	}}
	text, search := sqlsearch.MessageColumns(message)
	if text != `he said "hi"` {
		t.Fatalf("text = %q", text)
	}
	for _, want := range []string{"assistant", `he said "hi"`, "write_file", "docs/a.md", `quoted "text" and \ backslash`} {
		if !strings.Contains(search, want) {
			t.Errorf("search text %q lacks %q", search, want)
		}
	}
	if strings.Contains(search, `\"`) {
		t.Fatalf("search text kept JSON escaping: %q", search)
	}
}

func TestEventTextDecodesNestedPayload(t *testing.T) {
	if got := sqlsearch.EventText(nil); got != "" {
		t.Fatalf("empty payload = %q", got)
	}
	payload := []byte(`{"kind":"run_start","nested":{"model":"deepseek-chat"},"list":["a","b"]}`)
	got := sqlsearch.EventText(payload)
	for _, want := range []string{"run_start", "model", "deepseek-chat", "nested", "a", "b"} {
		if !strings.Contains(got, want) {
			t.Errorf("event text %q lacks %q", got, want)
		}
	}
	if sqlsearch.EventText([]byte("not json")) != "" {
		t.Fatal("invalid JSON produced search text")
	}
}

func TestEscapeLikeMatchesLiterally(t *testing.T) {
	if got := sqlsearch.EscapeLike(`50%_off\`); got != `50\%\_off\\` {
		t.Fatalf("EscapeLike = %q", got)
	}
}

func TestSnippetAroundTheMatch(t *testing.T) {
	text := strings.Repeat("prefix ", 30) + "needle" + strings.Repeat(" suffix", 30)
	snippet := sqlsearch.Snippet(text, "needle")
	if !strings.Contains(snippet, "needle") {
		t.Fatalf("snippet = %q", snippet)
	}
	if !strings.HasPrefix(snippet, "…") || !strings.HasSuffix(snippet, "…") {
		t.Fatalf("snippet is not marked as an excerpt: %q", snippet)
	}
	// A match that is not in the plain text falls back to the head of the text.
	fallback := sqlsearch.Snippet("short text with `code` and ```fence```", "elsewhere")
	if strings.Contains(fallback, "`") {
		t.Fatalf("fallback kept Markdown markers: %q", fallback)
	}
	if got := sqlsearch.Snippet("", "x"); got != "" {
		t.Fatalf("empty text snippet = %q", got)
	}
}

func TestSnippetIsRuneSafe(t *testing.T) {
	text := strings.Repeat("中文", 100) + "关键词" + strings.Repeat("结尾", 100)
	snippet := sqlsearch.Snippet(text, "关键词")
	if !strings.Contains(snippet, "关键词") {
		t.Fatalf("snippet = %q", snippet)
	}
}

func TestPlainTextDropsMarkdownNoise(t *testing.T) {
	got := sqlsearch.PlainText("## Heading\n\nsome   text\n```go\ncode\n```")
	if strings.Contains(got, "#") || strings.Contains(got, "`") {
		t.Fatalf("plain text kept Markdown: %q", got)
	}
	if got != "Heading some text go code" {
		t.Fatalf("plain text = %q", got)
	}
}
