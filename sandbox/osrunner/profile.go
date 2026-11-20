package osrunner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// 本文件把策略渲染成四个后端共用的可执行输入。
//
// # 与 DSH（sandbox-local）的严格对齐
//
// 渲染结果**逐 token / 逐字符**对齐 DSH 的实现，这是本包唯一的产品口径：
//
//	bwrap read-only        --ro-bind / /  --dev /dev  --unshare-pid  --proc /proc  --die-with-parent
//	bwrap workspace-write  上面 + --tmpfs /tmp  --bind <workspaceRoot> <workspaceRoot>
//	Landlock               --ro /  --rw /dev/null      （workspace-write 再 --rw /tmp --rw <ws>）
//	Seatbelt read-only     (version 1) (allow default) (deny file-write*) (allow file-write* (literal "/dev/null"))
//	Seatbelt workspace-wr  上面 + (allow file-write* (subpath "<ws>") (subpath "<tmp>") (subpath "<tmpdir>"))
//
// 因此本包**不再**渲染 DSH 没有的东西：`--cap-drop ALL`、`--remount-ro /dev`、
// `--unshare-net`、seccomp 网络过滤器、宿主守护进程 socket 遮蔽、工具链缓存根、
// `/var/tmp`、SBPL 的 pty/fcntl/user-preference 规则、保护路径的只读规则。这些是
// Rig 早期的加固，与"与 DSH 完全一致"互斥；对齐时被刻意放弃，清单见
// .audit/impl-osrunner.md。
//
// 边界（与 DSH 相同，必须在文档里说清）：只治理**文件效果**；网络、进程可见性、
// 设备访问都不在承诺内。

// Profile 是一次策略的可执行渲染输入：四个后端都只从这里派生。
//
// 字段语义都是"最终要渲染成什么"，而不是策略原貌：
//   - ReadOnly 是"读+执行"的宿主路径，恒为 "/"（没有它连可执行文件都读不到）；
//   - ReadWrite 是"读写"的宿主路径：read-only 是 /dev/null [+ 策略的 scratch 目录]；
//     workspace-write 是 /dev/null + 平台临时目录 + 策略工作区根 [+ scratch]
//     （顺序固定，见 BuildProfile）；
//   - Network 记录策略是否允许网络。**不再影响渲染**：DSH 的三个后端都不按
//     AllowNetwork 生成参数（bwrap 不加 --unshare-net、Landlock 不加 --no-net），
//     保留字段只为诊断与 API 兼容。
//   - Protected / MissingProtected 仍然按策略计算，但**任何后端都不再渲染**它们：
//     DSH 没有"根内只读子路径"的概念。它们只用于 RuntimeReport 诊断，让宿主知道
//     "进程内围栏保护了什么、OS 层没有保护什么"。
//   - ToolchainCaches / MissingToolchainCaches / MaskedSockets / TmpDisabledReason
//     保留字段以维持 API 兼容，恒为空：对齐后本包不再创建缓存目录、不再遮蔽
//     socket，也不再因为"工作区在 /tmp 下"禁用私有 tmpfs（顺序由 DSH 固定为
//     tmpfs 在前、工作区 bind 在后）。
type Profile struct {
	// Mode 是渲染所依据的模式（不会是 danger-full-access：那返回 nil Profile）。
	Mode sandbox.Mode
	// ReadOnly 是读+执行路径（规范化的真实绝对路径，已去重、保序）。
	ReadOnly []string
	// ReadWrite 是读写路径（同上）。
	ReadWrite []string
	// TempRoots 是本策略放行的平台临时目录根（workspace-write 才有；read-only 为
	// 空）。渲染层用它判断"要不要挂私有 tmpfs /tmp"、"哪些 ReadWrite 项是临时
	// 目录"，因此两个开关（WithSlashTmp/WithTmpdirEnv）在渲染层是真的生效的。
	TempRoots []string
	// Scratch 是策略声明的私有 scratch 目录（规范化的绝对路径；未设置时为空）。
	// 它是 read-only 档唯一的目录可写面，因此必须在 ReadWrite 里出现，并且各后端
	// 都要按**目录**（bind/--rw/subpath）渲染，而不是像 /dev/null 那样按文件。
	Scratch string
	// Protected 是策略声明为只读、但 OS 后端**不再渲染**的路径（仅诊断）。
	Protected []string
	// Network 表示策略是否允许对外网络（不再影响渲染，仅诊断）。
	Network bool
	// PrivateTmp 恒为 false：DSH 只在 workspace-write 里挂 `--tmpfs /tmp`，
	// read-only 不挂（挂了就等于给只读模式一个可写 /tmp）。
	PrivateTmp bool
	// Dropped 是用户显式配置、但渲染时被丢弃的可写根（不存在/无法解析）。
	// 它们必须对调用方可见：宿主的明确意图被忽略时不能静默。
	Dropped []string
	// WritableRoots 是策略的可写面模型（根 + 根内只读子路径）；仅诊断。
	WritableRoots []sandbox.WritableRoot
	// ToolchainCaches 恒为空（对齐后不再授权缓存根）；保留字段以维持 API。
	ToolchainCaches []sandbox.WritableRoot
	// MissingToolchainCaches 恒为空；保留字段以维持 API。
	MissingToolchainCaches []string
	// MaskedSockets 恒为空（不再遮蔽宿主守护进程 socket）；保留字段以维持 API。
	MaskedSockets []string
	// TmpDisabledReason 恒为空（不再禁用私有 tmpfs）；保留字段以维持 API。
	TmpDisabledReason string
	// MissingProtected 是策略声明了、但**当前不存在**因而无法作为只读规则渲染的
	// 保护路径（仅诊断：OS 后端不渲染保护规则）。
	MissingProtected []string
}

