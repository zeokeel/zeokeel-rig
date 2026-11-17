//go:build unix

package undo

import (
	"io/fs"
	"syscall"
)

// inodeOf 从 FileInfo 里取出 (dev, ino)，用于识别"多个路径指向同一 inode"（硬链接）。
//
// Unix 上 syscall.Stat_t 提供 Dev/Ino；两者组合在同一台机器上唯一标识一个 inode。
func inodeOf(info fs.FileInfo) (dev uint64, ino uint64, ok bool) {
	if info == nil {
		return 0, 0, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return 0, 0, false
	}
	return uint64(stat.Dev), uint64(stat.Ino), true
}
