package sandbox

import (
	"os"
	"path/filepath"
	"strings"
)

// 工作区元数据保护。
//
// 工作区可写不等于"什么都能改"：`.git`、`.aws`、`.codex` 这类目录承载的是
// 版本控制状态与凭据，被约束的 agent 修改它们等于改写自己的守卫（改 git hooks、
// 改 AWS 凭据、改工具配置）。常见实现对每个可写根默认只读这几种元数据；本 SDK 把它
// 做成**可选**的组装层能力（`full.AllOptions.ProtectWorkspaceMetadata` /
// `kit.Config.ProtectWorkspaceMetadata`），因为开启后工作区里的 `git commit`
// 之类写操作会被沙箱拒绝，属于会改变既有体验的行为。
//
// 缺失的路径同样返回：保护"尚不存在"的位置是有意义的（例如会话中途 git init），
// Seatbelt 的 subpath 规则按访问时求值；其余后端会把它记进
// Profile.MissingProtected 供诊断。

// workspaceMetadataNames 是被默认保护的工作区元数据目录名。
var workspaceMetadataNames = []string{".git", ".aws", ".codex"}

// WorkspaceMetadataPaths 返回某个工作区根下应该保护的元数据路径（已规范化、
// 去重、保留不存在的项）。`.git` 是文件（worktree / submodule）时，除了保护
// 指针文件本身，还会解析 `gitdir:` 指向的真实目录并一并保护——否则改写指针就能
// 把写入引到别处。
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

// AddProtectedPaths 在策略构造之后追加保护路径（相对路径按主根解析）。它由
// tools/full 与 kit 在开启 `ProtectWorkspaceMetadata` 时调用，避免宿主为了加几个
// 默认保护项而重建整个策略。重复项与非法空项被忽略。
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
