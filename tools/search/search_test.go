package search

// 本文件是 search 工具的行为测试（文件模糊搜索 / BM25 内容检索 / @提及解析 /
// auto 分流 / 限制与错误契约）。所有用例都在 t.TempDir() 里构造真实的小文件树，
// 因此断言的是模型真正看到的输出文本。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

// writeTree 把 files 里的相对路径写到临时根目录下，返回根目录。
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir for %q: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %q: %v", name, err)
		}
	}
	return root
}

// fixtureFiles 是默认的小仓库：含普通文件、被忽略目录、二进制与超长行。
func fixtureFiles() map[string]string {
	return map[string]string{
		"README.md":             "# Project\n\nWelcome to the project.\n",
		"main.go":               "package main\n\nfunc main() {\n\tfmt.Println(\"hello world\")\n}\n",
		"src/search.go":         "package search\n\nfunc SearchIndex() {}\n\n// full text search engine\n",
		"src/search_test.go":    "package search\n\nfunc TestSearch(t *testing.T) {}\n",
		"src/util.go":           "package search\n\nfunc utilHelper() {}\n",
		"docs/guide.md":         "fuzzy file search and BM25 ranking\n",
		"docs/user guide.md":    "# user guide\n\nmention me\n",
		"node_modules/lib/x.js": "search engine bundled\n",
		".git/HEAD":             "ref: refs/heads/main\n",
		"vendor/v.go":           "package vendor // search\n",
		"dist/bundle.js":        "search bundled\n",
		"build/out.go":          "package build // search\n",
		"bin.dat":               "abc\x00def search\n",
		"long.txt":              strings.Repeat("x", 260) + " needle\n",
	}
}

// invoke 调用一次工具；工具返回 error 时让测试立刻失败（timeout 的用例单独断言）。
func invoke(t *testing.T, tool rig.Tool, arguments string) rig.ToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := tool.Invoke(ctx, rig.ToolCall{Name: ToolName, Arguments: arguments})
	if err != nil {
		t.Fatalf("Invoke(%s) error: %v", arguments, err)
	}
	return result
}

// newTool 构造被测工具。
func newTool(t *testing.T, root string, opts ...Option) rig.Tool {
	t.Helper()
	return New(root, opts...)
}

// toolCall 构造一次工具调用。
func toolCall(t *testing.T, arguments string) rig.ToolCall {
	t.Helper()
	return rig.ToolCall{Name: ToolName, Arguments: arguments}
}

func TestFilesModeRankedFilenames(t *testing.T) {
	root := writeTree(t, fixtureFiles())
	got := invoke(t, newTool(t, root), `{"query":"search","mode":"files"}`)
	if got.IsError {
		t.Fatalf("unexpected error: %s", got.Content)
	}
	// 文件名里含 "search" 的只有这两个；更短的 search.go 排在前面。
	want := "src/search.go\nsrc/search_test.go"
	if got.Content != want {
		t.Fatalf("files result = %q, want %q", got.Content, want)
	}
}

func TestFilesModeFuzzySubsequence(t *testing.T) {
	root := writeTree(t, fixtureFiles())
	got := invoke(t, newTool(t, root), `{"query":"srch","mode":"files"}`)
	if got.IsError {
		t.Fatalf("unexpected error: %s", got.Content)
	}
	if !strings.Contains(got.Content, "src/search.go") {
		t.Fatalf("fuzzy result = %q, want it to contain src/search.go", got.Content)
	}
}

func TestFilesModeLimit(t *testing.T) {
	root := writeTree(t, fixtureFiles())
	got := invoke(t, newTool(t, root, WithMaxResults(1)), `{"query":"search","mode":"files"}`)
	want := "src/search.go\n[truncated: 1 more matches]"
	if got.Content != want {
		t.Fatalf("limited files result = %q, want %q", got.Content, want)
	}
}

