//go:build linux

package terminal

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// Linux ioctl 编号（asm-generic/ioctls.h）。
const (
	ioctlTIOCSPTLCK = 0x40045431 // 解锁 pty 对
	ioctlTIOCGPTN   = 0x80045430 // 获取从端 pty 编号
	ioctlTIOCSWINSZ = 0x5414     // 设置窗口大小
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

// ptySlaveName 解析已解锁 pty 的从端设备路径（/dev/pts/N）。
func ptySlaveName(master *os.File) (string, error) {
	var number uint32
	if err := ioctlPTR(master, ioctlTIOCGPTN, unsafe.Pointer(&number)); err != nil {
		return "", err
	}
	return fmt.Sprintf("/dev/pts/%d", number), nil
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
