//go:build unix

package search

import (
	"os"
	"syscall"
)

// hardLinkCount 返回一个文件在**本文件系统内**的硬链接数。
//
// 有多个名字意味着同一份内容可以从工作区之外的路径读到（硬链接不经过路径解析，
// WalkDir 看不到它），因此 WithHardlinkGuard(true) 会跳过 Nlink > 1 的文件。
// 第二个返回值表示该平台是否能取到硬链接数。
func hardLinkCount(info os.FileInfo) (uint64, bool) {
	if info == nil {
		return 0, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return 0, false
	}
	return uint64(stat.Nlink), true
}
