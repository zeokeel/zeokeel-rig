package terminal

import "os/exec"

// process 是终端会话背后的操作系统进程。Unix 与 Windows ConPTY 都实现它，
// 使 waitLoop/signal/close 不必依赖 *exec.Cmd（Windows ConPTY 通过
// CreateProcessW 启动，拿不到 exec.Cmd）。
type process interface {
	// Wait 回收进程并记录退出状态；只能调用一次。
	Wait() error
	// Kill 强制结束进程。
	Kill() error
	// Pid 返回进程 id。
	Pid() int
	// ExitCode 在 Wait 返回后报告退出码（未知时为 -1）。
	ExitCode() int
}

// execProcess 是普通 os/exec.Cmd 的 process 适配器（Unix PTY 与 pipe 会话）。
type execProcess struct {
	cmd *exec.Cmd
}

func (p execProcess) Wait() error { return p.cmd.Wait() }

func (p execProcess) Kill() error {
	if p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	return p.cmd.Process.Kill()
}

func (p execProcess) Pid() int {
	if p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

func (p execProcess) ExitCode() int {
	if p.cmd == nil || p.cmd.ProcessState == nil {
		return -1
	}
	return p.cmd.ProcessState.ExitCode()
}
