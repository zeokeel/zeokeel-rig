//go:build darwin

package osrunner

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// newPlatformRunner 选择 macOS 的 runner：系统 sandbox-exec（Seatbelt/SBPL）。
//
// 选择时用只读 profile 做一次功能探测（`sandbox-exec -p <profile> -- true`）。
// 二进制缺失、不可执行、profile 被拒或探测超时都返回包装
// sandbox.ErrSandboxUnavailable 的错误——调用方必须拒绝执行而不是裸跑。
//
// 探测成功后被选中的 runner 被缓存：New 只选择/探测一次，返回的 runner 的
// Confine 不再探测。
func newPlatformRunner(cfg Config) (sandbox.Runner, error) {
	execPath, err := exec.LookPath("sandbox-exec")
	if err != nil {
		return nil, fmt.Errorf("osrunner: sandbox-exec not found: %w: %w", err, sandbox.ErrSandboxUnavailable)
	}
	// 固定成绝对路径：argv[0] 不带分隔符时 exec.Command 会再次查 PATH，而 PATH
	// 在探测之后可能被调用方修改。
	absolute, err := filepath.Abs(execPath)
	if err != nil {
		return nil, fmt.Errorf("osrunner: resolve sandbox-exec path %q: %w: %w", execPath, err, sandbox.ErrSandboxUnavailable)
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.probeTimeout())
	defer cancel()
	if err := probeSeatbelt(ctx, absolute); err != nil {
		return nil, fmt.Errorf("osrunner: sandbox-exec is not usable: %w: %w", err, sandbox.ErrSandboxUnavailable)
	}
	return &seatbeltRunner{execPath: absolute}, nil
}

// runHelper 在 macOS 上没有 helper（sandbox-exec 直接作为 argv 前缀执行）。
func runHelper(args []string) (handled bool, code int) {
	return false, 0
}
