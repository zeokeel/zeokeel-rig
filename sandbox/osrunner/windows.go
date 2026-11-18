//go:build windows

package osrunner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig/internal/procattr"
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
	cmd := exec.CommandContext(probing, executable, HelperArgWindowsACL, winCommandProbe)
	procattr.HideWindow(cmd)
	output, err := cmd.CombinedOutput()
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
	// ACL 需要一个真实存在的对象：先补建缺失的缓存目录（只建目录、0700；
	// 失败不阻断）。
	ensureToolchainCacheRoots(policy)
	return buildWindowsConfinedCommand(r.executable, argv, policy)
}

// RunnerName 实现 sandbox.Reporter。
func (r *windowsACLRunner) RunnerName() string { return RunnerWindowsACL }

// RunnerEnforcement 实现 sandbox.Reporter：受限令牌 + 目录 ACL 只治理**写/删**，
// 读与网络不受限，因此恒为 partial。
func (r *windowsACLRunner) RunnerEnforcement() sandbox.Enforcement {
	return sandbox.EnforcementPartial
}

// RunnerLimits 实现 sandbox.Reporter：把 partial 的具体含义说清楚——这是
// sandbox_status 与拒绝归因要直接展示给模型的文字。
func (r *windowsACLRunner) RunnerLimits() string {
	return "writes/deletes are denied by ACL, but reads and network are not confined, and a hard link to an allowed file is another writable alias (NTFS does not expose link count to the helper)"
}

// NetworkCoverage 实现 sandbox.NetworkReporter：受限令牌 + 目录 ACL 只治理写/删，
// 网络完全不受限。
func (r *windowsACLRunner) NetworkCoverage() string {
	if r == nil {
		return "none"
	}
	return "none"
}
