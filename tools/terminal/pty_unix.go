//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package terminal

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// errPTYUnsupported is returned when the platform has no PTY implementation.
var errPTYUnsupported = errors.New("terminal: PTY transport is not supported on this platform")

// ptySupported reports whether this build can open a pseudo-terminal.
func ptySupported() bool { return true }

// startPTYCommand opens a PTY pair, starts shellPath interactively as the PTY's
// session leader (stdin/stdout/stderr all the slave end), and returns the master
// end. The caller owns the master *os.File.
//
// The child gets Setsid + Setctty so the slave becomes its controlling terminal;
// that is what makes interactive programs, job control and Ctrl-C work.
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
	// The child owns the slave now; the parent must close its copy or the master
	// read never sees EOF/EIO when the shell exits.
	_ = slave.Close()
	return master, cmd, nil
}
