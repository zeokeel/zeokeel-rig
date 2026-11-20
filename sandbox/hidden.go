package sandbox

// 本文件实现"对模型隐藏某些路径"（WithHidden）。
//
// 与已停用的拒绝 glob（WithDeniedGlobs）的区别只有一点，但那一点是本质的：
// WithHidden **参与判定**——它是 ResolvePath / CheckPath / CheckWritablePath 与
// DeniedPath 的共同判据，因此 read_file / list_dir / glob / grep / search 的结果
// 与写入都会被它挡住。WithDeniedGlobs 现在只记录在策略里，不拦任何东西。
//
// 为什么保留两套（而不是把 WithDeniedGlobs 复活）：v0.24.0 的 DSH 对齐已经把
// "拒绝 glob" 的语义从公开承诺里删掉了，旧名字的文档与回归测试都钉着"不拦"。
// 重新启用它会静默改变那些宿主的既有行为；新名字 + 新判定才是可预期的。

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
