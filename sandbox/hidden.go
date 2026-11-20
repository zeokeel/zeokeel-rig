package sandbox

// 本文件实现"对模型隐藏某些路径"（WithHidden）。
//
// 这是策略层**唯一**按模式约束路径的机制，而且它真的参与判定：ResolvePath /
// CheckPath / CheckWritablePath 与 DeniedPath 都看它，因此 read_file / list_dir /
// glob / grep / search 的结果与写入都会被它挡住。
//
// 历史上还有一层"拒绝 glob"（WithDeniedGlobs）。它先被降级成"只记录不拦截"，随后
// 整个删除：留着它只会让宿主以为设了边界。想按模式看住路径，用本文件这套。

import (
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// WithHidden 隐藏匹配这些 glob 的路径。模式相对**某个根**书写（例如 `**/.env`、
// `secrets/**`），不含 "/" 的模式按基名匹配任意深度（例如 `id_rsa`）。
//
// 隐藏的效果是"模型看不到"：读、列目录、glob/grep 的结果、写入全部拒绝。它不是
// 文件系统权限（shell 里 `cat .env` 仍然成立），也不是 OS 沙箱承诺的一部分——
// 真正的隔离在 runner/部署层。
func WithHidden(patterns ...string) Option {
	return func(policy *Policy) { policy.hidden = normalizeGlobs(patterns) }
}

// Hidden 报告绝对路径 abs 是否对模型隐藏。它是隐藏判定的唯一入口：工具用它筛
// 结果，Policy 的路径判定用它拦读写。
func (p *Policy) Hidden(abs string) bool {
	_, ok := p.hiddenPattern(abs)
	return ok
}

// HiddenPatterns 返回配置的隐藏模式（相对根、以 / 分隔），供诊断与 UI 展示。
func (p *Policy) HiddenPatterns() []string {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return slices.Clone(p.hidden)
}

// hiddenPattern 返回命中 abs 的模式。定义域与拒绝 glob 一致：相对所属根的路径
// （不在任何根内时退回绝对路径），大小写折叠随平台。
func (p *Policy) hiddenPattern(abs string) (string, bool) {
	if p == nil {
		return "", false
	}
	p.mu.RLock()
	patterns := p.hidden
	p.mu.RUnlock()
	if len(patterns) == 0 {
		return "", false
	}
	relative := p.globRelative(abs)
	if pathCaseInsensitive(pathOSName) {
		relative = strings.ToLower(relative)
	}
	base := path.Base(relative)
	for _, pattern := range patterns {
		candidate := pattern
		if pathCaseInsensitive(pathOSName) {
			candidate = strings.ToLower(candidate)
		}
		if GlobMatch(candidate, relative) {
			return pattern, true
		}
		// 不带斜杠的模式按基名匹配任意深度：用户在设置里写 `.env` 时想要的是
		// "任何层级的 .env"，而不是"根下的那一个"。
		if !strings.Contains(candidate, "/") && GlobMatch(candidate, base) {
			return pattern, true
		}
	}
	return "", false
}

// hiddenDenial 把命中转成带理由的拒绝错误；未命中返回 nil。
func (p *Policy) hiddenDenial(abs string) error {
	pattern, ok := p.hiddenPattern(abs)
	if !ok {
		return nil
	}
	return deny("path %q is hidden by pattern %q", p.globRelative(abs), pattern)
}

// globRelative 把绝对路径渲染成"相对所属根、以 / 分隔"的形式，供匹配与报错用。
// 不在任何根内时退回绝对路径（full-access 模式下没有根围栏，但隐藏仍然生效）。
func (p *Policy) globRelative(abs string) string {
	if p != nil {
		if rootPath, ok := p.containingRoot(abs); ok {
			if relative, err := filepath.Rel(rootPath, abs); err == nil && !escapesRelative(relative) {
				return filepath.ToSlash(relative)
			}
		}
	}
	return filepath.ToSlash(filepath.Clean(abs))
}