func TestFilesModePathRestriction(t *testing.T) {
	root := writeTree(t, fixtureFiles())
	tool := newTool(t, root)
	inside := invoke(t, tool, `{"query":"util","mode":"files","path":"src"}`)
	if inside.Content != "src/util.go" {
		t.Fatalf("scoped result = %q, want src/util.go", inside.Content)
	}
	outside := invoke(t, tool, `{"query":"util","mode":"files","path":"docs"}`)
	if outside.Content != "" {
		t.Fatalf("scoped result = %q, want no matches under docs", outside.Content)
	}
}

func TestFilesModeNoMatchesIsEmpty(t *testing.T) {
	root := writeTree(t, fixtureFiles())
	got := invoke(t, newTool(t, root), `{"query":"zzzznomatch","mode":"files"}`)
	if got.IsError {
		t.Fatalf("unexpected error: %s", got.Content)
	}
	if got.Content != "" {
		t.Fatalf("no-match result = %q, want the empty string (same convention as grep/glob)", got.Content)
	}
}

func TestTextModeReturnsHitLine(t *testing.T) {
	root := writeTree(t, fixtureFiles())
	got := invoke(t, newTool(t, root), `{"query":"bm25","mode":"text"}`)
	want := "docs/guide.md:1:fuzzy file search and BM25 ranking"
	if got.Content != want {
		t.Fatalf("text result = %q, want %q", got.Content, want)
	}
}

func TestTextModeMultipleWordsAreAND(t *testing.T) {
	root := writeTree(t, fixtureFiles())
	tool := newTool(t, root)
	both := invoke(t, tool, `{"query":"fuzzy bm25","mode":"text"}`)
	if both.Content != "docs/guide.md:1:fuzzy file search and BM25 ranking" {
		t.Fatalf("AND result = %q, want the single matching line", both.Content)
	}
	oneMissing := invoke(t, tool, `{"query":"fuzzy zzznomatch","mode":"text"}`)
	if oneMissing.Content != "" {
		t.Fatalf("AND with a missing term = %q, want no matches", oneMissing.Content)
	}
}

func TestTextModeSkipsIgnoredDirectories(t *testing.T) {
	root := writeTree(t, fixtureFiles())
	tool := newTool(t, root)
	// "bundled" 只出现在 node_modules/ 与 dist/ 里：两者都必须被跳过。
	if got := invoke(t, tool, `{"query":"bundled","mode":"text"}`); got.Content != "" {
		t.Fatalf("ignored-directory result = %q, want no matches", got.Content)
	}
	// "refs" 只出现在 .git/ 里。
	if got := invoke(t, tool, `{"query":"refs","mode":"text"}`); got.Content != "" {
		t.Fatalf(".git result = %q, want no matches", got.Content)
	}
}

func TestTextModeSkipsBinaryFiles(t *testing.T) {
	root := writeTree(t, fixtureFiles())
	got := invoke(t, newTool(t, root), `{"query":"def","mode":"text"}`)
	if got.Content != "" {
		t.Fatalf("binary result = %q, want no matches (bin.dat has a NUL byte)", got.Content)
	}
}

func TestTextModeSkipsOversizeFiles(t *testing.T) {
	root := writeTree(t, fixtureFiles())
	small := invoke(t, newTool(t, root, WithMaxFileBytes(16)), `{"query":"needle","mode":"text"}`)
	if small.Content != "" {
		t.Fatalf("oversize result = %q, want no matches when the file exceeds max_file_bytes", small.Content)
	}
	// 同一个查询在默认上限下命中 long.txt 的第一行。
	full := invoke(t, newTool(t, root), `{"query":"needle","mode":"text"}`)
	if !strings.HasPrefix(full.Content, "long.txt:1:") {
		t.Fatalf("full result = %q, want a long.txt hit", full.Content)
	}
}

