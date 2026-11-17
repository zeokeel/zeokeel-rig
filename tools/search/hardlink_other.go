//go:build !unix

package search

import "os"

// hardLinkCount 在非 Unix 平台上不可用。
//
// Windows 的硬链接信息不在 os.FileInfo 的通用字段里（需要额外打开文件句柄查
// FILE_STANDARD_INFO 或按文件 ID 查询），本包不为它引入平台专用调用：守卫在该
// 平台上退化为"不跳过"，这一点写在包文档的"已知边界"里。
func hardLinkCount(os.FileInfo) (uint64, bool) {
	return 0, false
}
