package memsearch

import (
	"reflect"
	"testing"
)

func TestTokenizeASCIILowercasesAndDropsPunctuation(t *testing.T) {
	got := Tokenize("Hello, World! 123")
	want := []string{"hello", "world", "123"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Tokenize = %#v, want %#v", got, want)
	}
}

func TestTokenizeCJKBigramsAndMixedScript(t *testing.T) {
	got := Tokenize("使用PostgreSQL做存储")
	want := []string{"使用", "postgresql", "做存", "存储"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Tokenize = %#v, want %#v", got, want)
	}
}

func TestTokenizeCJKSingleCharSurvives(t *testing.T) {
	if got := Tokenize("我"); !reflect.DeepEqual(got, []string{"我"}) {
		t.Fatalf("Tokenize(single) = %#v, want [\"我\"]", got)
	}
	got := Tokenize("改 部署")
	want := []string{"改", "部署"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Tokenize = %#v, want %#v", got, want)
	}
}

func TestTokenizeKeepsIdentifiersWholeAndSplitsThem(t *testing.T) {
	got := Tokenize("user_name.v2")
	want := []string{"user_name.v2", "user", "name", "v2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Tokenize = %#v, want %#v", got, want)
	}

	got = Tokenize("internal/memsearch")
	want = []string{"internal", "memsearch"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Tokenize = %#v, want %#v", got, want)
	}
}

func TestTokenizeTrimsSeparatorEdges(t *testing.T) {
	// 句末句点、开头的下划线都不应成为词项的一部分。
	got := Tokenize("调用 store.Search().")
	want := []string{"调用", "store.search", "store", "search"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Tokenize = %#v, want %#v", got, want)
	}
}

func TestTokenizeDropsPurePunctuation(t *testing.T) {
	for _, text := range []string{"", "   ", "!!!", "??? ,,, ...", "……", "-> <-", "++"} {
		if got := Tokenize(text); len(got) != 0 {
			t.Fatalf("Tokenize(%q) = %#v, want no tokens", text, got)
		}
	}
}

func TestTokenizeSplitsOperators(t *testing.T) {
	got := Tokenize("C++ and Go")
	want := []string{"c", "and", "go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Tokenize = %#v, want %#v", got, want)
	}
}

func TestTokenizeKeepsTermFrequencyDuplicates(t *testing.T) {
	got := Tokenize("cache cache cache")
	if len(got) != 3 {
		t.Fatalf("Tokenize = %#v, want three tokens", got)
	}
}

func TestTokenizeIsSharedByQueryAndIndex(t *testing.T) {
	// 查询用的切分与索引用的切分必须完全一致，否则中文二元组无法召回。
	doc := Tokenize("部署策略是蓝绿发布")
	query := Tokenize("蓝绿发布")
	seen := make(map[string]bool, len(doc))
	for _, token := range doc {
		seen[token] = true
	}
	for _, token := range query {
		if !seen[token] {
			t.Fatalf("query token %q missing from doc tokens %#v", token, doc)
		}
	}
}

func TestTokenizeKanaAndHangul(t *testing.T) {
	got := Tokenize("テスト")
	want := []string{"テス", "スト"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Tokenize = %#v, want %#v", got, want)
	}
	if got := Tokenize("한국"); !reflect.DeepEqual(got, []string{"한국"}) {
		t.Fatalf("Tokenize = %#v, want [\"한국\"]", got)
	}
}
