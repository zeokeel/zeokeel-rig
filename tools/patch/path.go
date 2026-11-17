package patch

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/zeokeel/zeokeel-rig/internal/fsguard"
)

// 本文件是补丁路径的唯一入口：所有补丁路径都必须经过 root.resolve 才能参与
// 读写。校验分两层：
//
//  1. **快速预检**（不碰磁盘）：拒绝绝对路径（POSIX "/x" 与 Windows "C:\x"、
//     `\\server\share` 都算）、".."、以及工作区根本身（"."）。这一层负责给出
//     可读的错误文案。
//  2. **fsguard 兜底**（基于 os.Root 的遍历安全句柄）：真正决定"能不能碰"。
//     任何符号链接逃逸、TOCTOU 换目录、大小写/保留名之类的差异都由它按平台
//     语义处理，补丁层不再自己拼绝对路径做包含性判断。
//
// 之所以两层都要：预检的文案是面向模型的接口（"path X escapes the workspace root"），
// 而 os.Root 的错误更偏系统层；反过来只有预检又挡不住校验与落盘之间的竞态。

// rootFS 是一个已经打开的工作区根：root 是规范化后的绝对路径（只用于错误文案与
// 沙箱策略校验），guard 是真正执行 I/O 的遍历安全句柄。
type rootFS struct {
	root  string
	guard *fsguard.Root
}

// newRootFS 校验并打开工作区根。
//
// 说明：这里的 os.Stat / EvalSymlinks 只作用于**宿主传入的 root 自身**（构造期一次），
// 目的是保留"root 不存在/不是目录"这类友好文案；之后所有针对补丁路径的 I/O 都走
// guard。
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
	guard, err := fsguard.Open(resolved)
	if err != nil {
		return nil, fmt.Errorf("workspace root %q: %w", root, err)
	}
	return &rootFS{root: filepath.Clean(resolved), guard: guard}, nil
}

// Close 释放底层句柄（fsguard 基于 os.Root，持有目录文件描述符）。
func (r *rootFS) Close() {
	if r == nil || r.guard == nil {
		return
	}
	_ = r.guard.Close()
}

// resolve 把补丁里的路径解析成"根相对路径 + 绝对路径"，并做预检与符号链接预检。
//
// 返回的 rel 是相对根的斜杠形式路径（结果文案、去重键与 fsguard 调用都用它）；
// abs 只用于沙箱策略校验（policy 的根与 glob 都是绝对路径）。
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
	rel = filepath.ToSlash(native)
	abs = filepath.Join(r.root, native)

	// 符号链接预检：给出比 os.Root 更可读的文案（真正兜底在 guard 里）。
	if linkErr := r.preflightLinks(rel); linkErr != nil {
		return "", "", linkErr
	}
	return rel, abs, nil
}

// preflightLinks 逐段检查 rel 里的符号链接，一旦解析结果逃出根就报出可读错误。
//
// 它只负责"文案友好"：命中常见逃逸（绝对目标、目标里出现 ".." 逃逸）时立刻返回；
// 解析不出来的情况（悬空链接、链太深、循环）留给 fsguard 在真正 I/O 时判定。
// maxLinkHops 限制解析轮数，避免链接环把预检卡死。
func (r *rootFS) preflightLinks(rel string) error {
	const maxLinkHops = 32
	current := rel
	for hop := 0; hop < maxLinkHops; hop++ {
		linkRel, target, isLink, err := r.firstLink(current)
		if err != nil {
			return err
		}
		if !isLink {
			return nil
		}
		if isAbsoluteAnyOS(target) {
			// 绝对目标分两种：真逃逸（给经典文案）与"指回根内但 os.Root 仍拒绝"
			// （os.Root 在系统调用层不接受绝对链接目标，实测 darwin/linux 都是
			// "path escapes from parent"）。
			if _, inside := r.relFromLinkTarget(linkRel, target); !inside {
				return fmt.Errorf("%s: resolves to %q which is outside the workspace root %q", rel, target, r.root)
			}
			return fmt.Errorf("%s: symbolic link %q points to the absolute path %q; only relative link targets inside the workspace root are supported", rel, linkRel, target)
		}
		resolved, inside := r.relFromLinkTarget(linkRel, target)
		if !inside {
			return fmt.Errorf("%s: resolves to %q which is outside the workspace root %q",
				rel, r.displayLinkTarget(linkRel, target), r.root)
		}
		if resolved == linkRel {
			return nil // 自引用：交给 fsguard 判定（会报错，且不会逃逸）
		}
		current = resolved
	}
	return nil
}

