package patch

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// 本文件是补丁路径的唯一入口：所有补丁路径都必须经过 root.resolve 才能参与
// 读写。规则（任何一条不满足都是硬错误，绝不"尽力而为"）：
//
//   - 不接受绝对路径（POSIX "/x" 与 Windows "C:\x" 都算）；
//   - 不接受 ".."：Clean 之后以 ".." 开头的路径直接拒绝；
//   - 不接受工作区根本身（"."）：根不能被补丁删除/替换；
//   - 符号链接必须留在根内：对"最深的已存在祖先"做 EvalSymlinks，解析结果仍在
//     根内才放行；目标本身是符号链接时按链接目标校验（写入会跟随链接）。
//
// 之所以对"最深的已存在祖先"求值而不是对整个路径求值：新增文件的路径可能整段
// 都还不存在，EvalSymlinks 会直接失败；但只要父目录链里没有逃逸的符号链接，
// 新建出来的文件就一定在根内。

// rootFS 是一个已经规范化的工作区根。
type rootFS struct {
	// root 是 EvalSymlinks 之后的绝对路径，所有包含性判断都以它为基准。
	root string
}

// newRootFS 校验并规范化工作区根。
func newRootFS(root string) (*rootFS, error) {
	trimmed := strings.TrimSpace(root)
	if trimmed == "" {
		return nil, errors.New("workspace root is empty")
	}
	absolute, err := filepath.Abs(trimmed)
	if err != nil {
		return nil, fmt.Errorf("workspace root %q: %w", root, err)
	}
	absolute = filepath.Clean(absolute)
	info, err := os.Stat(absolute)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("workspace root %q does not exist", root)
		}
		return nil, fmt.Errorf("workspace root %q: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workspace root %q is not a directory", root)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, fmt.Errorf("workspace root %q: resolve symlinks: %w", root, err)
	}
	return &rootFS{root: filepath.Clean(resolved)}, nil
}

// resolve 把补丁里的路径解析成根内的绝对路径。
//
// 返回的 rel 是相对根的斜杠形式路径（用于结果文案与去重），abs 是可以直接读写
// 的绝对路径。
func (r *rootFS) resolve(raw string) (rel string, abs string, err error) {
	display := strings.TrimSpace(raw)
	if display == "" {
		return "", "", errors.New("path is empty")
	}
	if isAbsoluteAnyOS(display) {
		return "", "", fmt.Errorf("absolute paths are not allowed: %q", display)
	}
	native := filepath.Clean(filepath.FromSlash(display))
	if native == ".." || strings.HasPrefix(native, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("path %q escapes the workspace root (%q is not allowed)", display, "..")
	}
	if native == "." {
		return "", "", fmt.Errorf("the workspace root itself cannot be modified by a patch: %q", display)
	}
	abs = filepath.Join(r.root, native)
	rel = filepath.ToSlash(native)

	// 找到最深的已存在祖先：它对整个写入目标的可达性负责。
	existing := abs
	for {
		if _, statErr := os.Lstat(existing); statErr == nil {
			break
		} else if !errors.Is(statErr, fs.ErrNotExist) {
			return "", "", fmt.Errorf("%s: %w", rel, statErr)
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			break
		}
		existing = parent
	}
	resolved, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", "", fmt.Errorf("%s: resolve symbolic links: %w", rel, err)
	}
	if !withinRoot(r.root, resolved) {
		return "", "", fmt.Errorf("%s: resolves to %q which is outside the workspace root %q", rel, resolved, r.root)
	}
	return rel, abs, nil
}

// followFinalLink 返回写入时应使用的最终路径：若 abs 本身是符号链接，则返回链接
// 目标（解析后的真实路径），第二个返回值为 true。
//
// 为什么必须显式跟随：落盘用的是 os.Rename(temp, 目标)，而 rename 不会跟随符号
// 链接——它会直接替换链接本身。文件工具（edit_file、WriteFile 语义）都是写穿链接
// 的，补丁工具必须一致，否则一个被链接的配置文件会在打补丁后变成普通文件，而真正
// 的目标文件保持旧内容。
//
// 安全性：resolve 已经对"最深的已存在祖先"（存在时就是 abs 自己）做过
// EvalSymlinks 与根内校验，因此这里解析出的目标一定在根内；仍然再校验一次。
func (r *rootFS) followFinalLink(abs string) (string, bool) {
	info, err := os.Lstat(abs)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return abs, false
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return abs, false
	}
	resolved = filepath.Clean(resolved)
	if !withinRoot(r.root, resolved) {
		return abs, false
	}
	return resolved, true
}

// isAbsoluteAnyOS 报告路径是否像绝对路径：POSIX 的 "/x" 与 Windows 的 "C:\x"、
// "C:/x"、`\\server\share` 都算。跨平台判断是刻意的——一份补丁可能在另一台机器上
// 生成，只按当前平台的 filepath.IsAbs 判断会漏掉 Windows 风格路径。
func isAbsoluteAnyOS(path string) bool {
	if filepath.IsAbs(path) {
		return true
	}
	if strings.HasPrefix(path, `\\`) || strings.HasPrefix(path, "//") {
		return true
	}
	if len(path) >= 2 && path[1] == ':' {
		if drive := path[0]; (drive >= 'a' && drive <= 'z') || (drive >= 'A' && drive <= 'Z') {
			return true
		}
	}
	return false
}

// withinRoot 报告 path 是否等于 root 或位于 root 之下（两者都应是规范的绝对路径）。
func withinRoot(root, path string) bool {
	if path == root {
		return true
	}
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	if relative == "." {
		return true
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
