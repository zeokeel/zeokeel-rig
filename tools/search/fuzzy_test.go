package search

// 本文件固定 FuzzyScore 的语义与单调性：空查询不匹配、大小写不敏感、连续匹配加分、
// 词首加分、目录段匹配加分、路径越短越靠前、以及 `a/b` 形式的路径分段匹配。

import "testing"

func TestFuzzyScoreNonMatches(t *testing.T) {
	cases := []struct {
		name  string
		query string
		path  string
	}{
		{name: "empty query", query: "", path: "main.go"},
		{name: "blank query", query: "   ", path: "main.go"},
		{name: "empty path", query: "main", path: ""},
		{name: "out of order", query: "abc", path: "cba.go"},
		{name: "missing rune", query: "main", path: "mn.go"},
		{name: "query longer than path", query: "verylongquery", path: "a.go"},
		{name: "no matching segment", query: "src/deep", path: "src/search/x.go"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			score, matched := FuzzyScore(testCase.query, testCase.path)
			if matched {
				t.Fatalf("FuzzyScore(%q, %q) matched with score %d, want no match", testCase.query, testCase.path, score)
			}
			if score != 0 {
				t.Fatalf("FuzzyScore(%q, %q) score = %d, want 0 for a non-match", testCase.query, testCase.path, score)
			}
		})
	}
}

func TestFuzzyScoreMatchesSubsequence(t *testing.T) {
	cases := []struct {
		name  string
		query string
		path  string
	}{
		{name: "contiguous", query: "search", path: "src/search.go"},
		{name: "subsequence", query: "srch", path: "src/search.go"},
		{name: "case insensitive query", query: "SEARCH", path: "src/search.go"},
		{name: "case insensitive path", query: "search", path: "SRC/SEARCH.GO"},
		{name: "backslash path", query: "search", path: `src\search.go`},
		{name: "segment query", query: "src/search", path: "src/search/deep.go"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			score, matched := FuzzyScore(testCase.query, testCase.path)
			if !matched {
				t.Fatalf("FuzzyScore(%q, %q) did not match, want a match", testCase.query, testCase.path)
			}
			if score == 0 {
				t.Fatalf("FuzzyScore(%q, %q) score = 0 for a match, want a non-zero score", testCase.query, testCase.path)
			}
		})
	}
}

// TestFuzzyScoreConsecutiveBonus 固定"连续匹配比分散匹配分高"。
func TestFuzzyScoreConsecutiveBonus(t *testing.T) {
	contiguous, matched := FuzzyScore("abc", "abc.go")
	if !matched {
		t.Fatal("FuzzyScore(\"abc\", \"abc.go\") did not match")
	}
	scattered, matched := FuzzyScore("abc", "a/b/c.go")
	if !matched {
		t.Fatal("FuzzyScore(\"abc\", \"a/b/c.go\") did not match")
	}
	if contiguous <= scattered {
		t.Fatalf("contiguous score = %d, scattered score = %d; want the contiguous match to score higher", contiguous, scattered)
	}
}

// TestFuzzyScoreShorterPathWins 固定"同样匹配时路径越短越靠前"。
func TestFuzzyScoreShorterPathWins(t *testing.T) {
	short, matched := FuzzyScore("main", "main.go")
	if !matched {
		t.Fatal("FuzzyScore(\"main\", \"main.go\") did not match")
	}
	long, matched := FuzzyScore("main", "cmd/internal/deep/main.go")
	if !matched {
		t.Fatal("FuzzyScore(\"main\", \"cmd/internal/deep/main.go\") did not match")
	}
	if short <= long {
		t.Fatalf("short score = %d, long score = %d; want the shorter path to score higher", short, long)
	}
}

// TestFuzzyScoreWordStartBonus 固定"词首匹配比词中匹配分高"。
func TestFuzzyScoreWordStartBonus(t *testing.T) {
	// 两条路径等长、都命中文件名，唯一区别是匹配是否落在词首（a-b.go 的第二个
	// 字符跟在连字符后面）。连续匹配其实是偏向失败者的，所以这个比较更严格。
	wordStart, matched := FuzzyScore("ab", "a-b.go")
	if !matched {
		t.Fatal("FuzzyScore(\"ab\", \"a-b.go\") did not match")
	}
	midWord, matched := FuzzyScore("ab", "xab.go")
	if !matched {
		t.Fatal("FuzzyScore(\"ab\", \"xab.go\") did not match")
	}
	if wordStart <= midWord {
		t.Fatalf("word-start score = %d, mid-word score = %d; want the word-start match to score higher", wordStart, midWord)
	}
}

// TestFuzzyScoreDirectorySegmentBonus 固定"查询命中某个目录段"加分。
func TestFuzzyScoreDirectorySegmentBonus(t *testing.T) {
	segment, matched := FuzzyScore("tools", "tools/search/x.go")
	if !matched {
		t.Fatal("FuzzyScore(\"tools\", \"tools/search/x.go\") did not match")
	}
	basenameOnly, matched := FuzzyScore("tools", "internal/tools_name.go")
	if !matched {
		t.Fatal("FuzzyScore(\"tools\", \"internal/tools_name.go\") did not match")
	}
	if segment <= basenameOnly {
		t.Fatalf("directory-segment score = %d, basename-only score = %d; want the directory segment to win",
			segment, basenameOnly)
	}
}

// TestFuzzyScoreBasenameBonus 固定"命中落在文件名里比落在普通目录段里分高"。
//
// 与"目录段完全相等"分开测：完全相等的目录段有独立加分（TestFuzzyScoreDirectorySegmentBonus），
// 两者是不同的信号。
func TestFuzzyScoreBasenameBonus(t *testing.T) {
	basename, matched := FuzzyScore("zebra", "cmd/zebra_test.go")
	if !matched {
		t.Fatal("FuzzyScore(\"zebra\", \"cmd/zebra_test.go\") did not match")
	}
	directory, matched := FuzzyScore("zebra", "azebra/x.go")
	if !matched {
		t.Fatal("FuzzyScore(\"zebra\", \"azebra/x.go\") did not match")
	}
	if basename <= directory {
		t.Fatalf("basename score = %d, non-final-directory score = %d; want the basename match to win", basename, directory)
	}
}

// TestFuzzyScoreCaseInsensitivity 固定大小写不改变分数。
func TestFuzzyScoreCaseInsensitivity(t *testing.T) {
	lower, _ := FuzzyScore("search", "src/search.go")
	upper, _ := FuzzyScore("SEARCH", "SRC/SEARCH.GO")
	if lower != upper {
		t.Fatalf("case-folded scores differ: %d vs %d", lower, upper)
	}
}

// TestFuzzyScoreSegmentMatching 固定 `a/b` 形式的查询必须按路径段顺序匹配。
func TestFuzzyScoreSegmentMatching(t *testing.T) {
	segmented, matched := FuzzyScore("src/search", "src/search/deep.go")
	if !matched {
		t.Fatal("FuzzyScore(\"src/search\", \"src/search/deep.go\") did not match")
	}
	scattered, matched := FuzzyScore("src/search", "x/srcx/search.go")
	if !matched {
		t.Fatal("FuzzyScore(\"src/search\", \"x/srcx/search.go\") did not match")
	}
	if segmented <= scattered {
		t.Fatalf("exact-segment score = %d, approximate score = %d; want the exact segments to win",
			segmented, scattered)
	}
}
