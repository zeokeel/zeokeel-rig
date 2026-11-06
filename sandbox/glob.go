package sandbox

import "strings"

// matchGlob matches a slash-separated glob pattern against a slash-separated
// path. Supported wildcards: "**" matches any number of segments, "*" and "?"
// match within one segment. Everything else is literal, so a pattern cannot
// accidentally become a regular expression.
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
