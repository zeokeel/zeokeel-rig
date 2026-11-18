package search

// 本文件是文件名模糊搜索的打分器（借鉴常见文件检索的诱导打分思路，但用
// 可解释的整数权重）：
//
//	score = 每个匹配字符           +10
//	      + 连续匹配（相邻字符）    +10 每个相邻对
//	      + 词首匹配（段首/边界）   +12 每个匹配字符
//	      + 命中落在文件名里        +20（匹配起点在最后一个路径段内）
//	      + 查询等于某个路径段      +30
//	      + 查询段在路径里连续      +20（多段查询，如 a/b）
//	      + 文件名以查询开头        +40（最强信号：文件名就叫这个）
//	      - 路径长度（按 rune）     -1 每个字符
//
// 分数只用于排序，绝对值没有语义；同一输入永远得到同一分数（测试依赖这一点）。

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// fuzzyPerCharBonus 是每个匹配字符的基础分。
	fuzzyPerCharBonus = 10
	// fuzzyConsecutiveBonus 是相邻字符连续匹配的额外分。
	fuzzyConsecutiveBonus = 10
	// fuzzyWordStartBonus 是匹配落在词首（路径段首、下划线/连字符/点/空格之后，
	// 或 camelCase 边界）的额外分。
	fuzzyWordStartBonus = 12
	// fuzzyBasenameBonus 是匹配起点落在文件名（最后一个路径段）内的额外分。
	fuzzyBasenameBonus = 20
	// fuzzySegmentExactBonus 是查询（或查询的某一段）与某个路径段完全相等的额外分。
	fuzzySegmentExactBonus = 30
	// fuzzySegmentChainBonus 是多段查询命中的路径段彼此相邻的额外分。
	fuzzySegmentChainBonus = 20
	// fuzzyBasenamePrefixBonus 是"文件名以查询开头"的额外分。
	//
	// 没有它会出现明显的排序反例：词首加成（+12/字符）高于连续加成（+10/字符），
	// 于是查询 `abc` 给出 `a_b_c.go`(78) 高于 `abc.go`(76)、`search` 给出
	// `s_e_a_r_c_h.go`(138) 高于 `search.go`(133)、`util` 给出
	// `util/src/other.go`(95) 高于 `src/util.go`(91)——"文件名就叫这个"反而排到后面。
	//
	// 刻意只认**前缀**而不是任意子串：任意的子串加成会让 "ab" 选中 `xab.go`(84)
	// 而压过 `a-b.go`(58)，直接推翻 fuzzy_test.go 里"词首比词中分高"的既有契约。
	// 前缀是 fuzzy 检索里最经典也最无争议的强信号。
	fuzzyBasenamePrefixBonus = 40
)

// FuzzyScore 返回文件名模糊匹配的 (score, matched)。
//
// 匹配是大小写不敏感的子序列匹配：查询的每个字符必须按顺序出现在路径里。
// 查询可以含 "/"（也接受 "\"），此时按路径段顺序匹配：每个查询段必须是某个路径段
// 的子序列，且后一个查询段必须落在更靠后的路径段里。空查询、空路径或不匹配返回
// (0, false)。
func FuzzyScore(query, pathText string) (int, bool) {
	normalizedQuery := normalizeFuzzyText(query)
	normalizedPath := normalizeFuzzyText(pathText)
	if normalizedQuery == "" || normalizedPath == "" {
		return 0, false
	}
	if strings.Contains(normalizedQuery, "/") {
		return fuzzySegmentScore(normalizedQuery, normalizedPath)
	}
	return fuzzySubsequenceScore(normalizedQuery, normalizedPath)
}

