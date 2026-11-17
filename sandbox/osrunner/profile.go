package osrunner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// 本文件是四个后端的单一真源：bwrap、Landlock、Seatbelt 与 Windows ACL 的授权
// 形状都只从 BuildProfile 派生，后端里不再各自解释一遍“策略等于什么”。
//
// 这里刻意只做“策略 → 可执行渲染输入”的翻译，不碰任何平台 API，因此可以在任意
// 宿主上单测（Windows/Seatbelt 的渲染参数在 Linux 上也能断言）。

// Profile 是一次策略的可执行渲染输入：四个后端都只从这里派生。
//
// 字段语义都是“最终要渲染成什么”，而不是策略原貌：
//   - ReadOnly 是“读+执行”的宿主路径，通常是 "/"（bwrap 用它做 --ro-bind /，
//     Landlock 用它做 --ro /：没有它连可执行文件都读不到）；
//   - ReadWrite 是“读写”的宿主路径，含设备节点与临时目录；
//   - Protected 声明在 ReadWrite 之下但必须保持只读的路径（例如 workspace 里的
//     .git）。各后端的表达方式不同：bwrap 用“后挂载的 --ro-bind 覆盖前面的 rw
//     bind”，Seatbelt 用 SBPL 的 last-match-wins，Landlock 只能把写授权下移到
//     保护链的兄弟项（它的规则是并集语义，见 landlockGrants）；
//   - PrivateTmp 表示后端能否提供一个“不落宿主”的私有 /tmp。bwrap 用 tmpfs，
//     Seatbelt 不需要（它的 profile 本来就不放行 /tmp 写入），而 Landlock
//     无法创建 tmpfs，因此 Landlock 的 read-only 是真的严格只读——见下面的
//     平台差异说明；
//   - Dropped 是用户显式配置、但渲染时被丢弃的可写根（不存在/无法解析）。它们
//     必须对调用方可见：宿主的明确意图被忽略时不能静默。
type Profile struct {
	// Mode 是渲染所依据的模式（不会是 danger-full-access：那返回 nil Profile）。
	Mode sandbox.Mode
	// ReadOnly 是读+执行路径（规范化的真实绝对路径，已去重、保序）。
	ReadOnly []string
	// ReadWrite 是读写路径（同上）。它已经按 IncludeSlashTmp/IncludeTmpdirEnv
	// 过滤过临时目录，并且**不含**被丢弃的路径。
	ReadWrite []string
	// Protected 是 ReadWrite 之下必须保持只读的路径。
	Protected []string
	// Network 表示策略是否允许对外网络；false 时后端应追加网络隔离参数。
	Network bool
	// PrivateTmp 表示后端是否应挂载私有 tmpfs /tmp。
	PrivateTmp bool
	// Dropped 是从 policy.AdditionalWritableRoots() 里被丢弃的项（不存在或无法解析）。
	// 调用方应当把它报告给用户，而不是当作“已生效”。
	Dropped []string
	// MissingProtected 是策略声明了、但**当前不存在**因而无法作为只读规则渲染的
	// 保护路径。它必须被报告出来：静默丢弃会让"保护 .git"在会话中途 git init
	// 之后变成空承诺（fail-open）。调用方可以在启动/诊断时报警，宿主也可以用
	// WithProtectMissingPaths(true) 把它变成硬错误。
	MissingProtected []string
}

