//go:build !unix

package terminal

import (
	"errors"
	"os/exec"
)

// configureProcessGroup 在没有 POSIX 进程组的平台上为空操作。持久 shell
// 终端面向 Unix；在这些平台上信号回退为直接杀死 shell 进程（不保证子进程
// 被回收）。
func configureProcessGroup(_ *exec.Cmd) {}

// signalProcessGroup 向 shell 进程发送信号。Windows 通过 os 包只支持 kill
// 语义，因此所有信号名都按 kill 处理；返回的错误会说明非 kill 请求的情况。
func signalProcessGroup(proc process, spec signalSpec) error {
	if proc == nil || proc.Pid() == 0 {
		return errors.New("terminal: process is not running")
	}
	if spec.name != "" && spec.name != "KILL" {
		if err := proc.Kill(); err != nil {
			return err
		}
		return errors.New("terminal: only KILL is supported on this platform; the process was killed instead")
	}
	return proc.Kill()
}