func TestTextModeTruncatesLongLines(t *testing.T) {
	root := writeTree(t, fixtureFiles())
	got := invoke(t, newTool(t, root), `{"query":"needle","mode":"text"}`)
	const prefix = "long.txt:1:"
	if !strings.HasPrefix(got.Content, prefix) {
		t.Fatalf("result = %q, want the %q prefix", got.Content, prefix)
	}
	snippet := strings.TrimPrefix(got.Content, prefix)
	runes := []rune(snippet)
	if len(runes) != maxSnippetRunes+1 {
		t.Fatalf("snippet rune length = %d, want %d (200 chars + ellipsis)", len(runes), maxSnippetRunes+1)
	}
	if !strings.HasSuffix(snippet, "…") {
		t.Fatalf("snippet = %q, want a trailing ellipsis", snippet)
	}
	if strings.Contains(snippet, "needle") {
		t.Fatalf("snippet = %q, want the line truncated before the term (spec: hit line truncated to 200 chars)", snippet)
	}
}

func TestTextModeCamelAndSnakeCaseQueries(t *testing.T) {
	root := writeTree(t, fixtureFiles())
	tool := newTool(t, root)
	// SearchIndex 分词成 search+index：只有 src/search.go 同时含两词。
	for _, query := range []string{"SearchIndex", "search_index", "SEARCH-INDEX"} {
		got := invoke(t, tool, fmt.Sprintf(`{"query":%q,"mode":"text"}`, query))
		want := "src/search.go:3:func SearchIndex() {}"
		if got.Content != want {
			t.Fatalf("query %q result = %q, want %q", query, got.Content, want)
		}
	}
}

func TestTextModeBM25Ordering(t *testing.T) {
	root := writeTree(t, map[string]string{
		"a.txt": "alpha alpha alpha\n",
		"b.txt": "alpha beta gamma delta epsilon zeta eta theta\n",
		"c.txt": "alpha\n",
	})
	got := invoke(t, newTool(t, root), `{"query":"alpha","mode":"text"}`)
	// tf 高的短文档最高；tf=1 的短文档次之；tf=1 的长文档最低。
	want := "a.txt:1:alpha alpha alpha\nc.txt:1:alpha\nb.txt:1:alpha beta gamma delta epsilon zeta eta theta"
	if got.Content != want {
		t.Fatalf("BM25 order = %q, want %q", got.Content, want)
	}
}

func TestTextModeDefaultLimit(t *testing.T) {
	files := make(map[string]string, 25)
	for index := 0; index < 25; index++ {
		files[fmt.Sprintf("f%02d.txt", index)] = "sharedtoken\n"
	}
	root := writeTree(t, files)
	got := invoke(t, newTool(t, root), `{"query":"sharedtoken","mode":"text"}`)
	lines := strings.Split(got.Content, "\n")
	if len(lines) != DefaultMaxResults+1 {
		t.Fatalf("line count = %d, want %d results + 1 truncation notice", len(lines), DefaultMaxResults)
	}
	if want := fmt.Sprintf("[truncated: %d more matches]", 25-DefaultMaxResults); lines[len(lines)-1] != want {
		t.Fatalf("last line = %q, want %q", lines[len(lines)-1], want)
	}
}

func TestMaxResultsClampedToCap(t *testing.T) {
	files := make(map[string]string, MaxMaxResults+10)
	for index := 0; index < MaxMaxResults+10; index++ {
		files[fmt.Sprintf("f%03d.txt", index)] = "clampedtoken\n"
	}
	root := writeTree(t, files)
	got := invoke(t, newTool(t, root, WithMaxResults(500)), `{"query":"clampedtoken","mode":"text"}`)
	lines := strings.Split(got.Content, "\n")
	if len(lines) != MaxMaxResults+1 {
		t.Fatalf("line count = %d, want %d results + notice", len(lines), MaxMaxResults)
	}
	if !strings.Contains(lines[len(lines)-1], "more matches") {
		t.Fatalf("last line = %q, want a truncation notice", lines[len(lines)-1])
	}
}

