package toolutil

// 路径分隔符的平台差异：POSIX 上 "\" 是**合法文件名字符**，不是分隔符。
//
// 模型（尤其是被 Windows 训练语料带的模型）会写 `dir\file.txt`。在 Linux/macOS 上
// 那会落盘成一个名字里真的带反斜杠的普通文件：既没写进 `dir/`，也绕开了所有基于
// "/" 的策略判定（保护路径 .git、拒绝 glob 等）——用户只会看到"文件写成功了但我
// 找不到"。统一拒绝，并在文案里直接给出正确写法。

import (
	"fmt"
	"runtime"
	"strings"
)

// pathSepOS 是"分隔符语义按哪个平台"的注入点（默认 runtime.GOOS），
// 便于在 Linux 上单测 Windows 分支（反之亦然）。
var pathSepOS = runtime.GOOS

// CheckPathSeparator 报告 path 是否用了当前平台不支持的分隔符。
//
// Windows 上 "\" 就是分隔符，直接放行；其余平台出现 "\" 一律拒绝。
func CheckPathSeparator(path string) error {
	if pathSepOS == "windows" {
		return nil
	}
	if !strings.ContainsRune(path, '\\') {
		return nil
	}
	return fmt.Errorf(`path %q contains a backslash, but this platform uses "/" as the path separator; write paths with "/" instead (for example "dir/file.txt")`, path)
}
