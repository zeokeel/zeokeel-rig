//go:build !unix && !windows

package builtin

import (
	"errors"
	"os"
	"os/exec"
)

// 前台 shell 的进程组治理（既没有 POSIX 进程组、也没有 taskkill 的平台：
// plan9、js/wasm 等）。
//
// Windows 走 proc_group_windows.go（taskkill /T /F + Process.Kill 兜底）。
// 在这些平台上前台超时只能杀直接子进程 —— 要治理整个进程树，宿主必须注入
// builtin.WithRunner(sandbox/osrunner)。

// configureForegroundProcessGroup 在没有 POSIX 进程组的平台上为空操作。
func configureForegroundProcessGroup(_ *exec.Cmd) {}

// killForegroundProcessGroup 回退为杀掉直接子进程（孙进程不保证被回收）。
func killForegroundProcessGroup(command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return nil
	}
	if err := command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}