func TestMentionsModeResolvesAndMarksMissing(t *testing.T) {
	root := writeTree(t, fixtureFiles())
	got := invoke(t, newTool(t, root), `{"query":"look at @src/search.go:2 and @nope.go","mode":"mentions"}`)
	want := "src/search.go:2\nnope.go (missing)"
	if got.Content != want {
		t.Fatalf("mentions result = %q, want %q", got.Content, want)
	}
}

func TestMentionsModeQuotedAndRanges(t *testing.T) {
	root := writeTree(t, fixtureFiles())
	tool := newTool(t, root)
	quoted := invoke(t, tool, `{"query":"read @\"docs/user guide.md\" please","mode":"mentions"}`)
	if quoted.Content != "docs/user guide.md" {
		t.Fatalf("quoted mention = %q, want docs/user guide.md", quoted.Content)
	}
	withRange := invoke(t, tool, `{"query":"@src/search.go:2-3","mode":"mentions"}`)
	if withRange.Content != "src/search.go:2-3" {
		t.Fatalf("range mention = %q, want src/search.go:2-3", withRange.Content)
	}
}

func TestMentionsModeWithoutMentionsIsEmpty(t *testing.T) {
	root := writeTree(t, fixtureFiles())
	got := invoke(t, newTool(t, root), `{"query":"no mentions here","mode":"mentions"}`)
	if got.Content != "" {
		t.Fatalf("mentions result = %q, want empty", got.Content)
	}
}

func TestAutoModeRouting(t *testing.T) {
	root := writeTree(t, fixtureFiles())
	tool := newTool(t, root)
	cases := []struct {
		name        string
		query       string
		wantExact   string
		wantContain string
	}{
		{name: "leading mention", query: "@src/search.go", wantExact: "src/search.go"},
		// 自然语言整句里的 @ 不再吞掉整句语义：走 text 模式，并在结果末尾提示
		// 可以显式改用 mentions（v0.20 行为变更，见 withMentionHint）。
		{name: "prose with mention", query: "please read @docs/guide.md now", wantExact: "# hints: contains @references, run mode=mentions to resolve them"},
		{name: "space routes to text", query: "hello world", wantExact: "main.go:4:\tfmt.Println(\"hello world\")"},
		{name: "single token matches filename", query: "search", wantExact: "src/search.go\nsrc/search_test.go"},
		{name: "single token falls back to text", query: "engine", wantExact: "src/search.go:5:// full text search engine"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := invoke(t, tool, fmt.Sprintf(`{"query":%q}`, testCase.query))
			if got.IsError {
				t.Fatalf("unexpected error: %s", got.Content)
			}
			if testCase.wantExact != "" && got.Content != testCase.wantExact {
				t.Fatalf("auto result = %q, want %q", got.Content, testCase.wantExact)
			}
			if testCase.wantContain != "" && !strings.Contains(got.Content, testCase.wantContain) {
				t.Fatalf("auto result = %q, want it to contain %q", got.Content, testCase.wantContain)
			}
		})
	}
}

func TestErrors(t *testing.T) {
	root := writeTree(t, fixtureFiles())
	tool := newTool(t, root)
	cases := []struct {
		name     string
		args     string
		contains []string
	}{
		{name: "empty query", args: `{"query":""}`, contains: []string{"query is required"}},
		{name: "blank query", args: `{"query":"   "}`, contains: []string{"query is required"}},
		{name: "unknown mode", args: `{"query":"x","mode":"bogus"}`, contains: []string{`unknown mode "bogus"`}},
		{name: "path escapes root", args: `{"query":"x","path":"../outside"}`, contains: []string{"outside the workspace root"}},
		{name: "absolute path", args: `{"query":"x","path":"/etc"}`, contains: []string{"outside the workspace root"}},
		{name: "path is a file", args: `{"query":"x","path":"README.md"}`, contains: []string{"not a directory"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := invoke(t, tool, testCase.args)
			if !got.IsError {
				t.Fatalf("args %s: got %q, want an error result", testCase.args, got.Content)
			}
			for _, want := range testCase.contains {
				if !strings.Contains(got.Content, want) {
					t.Fatalf("error = %q, want it to contain %q", got.Content, want)
				}
			}
		})
	}
}

