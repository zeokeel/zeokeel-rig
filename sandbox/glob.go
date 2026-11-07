package sandbox

import "strings"

// matchGlob 用斜杠分隔的 glob 模式匹配斜杠分隔的路径。支持的通配符："**" 匹配
// 任意多段，"*" 与 "?" 在单个路径段内匹配。其余字符都是字面量，因此模式不会
// 意外变成正则表达式。
func matchGlob(pattern, name string) bool {
	return matchGlobSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchGlobSegments(pattern, name []string) bool {
	if len(pattern) == 0 {
		return len(name) == 0
	}
	if pattern[0] == "**" {
		for index := 0; index <= len(name); index++ {
			if matchGlobSegments(pattern[1:], name[index:]) {
				return true
			}
		}
		return false
	}
	if len(name) == 0 {
		return false
	}
	if !matchGlobSegment(pattern[0], name[0]) {
		return false
	}
	return matchGlobSegments(pattern[1:], name[1:])
}

func matchGlobSegment(pattern, name string) bool {
	patternIndex, nameIndex := 0, 0
	starIndex, starName := -1, 0
	for nameIndex < len(name) {
		switch {
		case patternIndex < len(pattern) &&
			(pattern[patternIndex] == name[nameIndex] || pattern[patternIndex] == '?'):
			patternIndex++
			nameIndex++
		case patternIndex < len(pattern) && pattern[patternIndex] == '*':
			starIndex = patternIndex
			starName = nameIndex
			patternIndex++
		case starIndex >= 0:
			patternIndex = starIndex + 1
			starName++
			nameIndex = starName
		default:
			return false
		}
	}
	for patternIndex < len(pattern) && pattern[patternIndex] == '*' {
		patternIndex++
	}
	return patternIndex == len(pattern)
}
