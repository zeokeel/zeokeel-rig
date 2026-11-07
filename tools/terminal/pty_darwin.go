//go:build darwin

package terminal

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// Darwin ioctl 编号（sys/ttycom.h、sys/ioctl.h）。它们与 Linux 不同：
// 方向位是编号的一部分。
const (
	ioctlTIOCSPTLCK   = 0x80045431 // 解锁 pty 对
	ioctlTIOCPTYGNAME = 0x40807453 // 获取从端设备名
	ioctlTIOCSWINSZ   = 0x80087467 // 设置窗口大小
)

// ptyWinsize 对应 struct winsize。
type ptyWinsize struct {
	Row    uint16
	Col    uint16
	Xpixel uint16
	Ypixel uint16
}

// unlockPTY 清除 TIOCSPTLCK 锁，使从端可以被打开。
func unlockPTY(master *os.File) error {
	var unlock int32
	return ioctlPTR(master, ioctlTIOCSPTLCK, unsafe.Pointer(&unlock))
}

// ptySlaveName 在 Darwin 上通过 TIOCPTYGNAME 解析从端设备路径（例如
// /dev/ttys001）。未在 darwin 真机上测试；若 ioctl 编号有误，PTY 初始化会
// 失败，Manager 会回退到 pipe 传输。
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

// setPTYSize 为 pty 应用初始窗口大小。
func setPTYSize(pty *os.File, cols, rows int) error {
	size := ptyWinsize{Row: uint16(rows), Col: uint16(cols)}
	return ioctlPTR(pty, ioctlTIOCSWINSZ, unsafe.Pointer(&size))
}

// ioctlPTR 在 f 上执行一次指针参数 ioctl。
func ioctlPTR(f *os.File, request uintptr, arg unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), request, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}
