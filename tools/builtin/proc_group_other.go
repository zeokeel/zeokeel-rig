//go:build !unix

package builtin

import (
	"errors"
	"os"
	"os/exec"
)

// 前台 shell 的进程组治理（没有 POSIX 进程组的平台）。
//
// Windows 上没有可复用的 Job 对象路径：tools/builtin 的前台执行直接 exec
// 解析后的 argv，不经过 sandbox/osrunner 的 Job 实现。因此这里退回"杀直接
// 子进程"，与修复前行为一致；要治理整个进程树，宿主必须注入
// builtin.WithRunner(sandbox/osrunner)——Confine 之后由 runner 负责进程树
// （那是 opt-in 的 OS 沙箱路径，见 sandbox/osrunner 的文档）。

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
