package search

// 本文件固定 ParseMentions 的语法：@path、@"path with space"、@path:12、
// @path:12-20，以及不该被当成提及的形状（邮箱、未闭合引号、悬空 @）。

import (
	"reflect"
	"testing"
)

func TestParseMentionsBasic(t *testing.T) {
	cases := []struct {
		name string
		text string
		want []Mention
	}{
		{
			name: "single path",
			text: "请看 @src/search.go 里的实现",
			want: []Mention{{Path: "src/search.go"}},
		},
		{
			name: "leading mention",
			text: "@main.go",
			want: []Mention{{Path: "main.go"}},
		},
		{
			name: "quoted path with space",
			text: `读一下 @"docs/user guide.md" 再说`,
			want: []Mention{{Path: "docs/user guide.md"}},
		},
		{
			name: "line number",
			text: "定位 @src/search.go:12",
			want: []Mention{{Path: "src/search.go", Line: 12, EndLine: 12}},
		},
		{
			name: "line range",
			text: "定位 @src/search.go:12-20",
			want: []Mention{{Path: "src/search.go", Line: 12, EndLine: 20}},
		},
		{
			name: "quoted with line",
			text: `定位 @"docs/user guide.md":3-4`,
			want: []Mention{{Path: "docs/user guide.md", Line: 3, EndLine: 4}},
		},
		{
			name: "multiple in order",
			text: "@a.go 和 @b.go:7 以及 @c.txt",
			want: []Mention{{Path: "a.go"}, {Path: "b.go", Line: 7, EndLine: 7}, {Path: "c.txt"}},
		},
		{
			name: "trailing punctuation stripped",
			text: "see @a.go, then @b.go. done",
			want: []Mention{{Path: "a.go"}, {Path: "b.go"}},
		},
		{
			name: "duplicates collapsed",
			text: "@a.go @a.go:5 @a.go",
			want: []Mention{{Path: "a.go"}, {Path: "a.go", Line: 5, EndLine: 5}},
		},
		{
			name: "windows path",
			text: `打开 @C:\work\a.txt:7`,
			want: []Mention{{Path: `C:\work\a.txt`, Line: 7, EndLine: 7}},
		},
		{
			name: "directory mention",
			text: "@src/search/ 目录",
			want: []Mention{{Path: "src/search/"}},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := ParseMentions(testCase.text); !reflect.DeepEqual(got, testCase.want) {
				t.Fatalf("ParseMentions(%q) = %+v, want %+v", testCase.text, got, testCase.want)
			}
		})
	}
}

func TestParseMentionsIgnoresNonMentions(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{name: "email", text: "mail me at user@example.com"},
		{name: "at in code", text: "a@b"},
		{name: "dangling at", text: "just an @ sign"},
		{name: "unterminated quote", text: `broken @"docs/user guide.md`},
		{name: "empty quoted", text: `empty @"" here`},
		{name: "plain text", text: "no mentions at all"},
		{name: "empty text", text: ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := ParseMentions(testCase.text); len(got) != 0 {
				t.Fatalf("ParseMentions(%q) = %+v, want no mentions", testCase.text, got)
			}
		})
	}
}

// TestParseMentionsZeroLineIsPathText 固定 `:0` 不是合法行号（行号从 1 开始），
// 因此它留在路径文本里而不是被解析成 0。
func TestParseMentionsZeroLineIsPathText(t *testing.T) {
	mentions := ParseMentions("@a.go:0")
	if len(mentions) != 1 {
		t.Fatalf("mentions = %+v, want one", mentions)
	}
	if mentions[0].Path != "a.go:0" || mentions[0].Line != 0 || mentions[0].EndLine != 0 {
		t.Fatalf("mention = %+v, want the path to keep \":0\" with no line", mentions[0])
	}
}

// TestParseMentionsRangeOrderNormalized 固定 `12-9` 这种逆序范围被规范成升序。
func TestParseMentionsRangeOrderNormalized(t *testing.T) {
	mentions := ParseMentions("@a.go:12-9")
	if len(mentions) != 1 {
		t.Fatalf("mentions = %+v, want one", mentions)
	}
	if mentions[0].Line != 9 || mentions[0].EndLine != 12 {
		t.Fatalf("mention = %+v, want line 9..12 after normalization", mentions[0])
	}
}
