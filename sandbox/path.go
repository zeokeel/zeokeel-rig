package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ResolvePath 按策略校验 requested，并返回其真实绝对路径（在最长的已存在前缀上
// 解析符号链接）。
//
// 规则全部失败即拒绝（fail-closed）：
//   - 未配置任何根 -> 拒绝（无文件访问权限）；
//   - NUL 字节与任何 ".." 路径分量 -> 拒绝；
//   - 相对路径相对主根（第一个根）解析；
//   - 绝对路径必须落在某个根内；
//   - 解析后的路径（包括符号链接目标）必须仍停留在某个根内。
//
// 拒绝 glob（WithDeniedGlobs）在 DSH 对齐后**不再参与判定**：它只在策略里留下记录。
//
// 目标本身不必存在（write_file 会创建文件）；只解析已存在的祖先链，因此尚未创建
// 的文件也无法藏匿一个逃逸的符号链接。
func (p *Policy) ResolvePath(requested string) (string, error) {
	if err := p.modeDenial(); err != nil {
		return "", err
	}
	if p == nil || (len(p.roots) == 0 && !p.fullAccess()) {
		return "", deny("no roots configured")
	}
	requested = strings.TrimSpace(requested)
	if requested == "" {
		requested = "."
	}
	if strings.ContainsRune(requested, 0) {
		return "", deny("path contains a NUL byte")
	}
	if p.fullAccess() {
		return resolveFullAccessPath(p, requested)
	}
	if hasDotDotComponent(requested) {
		return "", deny("path %q contains a parent-directory component", requested)
	}

	var candidate string
	if filepath.IsAbs(requested) {
		candidate = filepath.Clean(requested)
	} else {
		candidate = filepath.Join(p.roots[0], filepath.FromSlash(requested))
	}
	resolved, err := resolveExistingPrefix(candidate)
	if err != nil {
		return "", deny("path %q: %v", requested, err)
	}
	resolved = filepath.Clean(resolved)
	if _, ok := p.containingRoot(resolved); !ok {
		return "", deny("path %q is outside the configured roots", requested)
	}
	return resolved, nil
}

// CheckPath 报告某个绝对路径是否被策略允许：它必须落在某个根内（danger-full-access
// 下不做根围栏）。它是工具实现对自己计算出的值（例如目录遍历得到的 glob 匹配结果）
// 所做的解析后检查。
//
// DSH 对齐后这里**不再**检查拒绝 glob。
func (p *Policy) CheckPath(abs string) error {
	if err := p.modeDenial(); err != nil {
		return err
	}
	if p == nil || (len(p.roots) == 0 && !p.fullAccess()) {
		return deny("no roots configured")
	}
	if abs == "" || !filepath.IsAbs(abs) {
		return deny("path %q is not absolute", abs)
	}
	if strings.ContainsRune(abs, 0) {
		return deny("path contains a NUL byte")
	}
	abs = filepath.Clean(abs)
	if p.fullAccess() {
		return nil
	}
	if _, ok := p.containingRoot(abs); !ok {
		return deny("path %q is outside the configured roots", abs)
	}
	return nil
}

