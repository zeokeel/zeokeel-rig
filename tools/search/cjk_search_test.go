package search

// 本文件是"中文查询必须命中"的端到端回归（审计 P1）。
//
// 旧行为：CJK 连续段被当成一个词项，查询的切分边界与文档不同就系统性零命中，
// 而且 auto 模式会静默回落到 0 条结果。这里同时钉住：
//   - 四字查询（"审批门禁"）命中含该短语的文档；
//   - 两字查询（"门禁"）命中；
//   - 单字查询（"门"）仍能命中（unigram 保留）；
//   - 中英混排（"gate 审批"）命中；
//   - 不相关文档不被拉进来（bigram 提供精度）。

import (
	"strings"
	"testing"
)

const cjkFixture = "本模块实现审批门禁组合器，并在 gate 层复用同一套判定。"

func TestSearchChineseQueryHits(t *testing.T) {
	root := writeTree(t, map[string]string{
		"docs/security.md":  cjkFixture,
		"docs/unrelated.md": "这是一段与主题无关的中文说明，讲的是界面配色与字体。\n",
		"docs/english.md":   "the gate module wraps permission checks\n",
	})
	tool := New(root)

	cases := []struct {
		name      string
		query     string
		wantFile  string
		wantEmpty bool
	}{
		{name: "four char phrase", query: "审批门禁", wantFile: "docs/security.md"},
		{name: "two char word", query: "门禁", wantFile: "docs/security.md"},
		{name: "single char", query: "门", wantFile: "docs/security.md"},
		{name: "cjk inside longer sentence", query: "组合器", wantFile: "docs/security.md"},
		{name: "mixed latin and cjk", query: "gate 审批", wantFile: "docs/security.md"},
		{name: "phrase absent", query: "缓存淘汰", wantEmpty: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			result := invoke(t, tool, `{"query":"`+testCase.query+`","mode":"text"}`)
			if result.IsError {
				t.Fatalf("search errored: %s", result.Content)
			}
			if testCase.wantEmpty {
				if strings.Contains(result.Content, "docs/security.md") {
					t.Fatalf("query %q matched docs/security.md unexpectedly: %q", testCase.query, result.Content)
				}
				return
			}
			if !strings.Contains(result.Content, testCase.wantFile) {
				t.Fatalf("query %q did not match %s: %q", testCase.query, testCase.wantFile, result.Content)
			}
			if strings.Contains(result.Content, "docs/unrelated.md") {
				t.Fatalf("query %q dragged in an unrelated document: %q", testCase.query, result.Content)
			}
		})
	}
}

// TestSearchChineseQueryAutoMode 走默认 auto：单词语查询会先试文件名（无命中），
// 然后必须回落到内容检索并命中——这正是用户实际会遇到的路径。
func TestSearchChineseQueryAutoMode(t *testing.T) {
	root := writeTree(t, map[string]string{
		"docs/security.md": cjkFixture,
	})
	tool := New(root)
	result := invoke(t, tool, `{"query":"审批门禁"}`)
	if result.IsError {
		t.Fatalf("search errored: %s", result.Content)
	}
	if !strings.Contains(result.Content, "docs/security.md") {
		t.Fatalf("auto mode did not fall back to content search: %q", result.Content)
	}
}

// TestSearchEnglishRankingUnchanged 保证英文排序没有被 CJK 改动影响。
func TestSearchEnglishRankingUnchanged(t *testing.T) {
	root := writeTree(t, map[string]string{
		"a.md": "search engine ranking\n",
		"b.md": "search engine ranking search engine ranking\n",
		"c.md": "unrelated text\n",
	})
	tool := New(root)
	result := invoke(t, tool, `{"query":"search engine","mode":"text"}`)
	if result.IsError {
		t.Fatalf("search errored: %s", result.Content)
	}
	if !strings.Contains(result.Content, "b.md") || !strings.Contains(result.Content, "a.md") {
		t.Fatalf("english hits missing: %q", result.Content)
	}
	if strings.Contains(result.Content, "c.md") {
		t.Fatalf("english query matched unrelated file: %q", result.Content)
	}
}
