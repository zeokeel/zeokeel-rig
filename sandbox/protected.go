package sandbox

import (
	"os"
	"path/filepath"
	"strings"
)

// 工作区元数据保护（**已停用强制**）。
//
// 历史背景：`.git`、`.aws`、`.codex` 这类目录承载版本控制状态与凭据，被约束的
// agent 修改它们等于改写自己的守卫，所以旧版本会把它们登记成"保护路径"，让写入
// 判定拒绝。
//
// Deprecated: DSH 对齐后策略层只做可写根围栏，**保护路径不再参与任何强制**：
// AddProtectedPaths / WithProtectedPaths 只记录、ProtectedPaths 返回空、
// WriteDenyPaths 恒空。本文件里的路径计算函数（WorkspaceMetadataPaths 等）保留
// 导出，因为宿主/诊断可能仍想枚举这些位置；但把结果交给策略不会再挡住任何写入。
//
// 仍然生效的"根内只读"只有一种实现：WritableRoot.ReadOnlySubpaths（见
// writable.go 与 gitpolicy.go，例如 .git/config、.git/hooks）。

// workspaceMetadataNames 是被默认保护（现已停用强制）的工作区元数据目录名。
var workspaceMetadataNames = []string{".git", ".aws", ".codex"}

// WorkspaceMetadataPaths 返回某个工作区根下的元数据路径（已规范化、去重、保留
// 不存在的项）。`.git` 是文件（worktree / submodule）时，除了指针文件本身，还会
// 解析 `gitdir:` 指向的真实目录。
//
// Deprecated: 返回值不再具有任何强制语义（DSH 对齐后没有"保护路径"）；它只是
// 一个路径枚举工具，宿主可用于诊断或自行决定如何处理。
func WorkspaceMetadataPaths(root string) []string {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil
	}
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	var out []string
	for _, name := range workspaceMetadataNames {
		candidate := filepath.Join(root, name)
		info, err := os.Lstat(candidate)
		if err != nil {
			// 不存在也保留：它是"将来可能出现"的位置。
			out = append(out, candidate)
			continue
		}
		if info.IsDir() || name != ".git" {
			out = append(out, candidate)
			continue
		}
		// .git 是文件：worktree / submodule 的指针。
		out = append(out, candidate)
		if target := readGitDirPointer(candidate); target != "" {
			out = append(out, target)
		}
	}
	return dedupeStrings(out)
}

// WorkspaceNonGitMetadataPaths 返回工作区里**不属于 git** 的元数据保护项
// （`.aws`、`.codex`）。
//
// 它给默认保护用：`.git` 不再整体保护（那会让 `git commit` 在沙箱里做不到），
// 改由 ProtectGitMetadata 按"数据面可写、控制面只读"登记（见 sandbox/gitpolicy.go）。
func WorkspaceNonGitMetadataPaths(root string) []string {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil
	}
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	out := make([]string, 0, len(workspaceMetadataNames))
	for _, name := range workspaceMetadataNames {
		if name == ".git" {
			continue
		}
		out = append(out, filepath.Join(root, name))
	}
	return out
}

// readGitDirPointer 读取 `.git` 文件里的 `gitdir: <path>`，返回规范化的目标目录
// （相对路径按 `.git` 所在目录解析）。读取失败或格式不符时返回空串。
func readGitDirPointer(gitFile string) string {
	data, err := os.ReadFile(gitFile)
	if err != nil {
		return ""
	}
	text := strings.TrimSpace(string(data))
	const prefix = "gitdir:"
	if !strings.HasPrefix(text, prefix) {
		return ""
	}
	target := strings.TrimSpace(strings.TrimPrefix(text, prefix))
	if target == "" {
		return ""
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(gitFile), target)
	}
	if abs, err := filepath.Abs(target); err == nil {
		target = abs
	}
	target = filepath.Clean(target)
	if real, err := filepath.EvalSymlinks(target); err == nil {
		target = filepath.Clean(real)
	}
	return target
}

// AddProtectedPaths 记录一组保护路径（相对路径按主根解析）。
//
// Deprecated: DSH 对齐后保护路径**不再参与任何强制**——本方法只把这些路径记进
// 策略（仅供诊断/回归观察），不会让 CheckWritablePath / AllowsWrite 拒绝任何写入，
// ProtectedPaths 也恒返回空。它由 tools/full 与 kit 的 `ProtectWorkspaceMetadata`
// 通路调用；保留它是为了让那些调用点继续编译，而不是继续生效。
func (p *Policy) AddProtectedPaths(paths ...string) {
	if p == nil || len(paths) == 0 {
		return
	}
	added := normalizeProtectedPaths(p.roots, paths)
	if len(added) == 0 {
		return
	}
	p.protectedMu.Lock()
	defer p.protectedMu.Unlock()
	existing := make(map[string]struct{}, len(p.protectedPaths))
	for _, path := range p.protectedPaths {
		existing[path] = struct{}{}
	}
	for _, path := range added {
		if _, ok := existing[path]; ok {
			continue
		}
		existing[path] = struct{}{}
		p.protectedPaths = append(p.protectedPaths, path)
	}
}

// WithProtectMissingPaths 控制"保护路径当前不存在"时的行为（**默认 true**）。
//
// true（默认，fail closed）：runner 无法渲染不存在的保护路径时，`Confine` 返回
// 错误而不是静默放行。触发条件是 `Profile.MissingProtected` 非空——即策略声明了
// 保护、但那个位置此刻还不存在（典型：工作区还没 `git init`，`.git/config` 还不
// 存在）。放任它继续，沙箱里新建出来的 `.git` 在本次运行里**完全不受保护**：
// "我开了保护、工具也说保护了 .git"与事实不符，属于保护承诺失效。
//
// false：保持"只报告"——不存在的项只出现在 `Profile.MissingProtected` 与诊断里，
// 命令照常执行。适合"我知道工作区还没有 .git，等它出现再保护"的宿主。
//
// 注意：Seatbelt（macOS）**不受本开关影响**——SBPL 的 subpath 规则按访问时求值，
// 不存在也能渲染成 deny 规则，所以它既不需要 fail closed 也不需要放行。
func WithProtectMissingPaths(enabled bool) Option {
	return func(policy *Policy) { policy.protectMissing = enabled }
}

// ProtectMissingPaths 报告当前策略是否要求"保护路径缺失即拒绝执行"。
func (p *Policy) ProtectMissingPaths() bool {
	if p == nil {
		return false
	}
	return p.protectMissing
}
