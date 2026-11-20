package sandbox

import "strings"

// GlobMatch 用一个以 "/" 分隔的 glob 模式匹配同样以 "/" 分隔的相对路径。
//
// 支持的语法只有三种：
//   - `**` 作为一整段时匹配任意多段（含零段），例如 `**/.env` 匹配 `.env` 与
//     `a/b/.env`；
//   - `*` 与 `?` 在**单个路径段内**匹配（不跨 `/`）；
//   - 其余字符都是字面量，因此模式不会意外变成正则表达式。
//
// 它只在路径段边界上工作，不做绝对路径判定、不解析符号链接、不改大小写：
// 大小写折叠由调用方按平台决定（见 Policy.Hidden）。
//
// 导出的理由：宿主经常需要"和策略同一套语义"的匹配（例如自己筛一遍外部列表、
// 在 UI 里预览某个模式会命中什么）。v0.24.0 把私有匹配器随拒绝 glob 一起删掉后，
// 每个宿主只能各写一套，`**/`、段内通配与基名匹配的语义随即漂移。
func GlobMatch(pattern, rel string) bool {
	return matchGlobSegments(strings.Split(pattern, "/"), strings.Split(rel, "/"))
}

// matchGlobSegments 是实现 `**` 的递归匹配。
func matchGlobSegments(pattern, name []string) bool {
	if len(pattern) == 0 {
		return len(name) == 0
	}
	if pattern[0] == "**" {
		// `**` 吃掉 0..n 段。段数很少（路径深度），递归深度可接受。
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

// matchGlobSegment 在单个路径段内匹配 `*` 与 `?`（线性回溯，和 filepath.Match
// 的单段语义一致，但不解释 `[...]` 之类的扩展）。
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
