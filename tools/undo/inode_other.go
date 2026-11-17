//go:build !unix

package undo

import "io/fs"

// inodeOf 在非 Unix 平台没有可移植的 inode 身份（Windows 用 FileID，但
// syscall.Win32FileAttributeData 不暴露它），因此硬链接检测关闭：返回 ok=false，
// 回退行为与引入硬链接识别之前完全一致。
func inodeOf(fs.FileInfo) (dev uint64, ino uint64, ok bool) {
	return 0, 0, false
}