// BuildProfile 把策略渲染成可执行 profile。
//
// danger-full-access 返回 (nil, nil)：调用方本来就是透传，没有需要渲染的约束；
// nil 策略返回包装 sandbox.ErrSandboxUnavailable 的错误（没有策略就没有可强制的
// 效果集，必须 fail closed）。
func BuildProfile(policy *sandbox.Policy) (*Profile, error) {
	if policy == nil {
		return nil, fmt.Errorf("osrunner: build profile requires a policy: %w", sandbox.ErrSandboxUnavailable)
	}
	switch mode := policy.Mode(); mode {
	case sandbox.ModeDangerFullAccess:
		return nil, nil
	case sandbox.ModeReadOnly:
		// read-only 的目录可写面只有 scratch（没有 workspace、没有 /tmp）。
		scratch, droppedScratch := canonicalSplit([]string{policy.ScratchDir()})
		readWrite := []string{"/dev/null"}
		readWrite = append(readWrite, scratch...)
		return &Profile{
			Mode:      sandbox.ModeReadOnly,
			ReadOnly:  []string{"/"},
			ReadWrite: readWrite,
			Scratch:   firstPath(scratch),
			Network:   policy.NetworkAllowed(),
			Dropped:   droppedScratch,
		}, nil
	case sandbox.ModeWorkspaceWrite:
		roots, dropped := canonicalSplit(workspaceRootCandidates(policy))
		tempRoots := profileTempRoots(policy)
		scratch, droppedScratch := canonicalSplit([]string{policy.ScratchDir()})
		readWrite := append([]string{"/dev/null"}, tempRoots...)
		readWrite = append(readWrite, roots...)
		readWrite = append(readWrite, scratch...)
		// 保护路径在 DSH 对齐后**不再由任何后端渲染**，策略层也已把它降级为
		// "仅记录"（`WriteDenyPaths` 恒返回空且已标 Deprecated）。这里保留空的
		// deny 切片是为了让诊断字段（Protected / MissingProtected）继续存在，
		// 但不调用任何已弃用 API。
		var deny []string
		return &Profile{
			Mode:             sandbox.ModeWorkspaceWrite,
			ReadOnly:         []string{"/"},
			ReadWrite:        canonicalUnique(readWrite),
			TempRoots:        tempRoots,
			Scratch:          firstPath(scratch),
			Protected:        canonicalUnique(existingPaths(deny)),
			MissingProtected: missingProtectionReport(deny),
			WritableRoots:    append([]sandbox.WritableRoot(nil), policy.WritableRoots()...),
			Network:          policy.NetworkAllowed(),
			Dropped:          append(dropped, droppedScratch...),
		}, nil
	default:
		return nil, fmt.Errorf("osrunner: build profile: unsupported policy mode %q: %w", mode, sandbox.ErrSandboxUnavailable)
	}
}

// workspaceRootCandidates 返回要 bind/授权的工作区根（策略根 + 显式追加的可写根）。
//
// DSH 的 workspace-write 只授权"工作区"，因此这里取策略的可写面根：`Policy.Roots()`
// 与 `AdditionalWritableRoots()` 合并（后者是宿主显式声明的额外可写目录），去重后
// 交给 canonicalSplit 过滤不存在的项。每次 Confine 都重新求值：运行期授权
// （`Policy.AddWritableRoot`）必须能在下一条命令上生效。
func workspaceRootCandidates(policy *sandbox.Policy) []string {
	if policy == nil {
		return nil
	}
	out := make([]string, 0, len(policy.Roots())+len(policy.AdditionalWritableRoots()))
	out = append(out, policy.Roots()...)
	out = append(out, policy.AdditionalWritableRoots()...)
	return out
}

// missingProtectionReport 返回"需要报告给调用方的缺失保护项"（仅诊断）。
//
// 规则：只保留**父目录存在**的缺失项——父目录存在意味着"这个位置随时可能出现"
// （例如 `.git/config.worktree` 在开启 extensions.worktreeConfig 后才出现），值得
// 让宿主知道"此刻它是空的保护承诺"。父目录都不存在时给出的是纯噪声。
func missingProtectionReport(paths []string) []string {
	var out []string
	for _, path := range dedupeStrings(paths) {
		if _, err := os.Stat(path); err == nil {
			continue
		}
		if _, err := os.Stat(filepath.Dir(path)); err != nil {
			continue
		}
		out = append(out, path)
	}
	return out
}

// dedupeStrings 去重并保序（空项丢弃）。
func dedupeStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

// canonicalUnique 把路径规范化为真实绝对路径并去重；不存在的候选被丢弃。
func canonicalUnique(paths []string) []string {
	kept, _ := canonicalSplit(paths)
	return kept
}

// canonicalSplit 与 canonicalUnique 相同，但额外把被丢弃的项返回给调用方。
//
// 丢弃规则：去空白后为空、无法 Abs、或（EvalSymlinks 之后）不存在。重复项只保留
// 第一次出现的位置，顺序保持不变。
func canonicalSplit(paths []string) (kept, dropped []string) {
	kept = make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			dropped = append(dropped, path)
			continue
		}
		abs = filepath.Clean(abs)
		if real, err := filepath.EvalSymlinks(abs); err == nil {
			abs = filepath.Clean(real)
		}
		if _, err := os.Stat(abs); err != nil {
			dropped = append(dropped, abs)
			continue
		}
		if _, exists := seen[abs]; exists {
			continue
		}
		seen[abs] = struct{}{}
		kept = append(kept, abs)
	}
	return kept, dropped
}

// existingPaths 过滤出当前存在的路径。
func existingPaths(paths []string) []string {
	var out []string
	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			out = append(out, path)
		}
	}
	return out
}
