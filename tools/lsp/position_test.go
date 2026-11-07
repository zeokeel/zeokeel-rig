package lsp

// UTF-16 位置与操作名归一化的单元测试：ASCII、CJK、代理对（BMP 之外的 emoji）、
// 越界坐标与 CRLF/末尾换行语义。

import (
	"strings"
	"testing"
)

func TestUTF16Len(t *testing.T) {
	cases := []struct {
		text string
		want int
	}{
		{"", 0},
		{"abc", 3},
		{"你好", 2},
		{"café", 4},
		{"😀", 2},
		{"a😀b", 4},
		{"😀😀", 4},
		{"名字 = \"😀\"", 9},
	}
	for _, testCase := range cases {
		if got := UTF16Len(testCase.text); got != testCase.want {
			t.Fatalf("UTF16Len(%q) = %d, want %d", testCase.text, got, testCase.want)
		}
	}
}

func TestValidatePositionValid(t *testing.T) {
	cases := []struct {
		name      string
		text      string
		line      int
		character int
		want      Position
	}{
		{"ASCII start", "package main\n\nfunc main() {}\n", 1, 1, Position{0, 0}},
		{"ASCII end of line", "package main\n", 1, 13, Position{0, 12}},
		{"empty line", "a\n\nb", 2, 1, Position{1, 0}},
		{"trailing newline opens an empty line", "a\n", 2, 1, Position{1, 0}},
		{"CRLF line split", "a\r\nb", 2, 1, Position{1, 0}},
		{"CR line split", "a\rb", 2, 2, Position{1, 1}},
		{"CJK width is two code units", "你好\n世界", 1, 3, Position{0, 2}},
		{"surrogate pair counts two units", "a😀b", 1, 4, Position{0, 3}},
		{"surrogate pair end of line", "a😀b", 1, 5, Position{0, 4}},
		{"empty document", "", 1, 1, Position{0, 0}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := ValidatePosition(testCase.text, testCase.line, testCase.character)
			if err != nil {
				t.Fatalf("ValidatePosition(%q, %d, %d): %v", testCase.text, testCase.line, testCase.character, err)
			}
			if got != testCase.want {
				t.Fatalf("ValidatePosition = %+v, want %+v", got, testCase.want)
			}
		})
	}
}

func TestValidatePositionErrors(t *testing.T) {
	cases := []struct {
		name      string
		text      string
		line      int
		character int
		want      string
	}{
		{"zero line", "a\nb", 0, 1, "1-based"},
		{"negative line", "a\nb", -1, 1, "1-based"},
		{"zero character", "a\nb", 1, 0, "character 0 is invalid"},
		{"negative character", "a\nb", 1, -2, "1-based UTF-16"},
		{"line past end", "a\nb", 3, 1, "outside the file"},
		{"character past ASCII line", "abc", 1, 5, "outside line 1"},
		{"character at end is allowed", "abc", 1, 4, ""},
		{"CJK character past line", "你好", 1, 4, "outside line 1"},
		{"surrogate character past line", "😀", 1, 4, "outside line 1"},
		{"emoji after ASCII past line", "a😀b", 1, 6, "outside line 1"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := ValidatePosition(testCase.text, testCase.line, testCase.character)
			if testCase.want == "" {
				if err != nil {
					t.Fatalf("ValidatePosition(%q, %d, %d) = %v, want success", testCase.text, testCase.line, testCase.character, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidatePosition(%q, %d, %d) succeeded, want %q", testCase.text, testCase.line, testCase.character, testCase.want)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("ValidatePosition error = %q, want %q", err.Error(), testCase.want)
			}
		})
	}
}

func TestDocumentLines(t *testing.T) {
	cases := []struct {
		text string
		want []string
	}{
		{"", []string{""}},
		{"a", []string{"a"}},
		{"a\n", []string{"a", ""}},
		{"a\r\nb\r\n", []string{"a", "b", ""}},
		{"a\rb", []string{"a", "b"}},
	}
	for _, testCase := range cases {
		got := documentLines(testCase.text)
		if len(got) != len(testCase.want) {
			t.Fatalf("documentLines(%q) = %q, want %q", testCase.text, got, testCase.want)
		}
		for index := range got {
			if got[index] != testCase.want[index] {
				t.Fatalf("documentLines(%q) = %q, want %q", testCase.text, got, testCase.want)
			}
		}
	}
}

func TestNormalizeOperation(t *testing.T) {
	valid := map[string]Operation{
		"hover":               OpHover,
		"HOVER":               OpHover,
		" definition ":        OpGoToDefinition,
		"goToDefinition":      OpGoToDefinition,
		"gotodefinition":      OpGoToDefinition,
		"goto_definition":     OpGoToDefinition,
		"findReferences":      OpFindReferences,
		"references":          OpFindReferences,
		"find_references":     OpFindReferences,
		"goToImplementation":  OpGoToImplementation,
		"implementation":      OpGoToImplementation,
		"goto_implementation": OpGoToImplementation,
	}
	for raw, want := range valid {
		got, err := NormalizeOperation(raw)
		if err != nil {
			t.Fatalf("NormalizeOperation(%q): %v", raw, err)
		}
		if got != want {
			t.Fatalf("NormalizeOperation(%q) = %q, want %q", raw, got, want)
		}
	}
	for _, raw := range []string{"", "   ", "rename", "lsp_references"} {
		if _, err := NormalizeOperation(raw); err == nil {
			t.Fatalf("NormalizeOperation(%q) succeeded, want an error", raw)
		}
	}
}

func TestOperationConstants(t *testing.T) {
	if OpGoToDefinition != "goToDefinition" || OpFindReferences != "findReferences" ||
		OpGoToImplementation != "goToImplementation" || OpHover != "hover" {
		t.Fatalf("canonical operation values changed: %q %q %q %q",
			OpGoToDefinition, OpFindReferences, OpGoToImplementation, OpHover)
	}
	if OpDefinition != OpGoToDefinition || OpReferences != OpFindReferences || OpImplementation != OpGoToImplementation {
		t.Fatalf("short operation aliases do not match the canonical constants")
	}
	if got := operationMethod(OpFindReferences); got != "textDocument/references" {
		t.Fatalf("operationMethod(references) = %q", got)
	}
	if got := operationMethod(OpGoToImplementation); got != "textDocument/implementation" {
		t.Fatalf("operationMethod(implementation) = %q", got)
	}
}
