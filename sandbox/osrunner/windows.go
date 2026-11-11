//go:build windows

package osrunner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// windowsACLRunner 是 Windows 上的 ACL 受限令牌 runner。
//
// Confine 只做纯 argv 包装（宿主可执行文件 + 隐藏 helper 参数 + 目标 argv）；
// 真正的令牌/ACL 工作在 helper 子进程里执行，因此每次 Confine 都是独立的一次
// 运行，没有持久 grant。
type windowsACLRunner struct {
	executable   string
	probeTimeout time.Duration
}

var _ sandbox.Runner = (*windowsACLRunner)(nil)

// newWindowsACLRunner 选择 Windows runner 并做一次功能探测。
//
// 探测通过隐藏 helper 的 --probe 完整跑一遍“受限令牌 + ACL 授权 + 受限 spawn”；
// 任何失败都返回包装 sandbox.ErrSandboxUnavailable 的错误（fail closed）。
func newWindowsACLRunner(cfg Config) (*windowsACLRunner, error) {
	if !winLayoutSupported {
		return nil, fmt.Errorf("osrunner: windows acl runner requires 64-bit Windows (Win32 ACL struct layout mismatch): %w", sandbox.ErrSandboxUnavailable)
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("osrunner: locate host executable for the windows acl helper: %v: %w", err, sandbox.ErrSandboxUnavailable)
	}
	probeTimeout := cfg.probeTimeout()
	probing, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	output, err := exec.CommandContext(probing, executable, HelperArgWindowsACL, winCommandProbe).CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(output))
		if detail == "" {
			detail = "no helper output"
		}
		return nil, fmt.Errorf("osrunner: windows acl probe failed (is osrunner.MaybeRunHelper called at the start of main?): %v: %s: %w", err, detail, sandbox.ErrSandboxUnavailable)
	}
	return &windowsACLRunner{executable: executable, probeTimeout: probeTimeout}, nil
}

// Confine 实现 sandbox.Runner。
func (r *windowsACLRunner) Confine(argv []string, policy *sandbox.Policy) (sandbox.ConfinedCommand, error) {
	return buildWindowsConfinedCommand(r.executable, argv, policy)
}
