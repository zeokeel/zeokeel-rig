package undo

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// resolveRoot 校验并规范化工作区根：绝对化、必须是存在的目录、解析符号链接。
// 之后的包含性判断都以解析后的真实路径为基准。
func resolveRoot(root string) (string, error) {
	trimmed := strings.TrimSpace(root)
	if trimmed == "" {
		return "", errors.New("undo: workspace root is empty")
	}
	absolute, err := filepath.Abs(trimmed)
	if err != nil {
		return "", fmt.Errorf("undo: workspace root %q: %w", root, err)
	}
	absolute = filepath.Clean(absolute)
	info, err := os.Stat(absolute)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("undo: workspace root %q does not exist", root)
		}
		return "", fmt.Errorf("undo: workspace root %q: %w", root, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("undo: workspace root %q is not a directory", root)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("undo: workspace root %q: resolve symlinks: %w", root, err)
	}
	return filepath.Clean(resolved), nil
}

// resolvePath 把工具/API 给的过滤路径解析成"相对工作区根的斜杠路径"。
//
// 这里只做**词法**校验（任何一条不满足都是硬错误）：
//   - 空路径、绝对路径（POSIX 与 Windows 风格都算）、"." （根本身）拒绝；
//   - ".." 逃逸拒绝。
//
// 符号链接逃逸不再由这里判断：实际访问全部经 internal/fsguard 的根句柄，内核会在
// 系统调用时拒绝任何逃逸（含父目录在检查后被换成指向根外的链接），因此错误的
// 判定点从"字符串检查"移到了"真正访问时"，中间不留 TOCTOU 窗口。
func (s *Snapshot) resolvePath(raw string) (string, error) {
	display := strings.TrimSpace(raw)
	if display == "" {
		return "", errors.New("undo: path is empty")
	}
	if isAbsoluteAnyOS(display) {
		return "", fmt.Errorf("undo: absolute paths are not allowed: %q", display)
	}
	native := filepath.Clean(filepath.FromSlash(display))
	if native == ".." || strings.HasPrefix(native, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("undo: path %q escapes the workspace root", display)
	}
	if native == "." {
		return "", fmt.Errorf("undo: the workspace root itself cannot be reverted: %q", display)
	}
	return filepath.ToSlash(native), nil
}

// isAbsoluteAnyOS 报告路径是否像绝对路径：POSIX 的 "/x"，Windows 的 "C:\x"、"C:/x"
// 与 UNC 的 `\\server\share` 都算。跨平台判断是刻意的——工作区里的路径可能来自另一台
// 机器生成的记录。
func isAbsoluteAnyOS(path string) bool {
	if filepath.IsAbs(path) {
		return true
	}
	if strings.HasPrefix(path, `\\`) || strings.HasPrefix(path, "//") {
		return true
	}
	if len(path) >= 2 && path[1] == ':' {
		drive := path[0]
		if (drive >= 'a' && drive <= 'z') || (drive >= 'A' && drive <= 'Z') {
			return true
		}
	}
	return false
}