// TestInvalidArgumentsAreAnError 固定"参数 JSON 非法"走工具 error（由 agent 回报），
// 而不是 IsError 结果——与本仓库其它工具一致。
func TestInvalidArgumentsAreAnError(t *testing.T) {
	tool := New(writeTree(t, fixtureFiles()))
	if _, err := tool.Invoke(context.Background(), toolCall(t, `{"query":`)); err == nil {
		t.Fatal("Invoke with malformed JSON = nil error, want a decode error")
	} else if !strings.Contains(err.Error(), "invalid arguments JSON") {
		t.Fatalf("error = %v, want it to mention invalid arguments JSON", err)
	}
}

func TestMissingRootIsAnError(t *testing.T) {
	root := filepath.Join(t.TempDir(), "does-not-exist")
	got := invoke(t, newTool(t, root), `{"query":"x"}`)
	if !got.IsError || !strings.Contains(got.Content, "workspace root") {
		t.Fatalf("result = %+v, want a workspace-root error", got)
	}
}

func TestTimeoutIsAnExplicitError(t *testing.T) {
	root := writeTree(t, fixtureFiles())
	// 1 纳秒的预算在进入搜索时就已经到期：这是确定性的超时路径。
	got := invoke(t, newTool(t, root, WithTimeout(time.Nanosecond)), `{"query":"search","mode":"files"}`)
	if !got.IsError {
		t.Fatalf("result = %+v, want a timeout error", got)
	}
	if !strings.Contains(got.Content, "timed out") {
		t.Fatalf("timeout error = %q, want it to say \"timed out\"", got.Content)
	}
}

func TestCanceledContextPropagates(t *testing.T) {
	root := writeTree(t, fixtureFiles())
	tool := New(root)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tool.Invoke(ctx, toolCall(t, `{"query":"search"}`)); err == nil {
		t.Fatal("Invoke with a canceled context = nil error, want context.Canceled")
	} else if !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestFilesModeIgnoresMaxFileBytes(t *testing.T) {
	root := writeTree(t, fixtureFiles())
	got := invoke(t, newTool(t, root, WithMaxFileBytes(1)), `{"query":"srch","mode":"files"}`)
	if !strings.Contains(got.Content, "src/search.go") {
		t.Fatalf("files result = %q, want filename search to ignore max_file_bytes", got.Content)
	}
}

func TestToolMetadata(t *testing.T) {
	tool := New(t.TempDir())
	if got := tool.Name(); got != ToolName {
		t.Fatalf("Name = %q, want %q", got, ToolName)
	}
	if strings.TrimSpace(tool.Description()) == "" {
		t.Fatal("Description is empty")
	}
	schema := tool.Schema()
	if got := schema["type"]; got != "object" {
		t.Fatalf("schema type = %v, want object", got)
	}
	if got := schema["additionalProperties"]; got != false {
		t.Fatalf("additionalProperties = %v, want false", got)
	}
	required, ok := schema["required"].([]string)
	if !ok || len(required) != 1 || required[0] != "query" {
		t.Fatalf("required = %v, want [query]", schema["required"])
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties = %T, want map[string]any", schema["properties"])
	}
	for _, name := range []string{"query", "path", "mode", "max_results"} {
		if _, exists := properties[name]; !exists {
			t.Fatalf("schema is missing the %q property", name)
		}
	}
	mode, ok := properties["mode"].(map[string]any)
	if !ok {
		t.Fatalf("mode property = %T", properties["mode"])
	}
	enum, ok := mode["enum"].([]string)
	if !ok {
		t.Fatalf("mode enum = %T, want []string", mode["enum"])
	}
	wantEnum := []string{"auto", "files", "text", "mentions"}
	if strings.Join(enum, ",") != strings.Join(wantEnum, ",") {
		t.Fatalf("mode enum = %v, want %v", enum, wantEnum)
	}
}
