//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package terminal

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// ptySupported 报告当前构建能否打开伪终端。
func ptySupported() bool { return true }

// startPTYCommand 打开一对 PTY，把 shellPath 以交互方式启动为 PTY 的
// 会话首进程（stdin/stdout/stderr 都指向从端），并返回主端。主端 *os.File
// 归调用方所有。
//
// 子进程获得 Setsid + Setctty，使从端成为它的控制终端；交互式程序、作业
// 控制与 Ctrl-C 正是由此工作。
func startPTYCommand(shellPath, workdir string, env []string, cols, rows int) (*os.File, *exec.Cmd, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}
	fail := func(err error) (*os.File, *exec.Cmd, error) {
		_ = master.Close()
		return nil, nil, err
	}
	if err := unlockPTY(master); err != nil {
		return fail(fmt.Errorf("unlock pty: %w", err))
	}
	if err := setPTYSize(master, cols, rows); err != nil {
		return fail(fmt.Errorf("set pty window size: %w", err))
	}
	slaveName, err := ptySlaveName(master)
	if err != nil {
		return fail(fmt.Errorf("pty slave name: %w", err))
	}
	slave, err := os.OpenFile(slaveName, os.O_RDWR, 0)
	if err != nil {
		return fail(fmt.Errorf("open pty slave %q: %w", slaveName, err))
	}

	cmd := exec.Command(shellPath, "-i")
	cmd.Dir = workdir
	cmd.Env = withDefaultEnv(env, "TERM", "xterm-256color")
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		_ = master.Close()
		_ = slave.Close()
		return nil, nil, fmt.Errorf("start pty shell: %w", err)
	}
	// 从端现在归子进程所有；父进程必须关闭自己的副本，否则 shell 退出时
	// 主端读取永远看不到 EOF/EIO。
	_ = slave.Close()
	return master, cmd, nil
}
