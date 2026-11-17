//go:build windows

package fsguard

// nonBlockFlag 在 Windows 上没有对应的打开标志（FIFO 不是普通文件系统对象，
// 命名管道由另一套 Win32 API 提供），因此为 0；OpenRegular 仍会用 fstat 判断类型。
const nonBlockFlag = 0