// normalizeFuzzyText 统一大小写与分隔符，并去掉路径开头的 "./"。
func normalizeFuzzyText(value string) string {
	normalized := strings.ReplaceAll(strings.TrimSpace(value), `\`, "/")
	normalized = strings.TrimPrefix(normalized, "./")
	return strings.ToLower(normalized)
}

// fuzzySubsequenceScore 对单段查询打分。
func fuzzySubsequenceScore(query, target string) (int, bool) {
	positions, ok := subsequencePositions(query, target)
	if !ok {
		return 0, false
	}
	score := fuzzyPerCharBonus * utf8.RuneCountInString(query)
	for index := 1; index < len(positions); index++ {
		if positions[index] == positions[index-1]+1 {
			score += fuzzyConsecutiveBonus
		}
	}
	for _, position := range positions {
		if isFuzzyWordStart(target, position) {
			score += fuzzyWordStartBonus
		}
	}
	baseStart := strings.LastIndex(target, "/") + 1
	if positions[0] >= baseStart {
		score += fuzzyBasenameBonus
	}
	// 文件名以查询开头：最强的相关性信号（"文件名就叫这个"）。
	if strings.HasPrefix(target[baseStart:], query) {
		score += fuzzyBasenamePrefixBonus
	}
	if exactSegmentIndex(target, query) >= 0 {
		score += fuzzySegmentExactBonus
	}
	score -= utf8.RuneCountInString(target)
	return score, true
}

// fuzzySegmentScore 对 "a/b" 形式的多段查询打分。
func fuzzySegmentScore(query, target string) (int, bool) {
	querySegments := splitPathSegments(query)
	targetSegments := splitPathSegments(target)
	if len(querySegments) == 0 {
		return 0, false
	}
	cursor := 0
	score := 0
	matchedSegments := make([]int, 0, len(querySegments))
	for _, segment := range querySegments {
		found := -1
		for index := cursor; index < len(targetSegments); index++ {
			if isSubsequence(segment, targetSegments[index]) {
				found = index
				break
			}
		}
		if found < 0 {
			return 0, false
		}
		score += fuzzyPerCharBonus * utf8.RuneCountInString(segment)
		if targetSegments[found] == segment {
			score += fuzzySegmentExactBonus
		}
		if found == len(targetSegments)-1 {
			score += fuzzyBasenameBonus
		}
		positions, _ := subsequencePositions(segment, targetSegments[found])
		for index := 1; index < len(positions); index++ {
			if positions[index] == positions[index-1]+1 {
				score += fuzzyConsecutiveBonus
			}
		}
		for _, position := range positions {
			if isFuzzyWordStart(targetSegments[found], position) {
				score += fuzzyWordStartBonus
			}
		}
		matchedSegments = append(matchedSegments, found)
		cursor = found + 1
	}
	if len(matchedSegments) > 1 {
		chained := true
		for index := 1; index < len(matchedSegments); index++ {
			if matchedSegments[index] != matchedSegments[index-1]+1 {
				chained = false
				break
			}
		}
		if chained {
			score += fuzzySegmentChainBonus
		}
	}
	score -= utf8.RuneCountInString(target)
	return score, true
}

// subsequencePositions 返回查询每个字符在目标里的位置（贪心取最靠前的匹配）。
func subsequencePositions(query, target string) ([]int, bool) {
	queryRunes := []rune(query)
	targetRunes := []rune(target)
	positions := make([]int, 0, len(queryRunes))
	cursor := 0
	for _, queryRune := range queryRunes {
		found := -1
		for index := cursor; index < len(targetRunes); index++ {
			if targetRunes[index] == queryRune {
				found = index
				break
			}
		}
		if found < 0 {
			return nil, false
		}
		positions = append(positions, found)
		cursor = found + 1
	}
	return positions, true
}

// isSubsequence 报告 needle 是否是 haystack 的子序列。
func isSubsequence(needle, haystack string) bool {
	_, ok := subsequencePositions(needle, haystack)
	return ok
}

// splitPathSegments 按 "/" 切分并丢弃空段（"a//b" 与 "a/b" 等价）。
func splitPathSegments(value string) []string {
	rawSegments := strings.Split(value, "/")
	segments := make([]string, 0, len(rawSegments))
	for _, segment := range rawSegments {
		if segment == "" || segment == "." {
			continue
		}
		segments = append(segments, segment)
	}
	return segments
}

// isFuzzyWordStart 报告 target 的第 position 个 rune 是否是词首。
func isFuzzyWordStart(target string, position int) bool {
	runes := []rune(target)
	if position <= 0 || position >= len(runes) {
		return position == 0
	}
	previous := runes[position-1]
	if previous == '/' || previous == '_' || previous == '-' || previous == '.' || unicode.IsSpace(previous) {
		return true
	}
	// camelCase 边界：aB。
	return unicode.IsLower(previous) && unicode.IsUpper(runes[position])
}

// exactSegmentIndex 返回与 query 完全相等的路径段下标（没有则 -1）。
func exactSegmentIndex(target, query string) int {
	for index, segment := range splitPathSegments(target) {
		if segment == query {
			return index
		}
	}
	return -1
}
