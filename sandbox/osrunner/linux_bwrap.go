//go:build linux

package osrunner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig/internal/procattr"
	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// bwrapRunner 用系统 bubblewrap（bwrap）包装目标命令。
//
// bwrap 走用户命名空间 + 挂载命名空间：read-only 把整个宿主根只读绑定，并把
// /dev 的文件系统重新挂成只读；workspace-write 额外挂一个私有 tmpfs /tmp，再把
// Profile.ReadWrite 里的每个宿主路径（策略根、$TMPDIR、/var/tmp 等）以读写 bind
// 挂回原路径，最后用 --ro-bind 覆盖 Protected 路径。
//
// 具体参数由 BwrapArgs 从 BuildProfile 生成，本文件只负责探测与包装。
type bwrapRunner struct {
	// path 是 exec.LookPath 解析出的 bwrap 可执行文件路径。
	path string
}

var _ sandbox.Runner = (*bwrapRunner)(nil)

// Confine 实现 sandbox.Runner。
func (r *bwrapRunner) Confine(argv []string, policy *sandbox.Policy) (sandbox.ConfinedCommand, error) {
	if policy == nil {
		return sandbox.ConfinedCommand{}, fmt.Errorf("osrunner: bwrap runner requires a policy: %w", sandbox.ErrSandboxUnavailable)
	}
	if r.path == "" {
		return sandbox.ConfinedCommand{}, fmt.Errorf("osrunner: bwrap runner has no executable: %w", sandbox.ErrSandboxUnavailable)
	}
	mode := policy.Mode()
	if mode == sandbox.ModeDangerFullAccess {
		return passthrough(argv), nil
	}
	if mode != sandbox.ModeReadOnly && mode != sandbox.ModeWorkspaceWrite {
		return sandbox.ConfinedCommand{}, fmt.Errorf("osrunner: bwrap runner: unsupported policy mode %q: %w", mode, sandbox.ErrSandboxUnavailable)
	}
	if len(argv) == 0 {
		return sandbox.ConfinedCommand{}, fmt.Errorf("osrunner: bwrap runner: empty argv: %w", sandbox.ErrSandboxUnavailable)
	}

	// 干净机器上 `~/.cargo`/`~/.m2`/`~/Library/Developer` 还不存在：bwrap 的
	// --bind 要求目标存在，所以先把这些"程序自己会创建的缓存目录"建出来
	//（只建目录、0700；失败不阻断，留给 MissingToolchainCaches 上报）。
	ensureToolchainCacheRoots(policy)
	// 保护路径缺失 → 拒绝执行（bwrap 的 --ro-bind 要求目标存在）。Seatbelt 不走这里。
	if profile, profileErr := BuildProfile(policy); profileErr == nil {
		if missingErr := checkMissingProtected(profile, policy, RunnerBwrap); missingErr != nil {
			return sandbox.ConfinedCommand{}, missingErr
		}
	}

	program, err := seccompProgram(policy.NetworkAllowed())
	if err != nil {
		// fail closed：策略要求断网但这个后端装不上 seccomp，就不能以"已隔离"
		// 的姿态跑命令。
		return sandbox.ConfinedCommand{}, fmt.Errorf("osrunner: bwrap runner: %w: %w", sandbox.ErrSandboxUnavailable, err)
	}
	bwrapArgs := append(BwrapArgs(policy), "--")
	if !program.Empty() {
		// 网络过滤器：`bwrap --seccomp 3`，由 sh 包装把程序文件打开成 fd 3
		// （argv 只能传数字 fd，不能传文件内容）。见 seccomp_linux.go。
		bwrapArgs = append(BwrapArgs(policy), "--seccomp", "3", "--")
		prefix, cleanup, prefixErr := bwrapSeccompPrefix(program, r.path, bwrapArgs)
		if prefixErr != nil {
			return sandbox.ConfinedCommand{}, fmt.Errorf("osrunner: bwrap runner: %w: %w", sandbox.ErrSandboxUnavailable, prefixErr)
		}
		// 注意：**不能**在这里 cleanup。过滤器文件必须活到子进程把它打开成 fd 3；
		// 包装脚本在 open 之后立刻 `rm -f`，所以正常路径不会有残留。
		// cleanup 只用于"包装脚本参数构造失败"这类错误路径（见下方 defer 语义）。
		_ = cleanup
		target := append(append([]string(nil), prefix...), argv...)
		return sandbox.ConfinedCommand{
			Argv:             target,
			Enforcement:      sandbox.EnforcementFull,
			DenialSignatures: DenialSignatures(RunnerBwrap),
		}, nil
	}
	return wrapped(
		[]string{r.path},
		bwrapArgs,
		argv,
		sandbox.EnforcementFull,
		DenialSignatures(RunnerBwrap),
	), nil
}

// probeBwrap 对系统 bwrap 做一次功能探测：用只读 profile 启动 `true`。
//
// 仅仅存在 /usr/bin/bwrap 不代表可用（容器里可能因为禁止用户命名空间而失败），
// 因此选择 runner 前必须真实运行一次。返回解析出的路径与是否可用。
func probeBwrap(timeout time.Duration) (string, bool) {
	path, reason := probeBwrapDetailed(timeout)
	return path, reason == ""
}

