//go:build !linux

package undo

import "io/fs"

// 非 Linux 平台没有可移植的标准库 xattr 接口（darwin 的 syscall 包未导出
// Setxattr/Getxattr，Windows 没有对等物），因此 cookie 降级为"不支持"：
// 根身份校验仍由 (dev,ino) 与"根必须是真实目录"两道检查完成。
const rootCookieAttr = ""

// setRootCookie 在 !linux 上不可用。
func setRootCookie(string, string) error { return fs.ErrInvalid }

// getRootCookie 在 !linux 上总是报告"没有 cookie"。
func getRootCookie(string) (string, error) { return "", fs.ErrNotExist }

// removeRootCookie 在 !linux 上是空操作。
func removeRootCookie(string) error { return nil }
