//go:build !linux && !darwin && !windows

package osrunner

import (
	"fmt"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// newPlatformRunner 在不支持的平台上 fail closed。
func newPlatformRunner(cfg Config) (sandbox.Runner, error) {
	_ = cfg
	return nil, fmt.Errorf("osrunner: no OS sandbox backend on this platform: %w", sandbox.ErrSandboxUnavailable)
}

// runHelper 在不支持的平台上没有 helper。
func runHelper(args []string) (handled bool, code int) {
	return false, 0
}