// probeBwrapDetailed 与 probeBwrap 相同，但额外返回**不可用的原因**（可用时为空）。
//
// 三种失败需要完全不同的处置，笼统地说"bwrap 不可用"没法排查：
//   - 没装 bwrap；
//   - 装了但用户命名空间被禁（容器/加固内核）；
//   - WSL1（不支持需要的命名空间特性）。
//
// 另外它会拒收"可被被约束进程替换"的 bwrap：候选二进制或其目录 group/world
// 可写、或候选落在当前工作目录（agent 工作区）之下时一律不信任——runner 是
// 沙箱本体，从 PATH 里挑一个可写候选等于把边界交给攻击者。
func probeBwrapDetailed(timeout time.Duration) (path string, reason string) {
	candidate, err := exec.LookPath("bwrap")
	if err != nil {
		return "", "bwrap is not installed (looked up on PATH)"
	}
	absolute, err := filepath.Abs(candidate)
	if err != nil {
		return "", fmt.Sprintf("cannot resolve bwrap path %q: %v", candidate, err)
	}
	if why := untrustedExecutableReason(absolute); why != "" {
		return "", fmt.Sprintf("refusing to trust %q: %s", absolute, why)
	}
	args := append([]string(nil), BwrapArgs(sandbox.New(sandbox.WithMode(sandbox.ModeReadOnly)))...)
	args = append(args, "--", "true")
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, absolute, args...)
	procattr.HideWindow(cmd)
	output, runErr := cmd.CombinedOutput()
	if runErr == nil {
		return absolute, ""
	}
	if ctx.Err() != nil {
		return "", fmt.Sprintf("bwrap probe timed out after %s", timeout)
	}
	return "", classifyBwrapFailure(string(output), runErr)
}

// classifyBwrapFailure 把 bwrap 的失败输出归类成可行动的原因。
func classifyBwrapFailure(output string, runErr error) string {
	lowered := strings.ToLower(output)
	switch {
	case strings.Contains(lowered, "no permissions to creating new namespace"),
		strings.Contains(lowered, "creating new namespace failed"),
		strings.Contains(lowered, "setting up uid map"),
		strings.Contains(lowered, "operation not permitted"):
		reason := "bubblewrap cannot create a user namespace on this host (unprivileged user namespaces are disabled or blocked by seccomp/LSM)"
		if isWSL1() {
			reason += "; this looks like WSL1, which does not support the required namespace features"
		}
		return reason
	case strings.Contains(lowered, "can't find source path"), strings.Contains(lowered, "no such file or directory"):
		return "bubblewrap rejected the read-only probe profile (unexpected missing path)"
	}
	detail := strings.TrimSpace(strings.SplitN(output, "\n", 2)[0])
	if detail == "" {
		detail = runErr.Error()
	}
	if len(detail) > 200 {
		detail = detail[:200] + "…"
	}
	return "bwrap probe failed: " + detail
}

// isWSL1 粗判 WSL1：/proc/version 提到 Microsoft 但没有 WSL2 的
// "microsoft-standard" 标识。仅用于把诊断文案说得更具体，不参与安全判断。
func isWSL1() bool {
	data, err := os.ReadFile("/proc/version")
	if err != nil {
		return false
	}
	text := strings.ToLower(string(data))
	return strings.Contains(text, "microsoft") && !strings.Contains(text, "microsoft-standard")
}

// RunnerName 实现 sandbox.Reporter。
func (r *bwrapRunner) RunnerName() string { return RunnerBwrap }

// RunnerEnforcement 实现 sandbox.Reporter：bwrap 通过挂载命名空间治理文件的
// 读/写/建/删，覆盖策略承诺的全部文件效果。
func (r *bwrapRunner) RunnerEnforcement() sandbox.Enforcement { return sandbox.EnforcementFull }

// RunnerLimits 实现 sandbox.Reporter：文件效果与 TCP 网络都是完整的（full），
// 剩下的只有"经宿主守护进程 unix socket 的请求"这一类——它们不是进程自己做的
// 文件动作，而是宿主动作。这类 socket 由 BuildProfile 显式屏蔽（见
// daemon_sockets.go），所以这里没有需要降级声明的能力。
func (r *bwrapRunner) RunnerLimits() string {
	return "file effects and network are fully governed; when the policy disallows network a seccomp filter denies connect(2)/bind(2)/listen(2)/accept(2) with EPERM, so unix sockets (docker/podman/containerd/ssh-agent/dbus/…) are unreachable and loopback is unreachable too — set AllowNetwork to keep a local dev server working; host daemon sockets are additionally masked with /dev/null as defence in depth"
}

// NetworkCoverage 实现 sandbox.NetworkReporter：`--unshare-net` 完全切断网络。
func (r *bwrapRunner) NetworkCoverage() string {
	if r == nil {
		return "none"
	}
	return "full"
}