// OpenPath 像 ResolvePath 一样解析 requested，并为包含它的根打开一个 *os.Root。
// 它返回打开的根、相对该根的路径（操作系统分隔符，请求根目录本身时为 "."）以及
// 真实绝对路径。调用方拥有返回的 *os.Root，必须关闭它。
//
// 工具应使用它，而不是自己拼接返回的绝对路径：这样所有 I/O 都经过 os.Root，
// 由 os.Root 在系统调用邻近层强制包含关系，即使文件系统在解析与使用之间发生变化，
// 也能拒绝逃逸的符号链接。
func (p *Policy) OpenPath(requested string) (root *os.Root, relative, abs, rootPath string, err error) {
	if err := p.modeDenial(); err != nil {
		return nil, "", "", "", err
	}
	if p == nil || (len(p.roots) == 0 && !p.fullAccess()) {
		return nil, "", "", "", deny("no roots configured")
	}
	abs, err = p.ResolvePath(requested)
	if err != nil {
		return nil, "", "", "", err
	}
	if p.fullAccess() {
		rootPath, relative, err = fullAccessRoot(abs)
		if err != nil {
			return nil, "", "", "", deny("path %q: %v", requested, err)
		}
		opened, err := os.OpenRoot(rootPath)
		if err != nil {
			return nil, "", "", "", deny("root %q is not accessible: %v", rootPath, err)
		}
		return opened, relative, abs, rootPath, nil
	}
	containing, ok := p.containingRoot(abs)
	if !ok {
		return nil, "", "", "", deny("path %q is outside the configured roots", requested)
	}
	rootPath = containing
	relative, err = filepath.Rel(rootPath, abs)
	if err != nil || escapesRelative(relative) {
		return nil, "", "", "", deny("path %q is outside the configured roots", requested)
	}
	opened, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, "", "", "", deny("root %q is not accessible: %v", rootPath, err)
	}
	return opened, relative, abs, rootPath, nil
}

// containingRoot 返回包含 abs 的最长已配置根。
func (p *Policy) containingRoot(abs string) (string, bool) {
	best := ""
	found := false
	for _, root := range p.roots {
		if !pathWithin(root, abs) {
			continue
		}
		if len(root) > len(best) {
			best = root
			found = true
		}
	}
	return best, found
}

// DeniedPath 报告 abs 在策略下是否不可见（位于所有根之外）。它是没有错误返回值的
// CheckPath，供过滤目录列表/glob/grep 结果的工具使用。
//
// DSH 对齐后它只反映根归属：拒绝 glob 不再让任何路径"不可见"。
func (p *Policy) DeniedPath(abs string) bool {
	return p.CheckPath(abs) != nil
}

// pathWithin 报告 path 是否等于 root 或位于其下。它是 sandbox.PathWithin 的内部
// 别名：**大小写语义随平台**（Windows/darwin 不敏感），不要在这里再写前缀比较。
func pathWithin(root, path string) bool { return PathWithin(root, path) }

// escapesRelative 报告一个已清理的相对路径是否指向其基准之外（与内置文件系统
// 工具的检查一致）。
func escapesRelative(relative string) bool {
	if relative == "" || filepath.IsAbs(relative) {
		return true
	}
	if relative == ".." {
		return true
	}
	return strings.HasPrefix(relative, ".."+string(filepath.Separator)) ||
		strings.HasPrefix(relative, "../")
}

// hasDotDotComponent 报告请求路径中是否有任何以分隔符切分的分量为 ".."。斜杠与
// 操作系统分隔符都会被考虑，因此调用方无法通过任一写法夹带目录穿越。
func hasDotDotComponent(requested string) bool {
	normalized := strings.ReplaceAll(requested, "/", string(filepath.Separator))
	for _, component := range strings.Split(normalized, string(filepath.Separator)) {
		if component == ".." {
			return true
		}
	}
	return false
}

// resolveExistingPrefix 用 EvalSymlinks 解析 path 最长的已存在祖先，并追加剩余
// 不存在的分量。这样尚未创建的文件也能被校验，同时仍能拒绝逃逸根目录的符号链接。
func resolveExistingPrefix(path string) (string, error) {
	remaining := make([]string, 0, 4)
	current := path
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			resolved = filepath.Clean(resolved)
			for i := len(remaining) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, remaining[i])
			}
			return resolved, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("no existing ancestor: %v", err)
		}
		remaining = append(remaining, filepath.Base(current))
		current = parent
	}
}

