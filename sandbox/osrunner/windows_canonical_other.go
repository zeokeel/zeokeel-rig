//go:build !windows

package osrunner

import (
	"fmt"
	"path/filepath"
	"strings"
)

// winCanonicalPathForPlatform 是 Windows 规范化逻辑在非 Windows 宿主上的等价实现：
// 解析符号链接并 Clean。它让"授权面与保护面必须共用同一个规范化函数"这条不变量
// 能在 Linux 上被单测（Windows 真机实现见 windows_canonical_windows.go，那里还要
// 处理 8.3 短名与 `\\?\` 前缀）。
func winCanonicalPathForPlatform(path string) (string, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return "", fmt.Errorf("windows-acl: cannot canonicalize empty path")
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(trimmed))
	if err != nil {
		return "", err
	}
	return filepath.Clean(real), nil
}
