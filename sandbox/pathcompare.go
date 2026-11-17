package sandbox

// 路径相等/包含判定的**唯一**实现（平台语义感知）。
//
// 为什么要收敛：Windows 与 macOS 的默认文件系统（NTFS / APFS）大小写不敏感，
// `C:\w\.GIT\config` 与 `/w/.GIT/config` 就是 `.git/config` 本身。安全判定（根
// 归属、保护路径、授权路径、拒绝 glob）如果用 `strings.HasPrefix` 做纯字节比较，
// 攻击者只要换个大小写就能绕过 —— 而这类 bug 在 Linux 上永远测不出来。
//
// 因此全包（以及 sandbox/osrunner）只允许通过这里的三个函数做路径相等/包含判定：
//
//	PathWithin(root, path)  包含（含相等）
//	PathEqual(a, b)         相等
//	pathWithinOS/…OS        带显式平台语义的版本（测试用）
//
// pathOSName 是平台语义的注入点（默认 runtime.GOOS）：测试在 Linux 上把它切成
// "darwin"/"windows" 就能覆盖大小写不敏感分支，做法与 tools/patch 的
// osNameForPathSemantics 一致。

import (
	"path/filepath"
	"runtime"
	"strings"
)

// pathOSName 是"路径比较按哪个平台语义"的钩子。默认 runtime.GOOS；只有测试
// （以及需要显式指定平台语义的调用方）会改它。
var pathOSName = runtime.GOOS

// pathCaseInsensitive 报告该平台的文件系统是否默认大小写不敏感。
//
// macOS 的 APFS 默认大小写不敏感（也有用户格式化成大小写敏感的，但这里按默认
// 走，即"更严格"的方向：受保护路径在大小写变体下也保持受保护）。
func pathCaseInsensitive(goos string) bool {
	return goos == "windows" || goos == "darwin"
}

// foldPath 在大小写不敏感平台上把路径折叠成可比较形式。
//
// 用 ToLower 而不是 EqualFold：包含判定需要一个"可前缀比较"的规范形式，而
// EqualFold 是逐 rune 的等价关系，无法直接做前缀比较。对真实文件名的 ASCII
// 大小写，两者的结论一致。
func foldPath(goos, path string) string {
	if pathCaseInsensitive(goos) {
		return strings.ToLower(path)
	}
	return path
}

// PathEqual 报告两个路径是否指向同一位置（按当前平台语义大小写敏感/不敏感）。
func PathEqual(a, b string) bool {
	return pathEqualOS(pathOSName, a, b)
}

// pathEqualOS 是 PathEqual 的显式平台版本。
func pathEqualOS(goos, a, b string) bool {
	if a == "" || b == "" {
		return a == "" && b == ""
	}
	return foldPath(goos, filepath.Clean(a)) == foldPath(goos, filepath.Clean(b))
}

// PathWithin 报告 path 是否等于 root 或位于其下（按路径**分量**比较，因此
// "/a/bc" 不算在 "/a/b" 之下；按当前平台语义大小写敏感/不敏感）。
//
// 空 root 不包含任何路径：旧实现把 "" 当成 "/" 前缀，会让"没配置根"变成"包含
// 一切"，那是 fail-open。
func PathWithin(root, path string) bool {
	return pathWithinOS(pathOSName, root, path)
}

// pathWithinOS 是 PathWithin 的显式平台版本。
func pathWithinOS(goos, root, path string) bool {
	if root == "" || path == "" {
		return false
	}
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	foldedRoot := foldPath(goos, root)
	foldedPath := foldPath(goos, path)
	if foldedPath == foldedRoot {
		return true
	}
	separator := string(filepath.Separator)
	if foldedRoot == separator {
		return strings.HasPrefix(foldedPath, separator)
	}
	return strings.HasPrefix(foldedPath, foldedRoot+separator)
}

// relativeWithin 返回 path 相对 root 的、用于 glob 匹配的斜杠形式路径，并按平台
// 语义折叠大小写（拒绝 glob 在大小写不敏感平台上必须同样命中）。
//
// 第二个返回值报告 path 是否在 root 之内。
func relativeWithin(goos, root, path string) (string, bool) {
	if !pathWithinOS(goos, root, path) {
		return "", false
	}
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	foldedRoot := foldPath(goos, root)
	foldedPath := foldPath(goos, path)
	if foldedPath == foldedRoot {
		return ".", true
	}
	separator := string(filepath.Separator)
	relative := strings.TrimPrefix(foldedPath, strings.TrimSuffix(foldedRoot, separator)+separator)
	return filepath.ToSlash(relative), true
}
