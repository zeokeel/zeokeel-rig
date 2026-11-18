package osrunner

// 本文件提供"按**显式**平台语义比较路径"的纯函数（不碰文件系统），供计划层与
// 跨平台测试在任意宿主上断言 Windows/macOS 的大小写不敏感语义。
//
// 为什么需要它：`profiles.go` 的 pathWithin 走 sandbox.PathWithin，用的是**当前
// 宿主**语义（runtime.GOOS）；而 Linux 上的测试想断言"在 Windows 语义下这个
// 路径是否落在那个根之下"（例如 Linux 侧工具链审计要排除 bwrap 私有 tmpfs 会遮
// 住的候选）。sandbox 包里的等价函数是未导出的 pathWithinOS，因此这里保留一份
// 同语义的包内实现（不导出），避免为测试去扩大 sandbox 的公开面。
//
// 与 sandbox.pathWithinOS 的语义保持一致：
//   - 按路径**分量**比较（"/a/bc" 不在 "/a/b" 之下）；
//   - windows/darwin 折叠大小写，其余平台敏感；
//   - 空 root 不包含任何路径（fail closed，不把 "" 当作 "/"）；
//   - 分隔符统一按 "/" 处理（Windows 写法里的 `\` 视作分隔符）。

import (
	"path"
	"strings"
)

// pathWithinOS 报告 candidate 是否等于 root 或位于其下，按 goos 指定的平台语义。
func pathWithinOS(goos, root, candidate string) bool {
	normalize := func(value string) string {
		value = strings.ReplaceAll(value, `\`, "/")
		value = path.Clean(value)
		if goos == "windows" || goos == "darwin" {
			value = strings.ToLower(value)
		}
		return value
	}
	root = normalize(root)
	candidate = normalize(candidate)
	if root == "" || candidate == "" {
		return false
	}
	if root == "." || candidate == "." {
		return root == candidate
	}
	if candidate == root {
		return true
	}
	if root == "/" {
		return strings.HasPrefix(candidate, "/")
	}
	return strings.HasPrefix(candidate, root+"/")
}
