package osrunner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/zeokeel/zeokeel-rig/internal/procattr"
	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// 本文件定义 Seatbelt 后端的纯逻辑（argv/profile 构造与功能探测）。
//
// 它刻意不带 build tag：argv 与 profile 构造不依赖 macOS，因此可以在 Linux 等
// 宿主上做单元测试；只有 platform_darwin.go 的 newPlatformRunner 会实例化
// seatbeltRunner。真实执行 `sandbox-exec` 的行为测试在 darwin_test.go。

// seatbeltProbeTail 是功能探测要启动的子进程 argv（紧跟 `--` 之后）。
//
// **必须是绝对路径**：`sandbox-exec -p <profile> -- true` 里的裸 "true" 要靠
// execvp 查 PATH，而沙箱内/最小环境（CI、被宿主重设过 PATH 的进程）里 PATH 可能
// 不含 /usr/bin，于是探测报 `sandbox-exec: execvp() of 'true' failed` —— 那是
// **探测自身的 bug**，不是这台机器不能用 Seatbelt。真机 CI 上正是这样红的。
const seatbeltProbeCommand = "/usr/bin/true"

// seatbeltProbeTail 返回探测子进程的 argv。
func seatbeltProbeTail() []string {
	if _, err := os.Stat(seatbeltProbeCommand); err == nil {
		return []string{seatbeltProbeCommand}
	}
	// 极简系统上兜底：/bin/sh 一定在（绝对路径，同样不依赖 PATH）。
	return []string{"/bin/sh", "-c", "exit 0"}
}

// seatbeltRunner 是 macOS 的 sandbox-exec（Seatbelt/SBPL）runner。
type seatbeltRunner struct {
	// execPath 是选择阶段 exec.LookPath 得到的 sandbox-exec 绝对路径。
	execPath string
}

var _ sandbox.Runner = (*seatbeltRunner)(nil)

// Confine 实现 sandbox.Runner：把 argv 包装成
// `sandbox-exec -p <SeatbeltProfile> -- <原 argv>`。
//
// 失败即拒绝：未配置的 runner、nil 策略、不支持的 mode 与空 argv 都返回包装
// sandbox.ErrSandboxUnavailable 的错误，绝不返回未受限 argv。
func (r *seatbeltRunner) Confine(argv []string, policy *sandbox.Policy) (sandbox.ConfinedCommand, error) {
	if r == nil || r.execPath == "" {
		return sandbox.ConfinedCommand{}, fmt.Errorf("osrunner: seatbelt runner is not configured: %w", sandbox.ErrSandboxUnavailable)
	}
	if policy == nil {
		return sandbox.ConfinedCommand{}, fmt.Errorf("osrunner: seatbelt runner requires a policy: %w", sandbox.ErrSandboxUnavailable)
	}
	mode := policy.Mode()
	switch mode {
	case sandbox.ModeDangerFullAccess:
		// danger-full-access 是显式的无约束模式：不包 sandbox-exec，只返回拷贝。
		return passthrough(argv), nil
	case sandbox.ModeReadOnly, sandbox.ModeWorkspaceWrite:
		// 继续按 profile 包装。
	default:
		return sandbox.ConfinedCommand{}, fmt.Errorf("osrunner: seatbelt runner does not support mode %q: %w", mode, sandbox.ErrSandboxUnavailable)
	}
	if len(argv) == 0 {
		return sandbox.ConfinedCommand{}, fmt.Errorf("osrunner: seatbelt runner requires a non-empty argv: %w", sandbox.ErrSandboxUnavailable)
	}
	// Seatbelt 的 subpath 规则按访问时求值，不存在的路径也能授权；但为了让
	// 其它后端与诊断看到一致状态，这里同样补建（失败不影响 SBPL 的正确性）。
	ensureToolchainCacheRoots(policy)
	// 下面是渲染：MissingToolchainCaches 里剩下的项（创建失败）仍会被渲染成
	// subpath——SBPL 允许授权尚不存在的路径，这是它相对其它后端的优势。
	profile := SeatbeltProfile(policy)
	if profile == "" {
		// 空 profile 说明策略里出现了无法安全转义的路径（例如含换行），或策略
		// 本身不可渲染。sandbox-exec 会拒绝这种 profile，但错误信息指向 SBPL
		// 语法而不是真正的原因；这里直接 fail closed 并给出可诊断的错误。
		return sandbox.ConfinedCommand{}, fmt.Errorf("osrunner: seatbelt profile is empty (a policy path contains a control character or the policy is not renderable): %w", sandbox.ErrSandboxUnavailable)
	}
	out := make([]string, 0, len(argv)+4)
	out = append(out, r.execPath, "-p", profile, "--")
	out = append(out, argv...)
	return sandbox.ConfinedCommand{
		Argv:             out,
		Enforcement:      sandbox.EnforcementFull,
		DenialSignatures: append([]string(nil), DenialSignatures(RunnerSeatbelt)...),
	}, nil
}

// seatbeltProbePolicy 返回功能探测使用的只读策略。
//
// 探测只需要一个合法的 SBPL profile；只读 profile 不含任何宿主路径，因此探测
// 结果与调用方的工作区无关。
func seatbeltProbePolicy() *sandbox.Policy {
	return sandbox.New(sandbox.WithMode(sandbox.ModeReadOnly))
}

