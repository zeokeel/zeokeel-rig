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
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
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
	// 非 Windows 上 "" 不是分隔符：`.git\config` 会落成怪名字文件而不是写进 .git
	// ——与 write_file/edit_file 统一为拒绝。
	if err := toolutil.CheckPathSeparator(display); err != nil {
		return "", "", err
	}
	// 任何含 ".." 分量的路径一律拒绝（与 write_file 一致）。之前这里先 Clean，
	// `.git/../escape.txt` 会变成 `escape.txt` 并被创建，两个写工具行为相反。
	for _, component := range strings.Split(display, "/") {
		if component == ".." {
			return "", "", fmt.Errorf("path %q contains a %q segment; apply_patch does not accept relative traversal, write the plain path instead", display, "..")
		}
	}
	native := filepath.Clean(filepath.FromSlash(display))
	if native == ".." || strings.HasPrefix(native, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("path %q escapes the workspace root (%q is not allowed)", display, "..")
	}
	if native == "." {
		return "", "", fmt.Errorf("the workspace root itself cannot be modified by a patch: %q", display)
	}
	rel = filepath.ToSlash(native)
	// 目录链里的符号链接必须先解析：策略（deny glob / protected）、去重键与写入
	// 目标都必须对齐到**真实落点**，否则 `shared -> secrets` 这种目录链接能把
	// `secrets/**` 的 deny glob 绕过去（词法路径 root/shared/x 不匹配 glob，而实际
	// 写入落在 secrets/x）。绝对路径与真正的逃逸在这里就给出可读文案。
	resolved, linkErr := r.preflightLinks(rel)
	if linkErr != nil {
		return "", "", linkErr
	}
	rel = resolved

	abs = filepath.Join(r.root, filepath.FromSlash(rel))
	return rel, abs, nil
}

// preflightLinks 解析 rel 里**目录链**上的符号链接，返回真实落点的根相对路径。
//
// 为什么必须回填落点：`checkPolicyPath` 的 deny glob / protected 与 claim 的去重键
// 都是路径字符串匹配。词法路径 `root/shared/x` 不匹配 `secrets/**`，但经
// `shared -> secrets` 链接后实际写入的是 `secrets/x` —— 只做"检查"而不回填就是
// 一条策略绕过（见 TestAuditFSDenyGlobThroughDirLink）。
//
// 只解析到**倒数第二段**：最后一段若是符号链接，删除/移动要作用于链接本身
// （rm/mv 语义），更新要"写穿"链接（由 fsguard 的 WriteFileAtomic 完成），
// 策略校验与去重再由 finalLinkTarget 单独覆盖。这样两种语义不会互相污染。
//
// 解析轮数上限防止链接环；解析不出来的情况（悬空链接等）保持词法路径，交给
// fsguard 在真正 I/O 时判定。
func (r *rootFS) preflightLinks(rel string) (string, error) {
	const maxLinkHops = 40
	parts := strings.Split(rel, "/")
	hops := 0
	for index := 0; index < len(parts)-1; index++ {
		prefix := strings.Join(parts[:index+1], "/")
		info, statErr := r.guard.Lstat(guardPath(prefix))
		if statErr != nil {
			if isNotExist(statErr) {
				break // 从这里往后都不存在：没有链接可解
			}
			return "", fmt.Errorf("%s: %w", rel, statErr)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		hops++
		if hops > maxLinkHops {
			return "", fmt.Errorf("%s: too many levels of symbolic links", rel)
		}
		target, readErr := r.guard.Readlink(guardPath(prefix))
		if readErr != nil {
			return "", fmt.Errorf("%s: read symbolic link %q: %w", rel, prefix, readErr)
		}
		// 绝对目标分两种：真逃逸（给经典文案）与"指回根内但 os.Root 仍拒绝"
		// （os.Root 在系统调用层不接受绝对链接目标，实测 darwin/linux 都是
		// "path escapes from parent"）。
		if isAbsoluteAnyOS(target) {
			if _, inside := r.relFromLinkTarget(prefix, target); !inside {
				return "", fmt.Errorf("%s: resolves to %q which is outside the workspace root %q", rel, target, r.root)
			}
			return "", fmt.Errorf("%s: symbolic link %q points to the absolute path %q; only relative link targets inside the workspace root are supported", rel, prefix, target)
		}
		next, inside := r.relFromLinkTarget(prefix, target)
		if !inside {
			return "", fmt.Errorf("%s: resolves to %q which is outside the workspace root %q",
				rel, r.displayLinkTarget(prefix, target), r.root)
		}
		if next == prefix {
			break // 自引用：交给 fsguard 判定（会报错，且不会逃逸）
		}
		rest := parts[index+1:]
		parts = append(strings.Split(next, "/"), rest...)
		index = -1 // 目标本身可能又含链接：从头重新扫描
	}
	return strings.Join(parts, "/"), nil
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

// finalAbsoluteLink 报告 rel 的最后一段是否是"目标为绝对路径"的符号链接，并返回
// 那个目标。它只用于把 fsguard 的 ErrEscape 翻译成更可读的文案（最后一段不在
// preflightLinks 的解析范围内，因为删除/移动要作用于链接本身）。
func (r *rootFS) finalAbsoluteLink(rel string) (string, bool) {
	info, statErr := r.guard.Lstat(guardPath(rel))
	if statErr != nil || info.Mode()&os.ModeSymlink == 0 {
		return "", false
	}
	target, readErr := r.guard.Readlink(guardPath(rel))
	if readErr != nil || !isAbsoluteAnyOS(target) {
		return "", false
	}
	return target, true
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
