//go:build !linux

package osrunner

import (
	"fmt"

	"github.com/zeokeel/zeokeel-rig/internal/seccomp"
)

// seccompProgram 在非 Linux 平台不可用：seccomp 是 Linux 独有的机制（darwin 用
// Seatbelt、Windows 用受限令牌 + ACL）。调用方据 error 决定 fail closed。
func seccompProgram(networkAllowed bool) (seccomp.Program, error) {
	if networkAllowed {
		return seccomp.Program{}, nil
	}
	return seccomp.Program{}, fmt.Errorf("osrunner: seccomp is only available on Linux")
}
