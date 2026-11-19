package osrunner

import (
	"errors"
	"strings"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// 保护路径的诊断层。
//
// # DSH 对齐后的状态
//
// 四个后端都**不再渲染**保护路径：bwrap 不生成覆盖它的 `--ro-bind`、Seatbelt 不
// 生成 `(deny file-write* (subpath …))`、Landlock 不做"写授权下移"、Windows 不下发
// 保护 ACE。这是"与 DSH（sandbox-local）完全一致"的直接后果——DSH 没有"根内只读
// 子路径"这个概念。
//
// 因此本文件只保留**诊断**能力：宿主仍能看到"策略声明了哪些保护路径、其中哪些
// 此刻还不存在"，但它不代表任何强制。真正拦住写入的是进程内围栏
// （`Policy.CheckWritablePath` / `AllowsWrite`）与命令风险分类。

// ErrMissingProtected 是历史哨兵：曾经表示"保护路径不存在 → 拒绝执行（fail closed）"。
//
// Deprecated: 保护规则不再渲染，因此本包不会因为保护路径缺失而拒绝执行；保留这个
// 导出值只为源码兼容（宿主可能仍在 `errors.Is` 里判它，那永远不会成立）。
var ErrMissingProtected = errors.New("osrunner: protected path does not exist yet")

// missingProtectedWarning 是给 sandbox_status / rigdoctor 的人话警告：这些路径
// 声明了保护但 OS 层不会保护它们。
//
// 对齐后**所有**声明了保护的路径都属于这一类（不是只有缺失的），文案要说清这一点，
// 否则宿主会以为"存在路径仍然受 OS 层保护"。
func missingProtectedWarning(profile *Profile) string {
	if profile == nil || len(profile.Protected) == 0 {
		return ""
	}
	return "these paths are declared as protected, but no OS backend renders protection rules any more (DSH parity): " +
		strings.Join(profile.Protected, ", ") +
		" — only the in-process fence denies writes to them"
}

// requiredMissingProtected 从 `Profile.MissingProtected` 里挑出宿主**显式声明**的项。
//
// 用途仅限诊断（RuntimeReport.UnprotectedPaths）：把它们按"声明了但此刻还不存在"
// 报给宿主/用户。对齐后它们同样不会被任何 OS 规则覆盖，所以这个名单只影响文案。
func requiredMissingProtected(profile *Profile, policy *sandbox.Policy) []string {
	if profile == nil || policy == nil {
		return nil
	}
	// 策略层的保护路径已降级为"仅记录"（`ProtectedPaths` 恒返回空且已标
	// Deprecated），因此这里不再读它——保留空切片即可，诊断文案不依赖它。
	var explicit []string
	var out []string
	for _, missing := range profile.MissingProtected {
		if withinAnyPath(missing, explicit) {
			out = append(out, missing)
		}
	}
	return out
}

// withinAnyPath 报告 path 是否等于或位于 dirs 中任意一项之下（平台感知）。
func withinAnyPath(path string, dirs []string) bool {
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		if sandbox.PathEqual(dir, path) || sandbox.PathWithin(dir, path) {
			return true
		}
	}
	return false
}
