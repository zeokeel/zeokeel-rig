//go:build linux

package osrunner

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// bwrapRunner 用系统 bubblewrap（bwrap）包装目标命令。
//
// bwrap 走用户命名空间 + 只读 bind 的方式：read-only 把整个宿主根只读绑定，
// workspace-write 额外挂一个私有 /tmp 并把策略根以读写 bind 挂回原路径。
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

	trailer := append(BwrapArgs(policy), "--")
	return wrapped(
		[]string{r.path},
		trailer,
		argv,
		sandbox.EnforcementFull,
		DenialSignatures(RunnerBwrap),
	), nil
}

// probeBwrap 对系统 bwrap 做一次功能探测：用只读 profile 启动 `true`。
//
// 仅仅存在 /usr/bin/bwrap 不代表可用（容器里可能因为禁止用户命名空间而失败），
// 因此选择 runner 前必须真实运行一次。返回解析出的 bwrap 路径与是否可用。
func probeBwrap(timeout time.Duration) (string, bool) {
	path, err := exec.LookPath("bwrap")
	if err != nil {
		return "", false
	}
	args := append([]string(nil), BwrapArgs(sandbox.New(sandbox.WithMode(sandbox.ModeReadOnly)))...)
	args = append(args, "--", "true")
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := exec.CommandContext(ctx, path, args...).Run(); err != nil {
		return "", false
	}
	return path, true
}
