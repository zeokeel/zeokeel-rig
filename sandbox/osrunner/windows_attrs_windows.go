//go:build windows

package osrunner

import (
	"fmt"
	"io/fs"
	"syscall"
)

// winProtectedEntrySkip 是保护子树遍历的生产跳过判定（Windows）。
//
// 用 GetFileAttributes 读 FILE_ATTRIBUTE_REPARSE_POINT：junction
// （mklink /J）与符号链接都带这一位，而 Go 的 fs.ModeSymlink 只对符号链接置位。
// 只看 ModeSymlink 会让 junction 被当成普通目录下钻，从而把 deny ACE 加到
// **工作区外**的真实对象上。
//
// 读不到属性时返回错误（fail closed）：保护遍历宁可让 helper 不 spawn，也不要
// 在"可能是重解析点"的对象上加 ACE。
func winProtectedEntrySkip(path string, entry fs.DirEntry) (bool, error) {
	pointer, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return false, fmt.Errorf("GetFileAttributes(%q): %w", path, err)
	}
	attributes, err := syscall.GetFileAttributes(pointer)
	if err != nil {
		return false, fmt.Errorf("GetFileAttributes(%q): %w", path, err)
	}
	return winEntryNeedsSkip(attributes, entry.Type()&fs.ModeSymlink != 0), nil
}