// resolveFullAccessPath 为 danger-full-access 解析 requested：没有根围栏、不拒绝
// 父目录分量，但仍然拒绝 NUL 字节。
func resolveFullAccessPath(p *Policy, requested string) (string, error) {
	var candidate string
	if filepath.IsAbs(requested) {
		candidate = filepath.Clean(requested)
	} else if len(p.roots) > 0 {
		candidate = filepath.Join(p.roots[0], filepath.FromSlash(requested))
	} else {
		abs, err := filepath.Abs(requested)
		if err != nil {
			return "", deny("path %q: %v", requested, err)
		}
		candidate = abs
	}
	resolved, err := resolveExistingPrefix(candidate)
	if err != nil {
		return "", deny("path %q: %v", requested, err)
	}
	return filepath.Clean(resolved), nil
}

// fullAccessRoot 返回包含 abs 的文件系统根以及 abs 相对它的路径，使 OpenPath 在
// full-access 模式下仍能使用 os.Root 的符号链接安全 I/O。
func fullAccessRoot(abs string) (rootPath, relative string, err error) {
	volume := filepath.VolumeName(abs)
	if volume == "" {
		rootPath = string(filepath.Separator)
	} else {
		rootPath = volume + string(filepath.Separator)
	}
	relative, err = filepath.Rel(rootPath, abs)
	if err != nil || escapesRelative(relative) {
		return "", "", fmt.Errorf("outside filesystem root")
	}
	return rootPath, relative, nil
}

// CheckWritablePath 报告 abs 是否可以**写入**：它必须通过 CheckPath（在根内），
// 策略不能是只读，且不能落在可写根内部的只读子路径（WritableRoot.ReadOnlySubpaths，
// 例如 .git/config 与 .git/hooks）之内。
//
// DSH 对齐后这里**不再**看保护路径（ProtectedPaths）与拒绝 glob：两者都不参与强制。
// 读路径不受只读子路径影响——读 .git/HEAD 是合法的——所以这个判定只给写工具
// （write_file/edit_file/apply_patch）在落盘前调用。
//
// 判定前会尽力把 abs 解析成真实路径（EvalSymlinks），因此"经符号链接目录写进只读
// 子路径"同样会被拒；目标还不存在时退而解析它的父目录。解析失败（例如父目录也不
// 存在）时按词法路径判定，不放大权限。
func (p *Policy) CheckWritablePath(abs string) error {
	if err := p.modeDenial(); err != nil {
		return err
	}
	if p.ReadOnly() {
		return deny("sandbox policy is read-only")
	}
	// 先解析成真实落点：`link -> .git/config`（或经目录链接）这类"词法路径看不出
	// 来"的写法，必须按**真实落点**判只读子路径。
	abs = resolveForProtection(abs)
	if err := p.CheckPath(abs); err != nil {
		return err
	}
	// 只读子路径与 AllowsWrite 用同一份判定（WritableRoots 里的 ReadOnlySubpaths），
	// 保证进程内围栏与 OS 后端渲染的规则同源。
	if !p.AllowsWrite(abs) {
		if denyPath, ok := p.denyAncestorOf(abs); ok {
			return deny("path %q is inside read-only subpath %q, which stays read-only", abs, denyPath)
		}
		return deny("path %q is not in a writable root", abs)
	}
	return nil
}

// denyAncestorOf 返回覆盖 abs 的只读子路径（可写根的 ReadOnlySubpaths；
// abs 本身或它的某个祖先）。
func (p *Policy) denyAncestorOf(abs string) (string, bool) {
	best := ""
	for _, denyPath := range p.readOnlySubpathsFlat() {
		if strings.TrimSpace(denyPath) == "" || !pathWithin(denyPath, abs) {
			continue
		}
		if len(denyPath) > len(best) {
			best = denyPath
		}
	}
	if best != "" {
		return best, true
	}
	return "", false
}

// resolveForProtection 尽力把路径解析成真实路径：先整条，再退回"父目录 + 基名"。
// 目标尚不存在（新建文件）时后者才可能成功；都失败就返回清理过的原路径。
func resolveForProtection(abs string) string {
	abs = filepath.Clean(abs)
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	if dir := filepath.Dir(abs); dir != abs {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(resolved, filepath.Base(abs))
		}
	}
	return abs
}