// BuildProfile 把策略渲染成可执行 profile。
//
// danger-full-access 返回 (nil, nil)：调用方本来就是透传，没有需要渲染的约束；
// nil 策略返回包装 sandbox.ErrSandboxUnavailable 的错误（没有策略就没有可强制的
// 效果集，必须 fail closed）。
//
// 两种受限模式的形状：
//
//   - read-only：ReadWrite 只有设备节点（/dev/null 必备，/dev/ptmx、/dev/pts、
//     /dev/tty 存在才加），**不**授予 workspace、/tmp、$TMPDIR，也**不**授予
//     /dev/shm（它是可写的 tmpfs，写进去的文件会真的落在宿主上——实测可见，
//     与“除 /dev/null 等设备外没有写授权”矛盾）；PrivateTmp = true。私有 tmpfs
//     属于“不落宿主”的可写区，让 mktemp、git status 这类只读意图的命令仍然可用。
//   - workspace-write：ReadWrite = policy.Roots() + policy.AdditionalWritableRoots() +
//     平台临时目录（按 IncludeSlashTmp / IncludeTmpdirEnv 过滤）+ 设备节点；
//     Protected = 策略的保护路径；PrivateTmp = true；
//     Network = policy.NetworkAllowed()。
//
// # 平台差异（必须知道）
//
// bwrap 能满足 PrivateTmp（`--tmpfs /tmp`），Landlock 不能：它只治理进程对既有
// 路径的访问，无法为用户新建挂载，也没有“私有 /tmp”的概念。因此 Landlock 的
// read-only 保持严格只读（只授予设备节点），需要写临时文件的命令在 Landlock 下
// 会失败——这是平台能力差异，不是 profile 的 bug。Seatbelt 同样没有 tmpfs，但
// 它只放行 profile 里列出的 subpath，效果与 Landlock 一致：read-only 时不放行
// /tmp。
func BuildProfile(policy *sandbox.Policy) (*Profile, error) {
	if policy == nil {
		return nil, fmt.Errorf("osrunner: build profile requires a policy: %w", sandbox.ErrSandboxUnavailable)
	}
	switch mode := policy.Mode(); mode {
	case sandbox.ModeDangerFullAccess:
		return nil, nil
	case sandbox.ModeReadOnly:
		return &Profile{
			Mode:       sandbox.ModeReadOnly,
			ReadOnly:   canonicalUnique([]string{"/"}),
			ReadWrite:  canonicalUnique(deviceWritePaths(false)),
			Network:    policy.NetworkAllowed(),
			PrivateTmp: true,
		}, nil
	case sandbox.ModeWorkspaceWrite:
		roots, dropped := canonicalSplit(workspaceWritableCandidates(policy))
		// 设备节点同样要进 ReadWrite：Landlock 没有 --dev 这种“自动设备集”，
		// 不给 /dev/null 写权限时 shell 的 `>/dev/null` 会在 workspace-write 下
		// 失败（bwrap 会跳过 /dev/*，因为它由 --dev 提供）。
		readWrite := append(append(append([]string(nil), roots...), profileTempRoots(policy)...), deviceWritePaths(true)...)
		return &Profile{
			Mode:      sandbox.ModeWorkspaceWrite,
			ReadOnly:  canonicalUnique([]string{"/"}),
			ReadWrite: canonicalUnique(readWrite),
			// 只有真实存在的路径才能被 bwrap --ro-bind / Landlock O_PATH 渲染；
			// 缺失项单独报告，Seatbelt 仍然会把它渲染成规则（见 SeatbeltProfile）。
			Protected:        canonicalUnique(existingPaths(policy.ProtectedPaths())),
			MissingProtected: missingPaths(policy.ProtectedPaths()),
			Network:          policy.NetworkAllowed(),
			PrivateTmp:       true,
			Dropped:          dropped,
		}, nil
	default:
		return nil, fmt.Errorf("osrunner: build profile: unsupported policy mode %q: %w", mode, sandbox.ErrSandboxUnavailable)
	}
}

// workspaceWritableCandidates 返回 workspace-write 下可写根的候选（未规范化）。
//
// 必须同时包含 policy.Roots() 与 policy.AdditionalWritableRoots()：后者只含
// WithWritableRoots 显式追加的根与运行期授权，**不含**策略根（roots 是读围栏的
// 基准，写授权由 workspace-write 模式本身表达）。只取 WritableRoots() 会让
// workspace-write 连工作区都写不进去；只取 Roots() 会忽略宿主显式配置的额外
// 可写目录与运行期授权。
func workspaceWritableCandidates(policy *sandbox.Policy) []string {
	out := append([]string(nil), policy.Roots()...)
	return append(out, policy.AdditionalWritableRoots()...)
}

// deviceWritePaths 返回必须在受限模式下保持可写的设备节点。
//
// /dev/null 无条件包含（shell 的 `>/dev/null` 依赖它）；/dev/ptmx、/dev/pts、
// /dev/tty 按存在性添加——它们是终端正常工作所必需的，缺一个就会让交互式命令
// 莫名失败。
//
// includeSharedMemory 控制 /dev/shm：它是**可写的 tmpfs**，写进去的内容会真的
// 落在宿主上（实测 /dev/shm/probe 在宿主上持久存在），所以 read-only 不授予它；
// workspace-write 授予它，因为共享内存（数据库、Chromium、Python multiprocessing）
// 在可写模式下本来就是预期能力。
func deviceWritePaths(includeSharedMemory bool) []string {
	paths := []string{"/dev/null"}
	candidates := []string{"/dev/ptmx", "/dev/pts", "/dev/tty"}
	if includeSharedMemory {
		candidates = append(candidates, "/dev/shm")
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			paths = append(paths, candidate)
		}
	}
	return paths
}

// profileTempRoots 返回按策略过滤后的可写临时目录候选。
//
// 过滤按**路径身份**而不是按“来源”做，避免 $TMPDIR=/tmp 时出现“关了 /tmp 却仍然
// 可写”的漏洞：
//   - 字面 /tmp（以及 macOS 的 /private/tmp）只受 IncludeSlashTmp 控制；
//   - 其余（os.TempDir() 解析出的自定义 TMPDIR）只受 IncludeTmpdirEnv 控制。
//
// 注意 bwrap 的 PrivateTmp 会额外挂一个私有 tmpfs /tmp：那是“不落宿主”的临时区，
// 即使 IncludeSlashTmp=false 也存在——它保护的是宿主 /tmp 不被写入，而不是禁止
// 进程拿到某个临时目录。
func profileTempRoots(policy *sandbox.Policy) []string {
	var out []string
	for _, candidate := range platformTempRoots() {
		if candidate == "/tmp" || candidate == "/private/tmp" {
			if policy.IncludeSlashTmp() {
				out = append(out, candidate)
			}
			continue
		}
		if policy.IncludeTmpdirEnv() {
			out = append(out, candidate)
		}
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

// missingPaths 返回给定路径里当前不存在的那部分，用于 Profile.MissingProtected
// 诊断：策略声明了保护、但渲染时路径不存在。
func missingPaths(paths []string) []string {
	var out []string
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			out = append(out, path)
		}
	}
	return out
}
