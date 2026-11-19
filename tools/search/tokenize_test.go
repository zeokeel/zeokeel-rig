package search

// 本文件固定 Tokenize 的确定性规则：按非字母数字拆分、camelCase 与 snake_case
// 再拆、全小写。BM25 的排序与 mentions 的解析都依赖同一份分词，所以先把它钉死。

import (
	"reflect"
	"testing"
)

func TestTokenize(t *testing.T) {
	cases := []struct {
		name string
		text string
		want []string
	}{
		{name: "empty", text: "", want: nil},
		{name: "blank", text: "   \t\n", want: nil},
		{name: "plain words", text: "Hello World", want: []string{"hello", "world"}},
		{name: "punctuation splits", text: "foo-bar.baz,qux", want: []string{"foo", "bar", "baz", "qux"}},
		{name: "snake case", text: "snake_case_name", want: []string{"snake", "case", "name"}},
		{name: "camel case", text: "searchIndex", want: []string{"search", "index"}},
		{name: "pascal case", text: "SearchIndex", want: []string{"search", "index"}},
		{name: "acronym", text: "HTTPServer", want: []string{"http", "server"}},
		{name: "acronym with digit", text: "getHTTP2Token", want: []string{"get", "http2", "token"}},
		{name: "digits stay attached", text: "b2b utf8", want: []string{"b2b", "utf8"}},
		{name: "pure digits", text: "123 456", want: []string{"123", "456"}},
		{name: "mixed separators", text: "  one__two--three  ", want: []string{"one", "two", "three"}},
		// CJK 段现在产出 unigram + 相邻 bigram（见 tokenize.go 的说明与
		// tokenize_cjk_test.go）：旧断言把"搜索"当成一个词项，会让"门禁"这类
		// 边界不同的查询系统性零命中。拉丁/数字/标识符的规则未变。
		{name: "unicode letters", text: "搜索 工具_测试", want: []string{
			"搜", "索", "搜索",
			"工", "具", "工具",
			"测", "试", "测试",
		}},
		{name: "all caps with separator", text: "SCREAMING_SNAKE", want: []string{"screaming", "snake"}},
		// 驼峰规则对任意大小写交替生效：MiXeD 是三次交替，因此拆成 mi|xe|d。
		// 源码里的标识符几乎不会这样写，这里固定规则本身而不是猜"本意"。
		{name: "camel boundary in mixed case", text: "MiXeD", want: []string{"mi", "xe", "d"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := Tokenize(testCase.text); !reflect.DeepEqual(got, testCase.want) {
				t.Fatalf("Tokenize(%q) = %v, want %v", testCase.text, got, testCase.want)
			}
		})
	}
}

// TestTokenizeIsDeterministic 固定"同一输入同一输出"（BM25 的 df/avgdl 依赖它）。
func TestTokenizeIsDeterministic(t *testing.T) {
	text := "BM25Searcher Indexes 2000 files_with snake_case and camelCase 分词"
	first := Tokenize(text)
	for attempt := 0; attempt < 5; attempt++ {
		if other := Tokenize(text); !reflect.DeepEqual(first, other) {
			t.Fatalf("Tokenize is not deterministic: %v vs %v", first, other)
		}
	}
}
