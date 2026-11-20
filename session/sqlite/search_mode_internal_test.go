package sqlite

// 内部用例：fts 的 MATCH 表达式与 CJK 词表的形状。
//
// 行为测试（search_quality_test.go）只看"搜不搜得到"，而"搜得到"可以由 substring
// 降级满足——那样的测试在删掉 CJK 分支之后仍然会绿。这里直接钉住表达式的来源：
// 2 字 CJK 必须走 search_cjk 列，普通 ASCII 走 search_text 列，撑不满窗口的短查询
// 返回空串（调用方据此降级）。

import (
	"strings"
	"testing"
)

func TestFTSMatchExpressionRoutesCJKThroughAuxiliaryColumn(t *testing.T) {
	cases := []struct {
		name       string
		text       string
		mustHave   []string
		mustNotHas []string
		empty      bool
	}{
		{
			name:     "two-char CJK uses the auxiliary column",
			text:     "磁盘",
			mustHave: []string{"{search_cjk}", `"#磁盘#"`},
		},
		{
			name:     "single CJK char uses the unigram",
			text:     "盘",
			mustHave: []string{"{search_cjk}", `"#盘#"`},
		},
		{
			name:       "ASCII phrase goes through search_text",
			text:       "deploy",
			mustHave:   []string{"{search_text}", `"deploy"`},
			mustNotHas: []string{"{search_cjk}"},
		},
		{
			name:  "query shorter than a trigram window has no expression",
			text:  "go",
			empty: true,
		},
	}
	for _, tc := range cases {
		expression := ftsMatchExpression(tc.text)
		if tc.empty {
			if expression != "" {
				t.Fatalf("%s: ftsMatchExpression(%q) = %q，want 空串（应降级为 substring）",
					tc.name, tc.text, expression)
			}
			continue
		}
		for _, want := range tc.mustHave {
			if !strings.Contains(expression, want) {
				t.Fatalf("%s: 表达式 %q 缺 %q", tc.name, expression, want)
			}
		}
		for _, unwanted := range tc.mustNotHas {
			if strings.Contains(expression, unwanted) {
				t.Fatalf("%s: 表达式 %q 不该包含 %q", tc.name, expression, unwanted)
			}
		}
	}
}

// TestSearchCJKForSkipsNonCJK：辅助列只放 CJK 词项，拉丁文本交给 search_text，
// 否则同一份内容会在 bm25 里被算两次。
func TestSearchCJKForSkipsNonCJK(t *testing.T) {
	if got := searchCJKFor("deploy the build"); got != "" {
		t.Fatalf("searchCJKFor(纯 ASCII) = %q, want 空串", got)
	}
	got := searchCJKFor("磁盘上 gate")
	for _, want := range []string{"#磁#", "#盘#", "#上#", "#磁盘#", "#盘上#"} {
		if !strings.Contains(got, want) {
			t.Fatalf("searchCJKFor = %q，缺 %q", got, want)
		}
	}
	if strings.Contains(got, "gate") {
		t.Fatalf("searchCJKFor = %q，不该包含拉丁词", got)
	}
}
