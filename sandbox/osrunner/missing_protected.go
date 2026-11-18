package osrunner

import (
	"errors"
	"fmt"
	"strings"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// ErrMissingProtected 表示策略声明了保护路径、但那个位置此刻还不存在，而当前后端
// **无法**渲染不存在的保护规则。
//
// 为什么必须拒绝执行（fail closed）：放任它继续，沙箱里新建出来的目录（典型是
// `git init` 出来的 `.git`）在本次运行里完全不受保护——"工具说保护了 `.git`"与
// 事实不符。这种"保护承诺失效"比直接报错危险得多：用户以为自己受保护。
var ErrMissingProtected = errors.New("osrunner: protected path does not exist yet")

// missingProtectedError 是"保护路径缺失导致拒绝执行"的具体错误。
//
// 它显式实现 Unwrap，让 `errors.Is(err, sandbox.ErrSandboxUnavailable)` 依然成立
// （调用方按"沙箱不可用"统一处理），同时错误文案以**我们自己的解释**结尾——
// 直接 `%w` 包一层会让消息尾巴变成 `sandbox: no usable OS sandbox runner`，
// 那对模型和用户都是误导（沙箱是好的，缺的是保护路径）。
type missingProtectedError struct {
	backend string
	paths   []string
}

func (e *missingProtectedError) Error() string {
	return fmt.Sprintf(
		"%s backend cannot enforce protection for paths that do not exist yet: %s; "+
			"continuing would silently leave them unprotected (for example a .git created inside the sandbox could be written); "+
			"create them first (or run the initialization as a separate step), or opt out with sandbox.WithProtectMissingPaths(false) if reporting-only is acceptable",
		e.backend, strings.Join(e.paths, ", "))
}

// Unwrap 让 errors.Is(err, sandbox.ErrSandboxUnavailable) 成立。
func (e *missingProtectedError) Unwrap() error { return sandbox.ErrSandboxUnavailable }

// Is 让 errors.Is(err, ErrMissingProtected) 成立。
func (e *missingProtectedError) Is(target error) bool { return target == ErrMissingProtected }

// checkMissingProtected 按后端能力决定"保护路径缺失"该怎么处理。
//
// 只有**无法渲染不存在路径**的后端（bwrap / Landlock / Windows ACL）才需要这个
// 检查：它们要求路径真实存在（绑定挂载 / O_PATH / ACL 对象），没有等价手段。
// Seatbelt 不该调用它——SBPL 的 subpath 规则按访问时求值，不存在的路径也能渲染成
// deny 规则，那条路本来就更强。
//
// 返回的错误里必须带上**具体路径**与**为什么不能继续**：模型与用户都要能读懂。
func checkMissingProtected(profile *Profile, policy *sandbox.Policy, backend string) error {
	if profile == nil || policy == nil {
		return nil
	}
	if !policy.ProtectMissingPaths() {
		return nil
	}
	// Seatbelt 不需要、也**不应该**走这条：SBPL 的 subpath 规则按访问时求值，
	// 缺失路径也能渲染成 deny 规则（见 SeatbeltProfile）。这里兜一层，防止将来
	// 有人在 Seatbelt 的 Confine 里误接这个检查，把"本可以保护"变成"拒绝执行"。
	if backend == RunnerSeatbelt {
		return nil
	}
	missing := requiredMissingProtected(profile, policy)
	if len(missing) == 0 {
		return nil
	}
	return &missingProtectedError{backend: backend, paths: missing}
}

// missingProtectedWarning 是给 sandbox_status / rigdoctor 的人话警告：当宿主显式
// 选择了"只报告"（WithProtectMissingPaths(false)）时，把这些路径列出来告诉用户
// "本次运行不会被保护"。
func missingProtectedWarning(profile *Profile) string {
	if profile == nil || len(profile.MissingProtected) == 0 {
		return ""
	}
	return "these protected paths do not exist yet, so this run will not protect them: " +
		strings.Join(profile.MissingProtected, ", ") +
		" (re-run after they are created to get protection)"
}

// requiredMissingProtected 从 `Profile.MissingProtected` 里挑出**必须挡下来**的项。
//
// MissingProtected 混着两类东西，语义完全不同：
//
//  1. **宿主显式声明的保护路径**（`WithProtectedPaths` / `AddProtectedPaths`）以及
//     **git 元数据目录**这类"整块被声明保护"的位置——它们不在就等于保护承诺失效，
//     在不支持"渲染不存在路径"的后端上必须拒绝执行（F3）。
//  2. **尽力而为的子路径提示**：git 控制面里的 `config.worktree`、`info/`、
//     `~/.npm/.npmrc` 这类"将来可能出现"的位置。正常仓库里它们本来就缺，把它们也
//     算成硬失败会让每一条命令都跑不起来——那会把 fail-closed 变成产品不可用。
//
// 判据：缺失项落在某个**显式保护路径**或某个**可降级可写根（git 目录）**之下才算。
func requiredMissingProtected(profile *Profile, policy *sandbox.Policy) []string {
	if profile == nil || policy == nil {
		return nil
	}
	explicit := policy.ProtectedPaths()
	var out []string
	for _, missing := range profile.MissingProtected {
		// 只认"显式声明"的保护路径。git 元数据目录本身若存在，它的
		// `config.worktree`/`info/` 之类缺失项属于"尽力而为的提示"（正常仓库里
		// 本来就缺），把它们算成硬失败会让每条命令都跑不起来；若 git 目录**根本
		// 不存在**，kit 侧不会登记它（见 kit.addDefaultProtection），因此也不会
		// 走进这里——那种场景由"宿主显式声明 `.git`"覆盖。
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
