//go:build !linux

package seccomp

import "fmt"

// Install 在非 Linux 平台不可用。
//
// seccomp 是 Linux 独有的机制；darwin 用 Seatbelt、Windows 用受限令牌 + ACL，
// 它们各自有对应的降级声明（见 osrunner 的 RunnerLimits）。
func Install(Program) error {
	return fmt.Errorf("seccomp: not supported on this platform")
}