// probeSeatbelt 用只读 profile 运行 `sandbox-exec -p <profile> -- true`，验证
// sandbox-exec 存在、接受内联 profile 且能启动子进程；随后再做一次**拒绝自检**：
// 在同一只读 profile 下尝试写工作区外的临时文件，必须被拒绝。
//
// 第二步针对的是“API 还在、行为静默变弱”的风险：Apple 已弃用 Seatbelt 但保留
// 它，真正的风险不是移除，而是某些 SBPL 操作不再被 honor。只验证 `true` 能跑
// 无法发现这种退化。
//
// 失败时返回诊断错误：超时包装 context 错误（errors.Is 可用），非零退出附带
// 裁剪后的 stderr/stdout 一行。调用方（newPlatformRunner）负责把错误再包装成
// sandbox.ErrSandboxUnavailable。
func probeSeatbelt(ctx context.Context, execPath string) error {
	if execPath == "" {
		return fmt.Errorf("osrunner: sandbox-exec path is empty: %w", sandbox.ErrSandboxUnavailable)
	}
	profile := SeatbeltProfile(seatbeltProbePolicy())
	tail := seatbeltProbeTail()
	args := append([]string{"-p", profile, "--"}, tail...)
	cmd := exec.CommandContext(ctx, execPath, args...)
	procattr.HideWindow(cmd)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("osrunner: sandbox-exec probe failed: %w", ctxErr)
		}
		if detail := seatbeltProbeDetail(output); detail != "" {
			return fmt.Errorf("osrunner: sandbox-exec probe failed: %w (%s)", err, detail)
		}
		return fmt.Errorf("osrunner: sandbox-exec probe failed: %w", err)
	}
	return seatbeltDenialSelfTest(ctx, execPath)
}

// seatbeltDenialProbeArgv 返回拒绝自检的 argv：在只读 profile 下用 /bin/sh 尝试
// 写入 target。target 通过位置参数传入，避免路径拼接带来的引号问题。
func seatbeltDenialProbeArgv(execPath, target string) []string {
	return []string{
		execPath,
		"-p", SeatbeltProfile(seatbeltProbePolicy()),
		"--",
		"/bin/sh", "-c", `echo probe > "$1"`, "rig-sandbox-probe", target,
	}
}

// seatbeltDenialSelfTest 在只读 profile 下尝试写入一个工作区外的临时文件；写入
// 成功说明 Seatbelt 没有按预期拒绝 file-write*，此时 runner 不可信，必须 fail
// closed。
func seatbeltDenialSelfTest(ctx context.Context, execPath string) error {
	dir, err := os.MkdirTemp("", "rig-sandbox-probe-")
	if err != nil {
		return fmt.Errorf("osrunner: sandbox-exec denial self-test: create temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	target := filepath.Join(dir, "must-not-exist")

	probeArgv := seatbeltDenialProbeArgv(execPath, target)
	cmd := exec.CommandContext(ctx, probeArgv[0], probeArgv[1:]...)
	procattr.HideWindow(cmd)
	output, runErr := cmd.CombinedOutput()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("osrunner: sandbox-exec denial self-test timed out: %w", ctxErr)
	}
	if _, statErr := os.Stat(target); statErr == nil {
		if detail := seatbeltProbeDetail(output); detail != "" {
			return fmt.Errorf("osrunner: sandbox-exec denial self-test wrote %q under a read-only profile; the Seatbelt profile is not enforced (%s): %w", target, detail, sandbox.ErrSandboxUnavailable)
		}
		return fmt.Errorf("osrunner: sandbox-exec denial self-test wrote %q under a read-only profile; the Seatbelt profile is not enforced: %w", target, sandbox.ErrSandboxUnavailable)
	}
	if runErr == nil {
		// 命令退出 0 却没有写出文件：shell 行为不符合预期，同样不可信。
		return fmt.Errorf("osrunner: sandbox-exec denial self-test exited successfully without a denial: %w", sandbox.ErrSandboxUnavailable)
	}
	return nil
}

// seatbeltProbeDetail 把探测输出裁剪成一行诊断信息（最多 200 字节）。
func seatbeltProbeDetail(output []byte) string {
	text := strings.TrimSpace(string(output))
	if text == "" {
		return ""
	}
	if index := strings.IndexAny(text, "\r\n"); index >= 0 {
		text = strings.TrimSpace(text[:index])
	}
	const maxDetailBytes = 200
	if len(text) > maxDetailBytes {
		text = text[:maxDetailBytes] + "…"
	}
	return text
}

// RunnerName 实现 sandbox.Reporter。
func (r *seatbeltRunner) RunnerName() string { return RunnerSeatbelt }

// RunnerEnforcement 实现 sandbox.Reporter：Seatbelt 的 SBPL 在 file-write* 上
// 完整覆盖，且额外关掉了 system-fcntl 与 user-preference-write 两条旁路。
func (r *seatbeltRunner) RunnerEnforcement() sandbox.Enforcement { return sandbox.EnforcementFull }

// RunnerLimits 实现 sandbox.Reporter。
func (r *seatbeltRunner) RunnerLimits() string { return "" }

// NetworkCoverage 实现 sandbox.NetworkReporter：SBPL 的 `(deny network*)` 覆盖
// 全部网络操作（Unix socket 的创建/连接除外——那属于 file 面）。
func (r *seatbeltRunner) NetworkCoverage() string {
	if r == nil {
		return "none"
	}
	return "full"
}
