//go:build freebsd || netbsd || openbsd || dragonfly

package terminal

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// BSD ioctl numbers. They are the FreeBSD/NetBSD TIOCPTMGET-era values and are
// compile-checked only: PTY support on BSD is best effort and was not tested on
// real hardware. If an ioctl fails at runtime the Manager falls back to the
// pipe transport automatically.
const (
	ioctlTIOCSPTLCK = 0x80047410 // unlock the pty pair
	ioctlTIOCGPTN   = 0x4004740f // get the slave pty number
	ioctlTIOCSWINSZ = 0x80087467 // set window size
)

// ptyWinsize mirrors struct winsize.
type ptyWinsize struct {
	Row    uint16
	Col    uint16
	Xpixel uint16
	Ypixel uint16
}

// unlockPTY clears the TIOCSPTLCK lock so the slave can be opened.
func unlockPTY(master *os.File) error {
	var unlock int32
	return ioctlPTR(master, ioctlTIOCSPTLCK, unsafe.Pointer(&unlock))
}

// ptySlaveName resolves the slave device path through TIOCGPTN.
func ptySlaveName(master *os.File) (string, error) {
	var number uint32
	if err := ioctlPTR(master, ioctlTIOCGPTN, unsafe.Pointer(&number)); err != nil {
		return "", err
	}
	return fmt.Sprintf("/dev/pts/%d", number), nil
}

// setPTYSize applies an initial window size to the pty.
func setPTYSize(pty *os.File, cols, rows int) error {
	size := ptyWinsize{Row: uint16(rows), Col: uint16(cols)}
	return ioctlPTR(pty, ioctlTIOCSWINSZ, unsafe.Pointer(&size))
}

// ioctlPTR issues one pointer-argument ioctl on f.
func ioctlPTR(f *os.File, request uintptr, arg unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), request, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}
