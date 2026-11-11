//go:build windows

package terminal

import (
	"errors"
	"os"
	"os/exec"
)

// errPTYUnsupported 说明 Windows 上的限制；ConPTY 超出 v0.5 范围，因此
// 该平台的 Manager 自动使用 pipe 传输。
var errPTYUnsupported = errors.New("terminal: PTY transport requires ConPTY, which is not implemented in v0.5; using pipe")

// ptySupported 报告当前构建能否打开伪终端。
func ptySupported() bool { return false }

// startPTYCommand 在 Windows 上总是失败。
func startPTYCommand(_ []string, _ string, _ []string, _, _ int) (*os.File, *exec.Cmd, error) {
	return nil, nil, errPTYUnsupported
}
