//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly && !windows

package terminal

import (
	"errors"
	"os"
	"os/exec"
)

// errPTYUnsupported 在没有 PTY 实现的平台上返回。
var errPTYUnsupported = errors.New("terminal: PTY transport is not supported on this platform; using pipe")

// ptySupported 报告当前构建能否打开伪终端。
func ptySupported() bool { return false }

// startPTYCommand 在不支持的平台上总是失败。
func startPTYCommand(_ []string, _ string, _ []string, _, _ int) (*ptyEnd, process, error) {
	return nil, nil, errPTYUnsupported
}
