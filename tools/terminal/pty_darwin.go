//go:build darwin

package terminal

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// Darwin ioctl numbers (sys/ttycom.h, sys/ioctl.h). They differ from Linux:
// the direction bits are part of the number.
const (
	ioctlTIOCSPTLCK   = 0x80045431 // unlock the pty pair
	ioctlTIOCPTYGNAME = 0x40807453 // get the slave device name
	ioctlTIOCSWINSZ   = 0x80087467 // set window size
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

// ptySlaveName resolves the slave device path on Darwin (e.g. /dev/ttys001)
// through TIOCPTYGNAME. Not tested on darwin hardware; if the ioctl number is
// wrong PTY setup fails and the Manager falls back to the pipe transport.
func ptySlaveName(master *os.File) (string, error) {
	var name [128]byte
	if err := ioctlPTR(master, ioctlTIOCPTYGNAME, unsafe.Pointer(&name[0])); err != nil {
		return "", err
	}
	end := 0
	for end < len(name) && name[end] != 0 {
		end++
	}
	if end == 0 {
		return "", fmt.Errorf("TIOCPTYGNAME returned an empty slave name")
	}
	return string(name[:end]), nil
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
