//go:build linux

package osrunner

import (
	"fmt"
	"os"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// newPlatformRunner 选择 Linux 的 runner 链：bwrap 优先，其次内置 Landlock 启动器。
//
// 每个候选都做一次真实功能探测（bwrap 运行 `true`；Landlock 运行 helper --probe
// 并自我限制），第一个可用者被选中并缓存到返回的 runner 里——New 只探测一次，
// Confine 不再探测。全部不可用时返回包装 sandbox.ErrSandboxUnavailable 的错误，
// 调用方必须拒绝执行而不是裸跑。
func newPlatformRunner(cfg Config) (sandbox.Runner, error) {
	timeout := cfg.probeTimeout()
	if path, ok := probeBwrap(timeout); ok {
		return &bwrapRunner{path: path}, nil
	}
	if abi, ok := probeLandlock(timeout); ok {
		if exe, err := os.Executable(); err == nil && exe != "" {
			return &landlockRunner{exe: exe, abi: abi}, nil
		}
	}
	return nil, fmt.Errorf("osrunner: no usable Linux sandbox runner (bwrap, Landlock): %w", sandbox.ErrSandboxUnavailable)
}

// runHelper 执行平台 helper 子命令；非 helper 参数返回 handled=false 让宿主继续
// 正常启动。
func runHelper(args []string) (handled bool, code int) {
	if len(args) == 0 || args[0] != HelperArgLandlock {
		return false, 0
	}
	return true, runLandlockHelper(args[1:])
}
