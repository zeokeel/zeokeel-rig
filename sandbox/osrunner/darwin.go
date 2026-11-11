package osrunner

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// 本文件定义 Seatbelt 后端的纯逻辑（argv/profile 构造与功能探测）。
//
// 它刻意不带 build tag：argv 与 profile 构造不依赖 macOS，因此可以在 Linux 等
// 宿主上做单元测试；只有 platform_darwin.go 的 newPlatformRunner 会实例化
// seatbeltRunner。真实执行 `sandbox-exec` 的行为测试在 darwin_test.go。

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
	out := make([]string, 0, len(argv)+4)
	out = append(out, r.execPath, "-p", SeatbeltProfile(policy), "--")
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
// sandbox-exec 存在、接受内联 profile 且能启动子进程。
//
// 失败时返回诊断错误：超时包装 context 错误（errors.Is 可用），非零退出附带
// 裁剪后的 stderr/stdout 一行。调用方（newPlatformRunner）负责把错误再包装成
// sandbox.ErrSandboxUnavailable。
func probeSeatbelt(ctx context.Context, execPath string) error {
	if execPath == "" {
		return fmt.Errorf("osrunner: sandbox-exec path is empty: %w", sandbox.ErrSandboxUnavailable)
	}
	profile := SeatbeltProfile(seatbeltProbePolicy())
	cmd := exec.CommandContext(ctx, execPath, "-p", profile, "--", "true")
	output, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("osrunner: sandbox-exec probe failed: %w", ctxErr)
	}
	if detail := seatbeltProbeDetail(output); detail != "" {
		return fmt.Errorf("osrunner: sandbox-exec probe failed: %w (%s)", err, detail)
	}
	return fmt.Errorf("osrunner: sandbox-exec probe failed: %w", err)
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
