//go:build !windows

package osrunner

import "io/fs"

// winProtectedEntrySkip 是保护子树遍历在非 Windows 宿主上的退化实现：只有符号
// 链接可见（其余重解析点概念属于 Windows）。它保证同一段遍历逻辑能在 Linux 上
// 单测；Windows 上的真实判定见 windows_attrs_windows.go。
func winProtectedEntrySkip(_ string, entry fs.DirEntry) (bool, error) {
	return winEntryNeedsSkip(0, entry.Type()&fs.ModeSymlink != 0), nil
}