// firstLink 返回 current 路径里**最靠前的**符号链接：linkRel 是它的根相对路径，
// target 是 Readlink 得到的原始目标。没有链接时 isLink 为 false。
func (r *rootFS) firstLink(current string) (linkRel, target string, isLink bool, err error) {
	segments := strings.Split(filepath.ToSlash(current), "/")
	for index := range segments {
		prefix := strings.Join(segments[:index+1], "/")
		info, statErr := r.guard.Lstat(guardPath(prefix))
		if statErr != nil {
			if isNotExist(statErr) {
				return "", "", false, nil // 从这里往后都不存在，没有链接可跟
			}
			return "", "", false, fmt.Errorf("%s: %w", current, statErr)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		linkTarget, readErr := r.guard.Readlink(guardPath(prefix))
		if readErr != nil {
			return "", "", false, fmt.Errorf("%s: read symbolic link: %w", prefix, readErr)
		}
		return prefix, linkTarget, true, nil
	}
	return "", "", false, nil
}

// finalLinkTarget 返回"写入时实际会落到哪个根相对路径"：若 rel 的最后一段是符号
// 链接，则返回链接目标（可多级，最多 maxLinkHops 跳）；不是链接或解析不出来时
// 原样返回 rel，第二个返回值为 false。
//
// 用途：① 同一物理文件写两次的去重键；② 让沙箱策略校验链接的真实落点
// （否则 `link -> .env` 能绕开 deny glob）。真正的"写穿链接"由 fsguard 的
// WriteFileAtomic 完成，这里只是估算落点做校验。
func (r *rootFS) finalLinkTarget(rel string) (string, bool) {
	const maxLinkHops = 8
	current := rel
	followed := false
	for hop := 0; hop < maxLinkHops; hop++ {
		info, err := r.guard.Lstat(guardPath(current))
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			return current, followed
		}
		target, err := r.guard.Readlink(guardPath(current))
		if err != nil {
			return current, followed
		}
		next, inside := r.relFromLinkTarget(current, target)
		if !inside || next == current {
			return current, followed
		}
		current = next
		followed = true
	}
	return current, followed
}

// relFromLinkTarget 把一条符号链接目标解析成根相对路径：相对目标按链接所在目录
// 解析，绝对目标必须在根内（例如 autotools 风格的绝对链接）。逃出根时 ok 为 false。
func (r *rootFS) relFromLinkTarget(linkRel, target string) (rel string, ok bool) {
	if isAbsoluteAnyOS(target) {
		absolute := filepath.Clean(filepath.FromSlash(target))
		relative, err := filepath.Rel(r.root, absolute)
		if err != nil || escapesRel(filepath.ToSlash(relative)) {
			return "", false
		}
		return filepath.ToSlash(relative), true
	}
	joined := path.Clean(path.Join(path.Dir(linkRel), filepath.ToSlash(target)))
	if escapesRel(joined) {
		return "", false
	}
	return joined, true
}

// displayLinkTarget 渲染错误文案里的链接目标（尽量给绝对路径，便于用户定位）。
func (r *rootFS) displayLinkTarget(linkRel, target string) string {
	if isAbsoluteAnyOS(target) {
		return target
	}
	return filepath.Join(r.root, filepath.FromSlash(path.Clean(path.Join(path.Dir(linkRel), filepath.ToSlash(target)))))
}

// escapesRel 报告根相对路径是否逃出根（".." 前缀或绝对形式）。
func escapesRel(rel string) bool {
	if rel == ".." {
		return true
	}
	return strings.HasPrefix(rel, "../") || filepath.IsAbs(rel)
}

// absFor 把根相对路径还原成绝对路径，只用于沙箱策略校验与错误文案。
func (r *rootFS) absFor(rel string) string {
	return filepath.Join(r.root, filepath.FromSlash(rel))
}

// runtimeGOOS 返回当前平台名（可被测试替换，见 osNameForPathSemantics）。
func runtimeGOOS() string { return runtime.GOOS }

// guardPath 把补丁层的斜杠路径转成 fsguard 期望的本机形式。
func guardPath(rel string) string { return filepath.FromSlash(rel) }

// isNotExist 把 fsguard 的"不存在"哨兵与 os 的错误统一成布尔判断。
func isNotExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, fsguard.ErrNotExist)
}

// isEscape 判断错误是否意味着"路径逃出了工作区根"。
func isEscape(err error) bool { return errors.Is(err, fsguard.ErrEscape) }

// isTooLarge 判断错误是否来自 ReadFileLimit 的大小上限。
func isTooLarge(err error) bool { return errors.Is(err, fsguard.ErrTooLarge) }

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

// caseInsensitivePathFS 报告当前平台的文件系统通常是否大小写不敏感。
//
// 这是一个**近似**：APFS/HFS+ 与 NTFS 的默认配置大小写不敏感，Linux 通常敏感，
// 但挂载参数可以改变这一点（例如 macOS 上的 case-sensitive APFS、Linux 上的
// vfat）。这里按平台给默认值，去重键据此归一化。
func caseInsensitivePathFS() bool {
	switch osNameForPathSemantics() {
	case "darwin", "windows":
		return true
	default:
		return false
	}
}

// osNameForPathSemantics 是 runtime.GOOS 的替身：真实取值固定，测试可以临时覆盖
// 以便在 Linux 上也能断言 darwin/windows 的分支（case-fold 去重的平台差异）。
var osNameForPathSemantics = runtimeGOOS

// dedupeKey 返回去重用的规范化键：大小写不敏感的平台统一转小写。
func dedupeKey(rel string) string {
	if caseInsensitivePathFS() {
		return strings.ToLower(rel)
	}
	return rel
}
