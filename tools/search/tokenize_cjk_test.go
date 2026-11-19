package search

// 本文件固定 CJK 的 bigram 分词规则。
//
// 背景（审计 P1）：旧实现把连续汉字当一个词项（"审批门禁" → 单个词项），于是
// 查询里只要边界不同（"门禁" vs 文档里的"审批门禁"）就**系统性零命中**，而且
// 静默——用户以为没这段内容。规则改为：
//
//   - 连续 CJK 段：每个字一个 unigram（保住单字查询）+ 每对相邻字一个 bigram；
//   - 拉丁/数字与既有 camelCase/snake_case 规则**完全不变**（英文排序不变）。

import (
	"reflect"
	"strings"
	"testing"
)

func TestTokenizeCJKBigrams(t *testing.T) {
	cases := []struct {
		name string
		text string
		want []string
	}{
		{name: "single char keeps unigram", text: "门", want: []string{"门"}},
		{name: "two chars", text: "搜索", want: []string{"搜", "索", "搜索"}},
		{
			name: "four chars",
			text: "审批门禁",
			want: []string{"审", "批", "门", "禁", "审批", "批门", "门禁"},
		},
		{
			name: "mixed latin and cjk in one query",
			text: "gate 审批",
			want: []string{"gate", "审", "批", "审批"},
		},
		{
			name: "cjk runs split by punctuation stay separate",
			text: "审批，门禁",
			want: []string{"审", "批", "审批", "门", "禁", "门禁"},
		},
		{name: "latin unchanged", text: "searchIndex", want: []string{"search", "index"}},
		{name: "digits unchanged", text: "b2b utf8", want: []string{"b2b", "utf8"}},
		{
			name: "longer sentence emits overlapping bigrams",
			text: "实现审批门禁组合器",
			want: []string{
				"实", "现", "审", "批", "门", "禁", "组", "合", "器",
				"实现", "现审", "审批", "批门", "门禁", "禁组", "组合", "合器",
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := Tokenize(testCase.text); !reflect.DeepEqual(got, testCase.want) {
				t.Fatalf("Tokenize(%q) = %v, want %v", testCase.text, got, testCase.want)
			}
		})
	}
}

// TestTokenizeCJKIsDeterministic 与英文那条一样：BM25 的 df/avgdl 依赖确定性。
func TestTokenizeCJKIsDeterministic(t *testing.T) {
	text := "实现审批门禁组合器，并在 gate 层复用"
	first := Tokenize(text)
	for attempt := 0; attempt < 5; attempt++ {
		if other := Tokenize(text); !reflect.DeepEqual(first, other) {
			t.Fatalf("Tokenize is not deterministic: %v vs %v", first, other)
		}
	}
	if !strings.Contains(strings.Join(first, " "), "审批") {
		t.Fatalf("bigram missing from %v", first)
	}
}
