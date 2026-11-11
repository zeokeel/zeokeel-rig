//go:build !windows

package terminal

import "errors"

// fallbackShellPath 只在 Windows 上提供 System32 兜底；其他平台返回错误，
// 由调用方报告原始 LookPath 失败。
func fallbackShellPath(name string) (string, error) {
	return "", errors.New("terminal: no platform shell fallback for " + name)
}
