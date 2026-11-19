package sandbox

import (
	"os"
	"path/filepath"
	"strings"
)

// `.git` 的写授权策略：大部分可写，危险子路径只读。
//
// 为什么不能整体保护：`git commit` 要写 `index`、`objects/**`、`refs/**`、`logs/**`，
// `git checkout` 还要写 `HEAD`、`ORIG_HEAD`。以前 `.git` 整个进 ProtectedPaths，
// 结果是"帮我提交"这个个人 agent 的核心任务在沙箱里根本做不到（实测三平台一致）。
//
// 为什么不能整体放开：`.git` 里有两类东西一旦可写就等于把围栏交给被约束的进程：
//   - `config`（以及 `config.worktree`）：可以设 `core.hooksPath`（让下一次 git
//     command 执行任意脚本）、`core.pager`（命令执行）、`url.*.insteadOf`
//     （把凭据/请求引到别处）、`credential.helper`；
//   - `hooks/**`：仓库自带的可执行代码，`git commit` 会直接运行它们；
//   - `info/*`（exclude/attributes/sparse-checkout）：可改变哪些文件被视为存在或
//     被跟踪，属于"改写规则"；`modules/**/config` 同理是子模块的配置面。
//
// 所以策略是"数据面可写、控制面只读"：index/objects/refs/logs/HEAD 等只是数据，
// config/hooks/info 是执行面与规则面。这条边界也解释了为什么 `request_sandbox_access`
// 不该一刀切拒绝 `.git`——申请 `.git/objects` 是合理诉求，申请 `.git/hooks` 不是。

// gitReadOnlyNames 是 `.git` 下**文件形式**的只读项（相对 gitDir）。
var gitReadOnlyNames = []string{
	"config",
	"config.worktree",
}

// gitReadOnlyDirs 是 `.git` 下整棵子树只读的目录（相对 gitDir）。
var gitReadOnlyDirs = []string{
	"hooks",
	"info",
}

// ProtectGitMetadata 返回一个选项：把 root 的 git 元数据按"数据面可写、控制面
// 只读"登记到策略上。
//
// root 不是 git 仓库（没有 `.git`）时什么都不做——策略保持"整个工作区可写"。
// `.git` 是文件（worktree/submodule 指针）时，除了保护指针文件本身，还会解析
// `gitdir:` 指向的真实目录并按其结构登记只读子路径。
func ProtectGitMetadata(root string) Option {
	return func(policy *Policy) { policy.AddGitProtection(root) }
}

// AddGitProtection 是 ProtectGitMetadata 的命令式版本（供构造后追加，例如
// 会话中途 `git init` 之后）。
func (p *Policy) AddGitProtection(root string) {
	if p == nil {
		return
	}
	resolved, ok := p.canonicalWritableRoot(root)
	if !ok {
		return
	}
	for _, gitDir := range gitDirectories(resolved) {
		// `.git` 自身声明成可写根（带降级标志）：git commit 要在它里面新建
		// index.lock 再改名，只有"整根可写"才能表达；控制面（config/hooks/info）
		// 作为该根的只读子路径，在能表达交集的后端（bwrap/Seatbelt/ACL）上仍然只读。
		p.DeclareDegradableWritableRoot(gitDir, GitReadOnlySubpaths(gitDir)...)
	}
	// `.git` 是文件（worktree/submodule 指针）时，旧版本会把指针文件登记成保护
	// 路径。DSH 对齐后 AddProtectedPaths 只是记录型 no-op，指针文件因此**可写**；
	// 这里保留调用仅用于把"曾经想保护的位置"留在策略记录里（诊断可见）。
	if info, err := os.Lstat(filepath.Join(resolved, ".git")); err == nil && !info.IsDir() {
		p.AddProtectedPaths(filepath.Join(resolved, ".git"))
	}
}

// GitReadOnlySubpaths 返回某个 gitDir 下必须只读的绝对路径（文件与目录混合）。
//
// 目录项表示整棵子树只读。`modules/*/config` 这类"每层都有"的项按当前磁盘内容
// 枚举；之后新增的子模块不在保护范围内，——这是策略的已知边界（`modules` 本身
// 必须可写，`git submodule update` 要写模块的对象库）。
func GitReadOnlySubpaths(gitDir string) []string {
	gitDir = filepath.Clean(strings.TrimSpace(gitDir))
	if gitDir == "" {
		return nil
	}
	out := make([]string, 0, len(gitReadOnlyNames)+len(gitReadOnlyDirs)+4)
	for _, name := range gitReadOnlyNames {
		out = append(out, filepath.Join(gitDir, name))
	}
	for _, dir := range gitReadOnlyDirs {
		out = append(out, filepath.Join(gitDir, dir))
	}
	// 子模块：modules/<name>/config。
	modules := filepath.Join(gitDir, "modules")
	if entries, err := os.ReadDir(modules); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			out = append(out, filepath.Join(modules, entry.Name(), "config"))
		}
	}
	return out
}

// gitDirectories 返回 root 下所有真实的 gitDir（主 `.git` 目录，或 worktree
// 指针指向的目录）。没有 `.git` 时返回 nil。
func gitDirectories(root string) []string {
	candidate := filepath.Join(root, ".git")
	info, err := os.Lstat(candidate)
	if err != nil {
		return nil
	}
	if info.IsDir() {
		return []string{candidate}
	}
	// `.git` 是文件：读取 `gitdir: <path>`。
	data, readErr := os.ReadFile(candidate)
	if readErr != nil {
		return nil
	}
	text := strings.TrimSpace(string(data))
	const prefix = "gitdir:"
	if !strings.HasPrefix(text, prefix) {
		return nil
	}
	target := strings.TrimSpace(strings.TrimPrefix(text, prefix))
	if target == "" {
		return nil
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(root, target)
	}
	if abs, absErr := filepath.Abs(target); absErr == nil {
		target = abs
	}
	target = filepath.Clean(target)
	if real, realErr := filepath.EvalSymlinks(target); realErr == nil {
		target = filepath.Clean(real)
	}
	return []string{target}
}

// WorkspaceGitReadOnlySubpaths 返回某工作区所有 git 元数据的只读子路径
// （绝对路径）。它给 `kit` / `tools/full` 的默认保护用：调用方把结果交给
// Policy.AddReadOnlySubpaths(root, …)。
func WorkspaceGitReadOnlySubpaths(root string) []string {
	var out []string
	for _, gitDir := range gitDirectories(root) {
		out = append(out, GitReadOnlySubpaths(gitDir)...)
	}
	return dedupeStrings(out)
}
