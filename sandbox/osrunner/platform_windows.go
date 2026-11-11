//go:build windows

package osrunner

import (
	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// newPlatformRunner 选择 Windows 的 runner：受限令牌 + 目录 ACL 的隐藏 helper。
func newPlatformRunner(cfg Config) (sandbox.Runner, error) {
	return newWindowsACLRunner(cfg)
}

// runHelper 执行 Windows 受限令牌 helper。
func runHelper(args []string) (handled bool, code int) {
	return runWindowsACLHelper(args)
}
