//go:build linux

package undo

import (
	"errors"
	"io/fs"
	"strings"
	"syscall"
)

// rootCookieAttr 是记录在根目录 **inode** 上的扩展属性名。
//
// 用 xattr 而不是标记文件，是因为它对工作区"不可见"：不产生目录项（宿主/模型/测试
// 的 `os.ReadDir` 都看不到）、不进快照、不会被 undo 判断成变更或删除。
const rootCookieAttr = "user." + rootCookieName

// setRootCookie 在 dir 上写入 cookie（flags=0，已存在则覆盖）。
//
// 返回错误时调用方降级为 (dev,ino) 校验：不能因为"文件系统不支持 xattr"就让 Take
// 失败（只读/网络文件系统上 undo 仍然要能拍快照）。
func setRootCookie(dir, token string) error {
	return syscall.Setxattr(dir, rootCookieAttr, []byte(token), 0)
}

// getRootCookie 读取 dir 上的 cookie。
//
// 不存在（含文件系统不支持）时返回 fs.ErrNotExist：对调用方来说"读不到 cookie"
// 与"根被换成了没有 cookie 的新目录"是同一件事——都不可信。
func getRootCookie(dir string) (string, error) {
	buffer := make([]byte, 1024)
	size, err := syscall.Getxattr(dir, rootCookieAttr, buffer)
	if err != nil {
		if errors.Is(err, syscall.ENODATA) || errors.Is(err, syscall.ENOTSUP) ||
			errors.Is(err, syscall.EOPNOTSUPP) {
			return "", fs.ErrNotExist
		}
		return "", err
	}
	if size <= 0 || size > len(buffer) {
		return "", fs.ErrNotExist
	}
	return strings.TrimSpace(string(buffer[:size])), nil
}

// removeRootCookie 删除 cookie（Take 失败时清理；不存在视为成功）。
func removeRootCookie(dir string) error {
	err := syscall.Removexattr(dir, rootCookieAttr)
	if err != nil && (errors.Is(err, syscall.ENODATA) || errors.Is(err, syscall.ENOTSUP) ||
		errors.Is(err, syscall.EOPNOTSUPP)) {
		return nil
	}
	return err
}
