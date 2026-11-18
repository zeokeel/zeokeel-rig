//go:build linux

package seccomp

import (
	"fmt"
	"syscall"
	"unsafe"
)

const (
	// seccompSetModeFilter 是 SECCOMP_SET_MODE_FILTER 操作码。
	seccompSetModeFilter = 1
	// seccompSyscall 是 seccomp(2) 的系统调用号（asm-generic，amd64/arm64 相同）。
	seccompSyscall = 317
	// prSetNoNewPrivileges 是 prctl(PR_SET_NO_NEW_PRIVS)。
	prSetNoNewPrivileges = 38
)

// sockFprog 是 struct sock_fprog（内核用它描述一个过滤器程序）。
type sockFprog struct {
	len    uint16
	filter uintptr
}

// Install 把程序装到**当前进程**上（不可撤销，且对所有后代生效）。
//
// 先 `prctl(PR_SET_NO_NEW_PRIVS, 1)`：没有它内核拒绝限制（否则 setuid 程序可
// 重获特权）。装不上时返回错误，调用方必须 fail closed——绝不在"以为装了"的
// 状态下 exec。
func Install(program Program) error {
	if program.Empty() {
		return nil
	}
	raw := program.Bytes()
	if len(raw)%8 != 0 || len(raw) == 0 {
		return fmt.Errorf("seccomp: program length %d is not a multiple of 8", len(raw))
	}
	if _, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, prSetNoNewPrivileges, 1, 0, 0, 0, 0); errno != 0 {
		return fmt.Errorf("seccomp: prctl(PR_SET_NO_NEW_PRIVS): %w", errno)
	}
	prog := sockFprog{len: uint16(len(raw) / 8), filter: uintptr(unsafe.Pointer(&raw[0]))}
	if _, _, errno := syscall.RawSyscall(seccompSyscall, seccompSetModeFilter, 0, uintptr(unsafe.Pointer(&prog))); errno != 0 {
		return fmt.Errorf("seccomp: seccomp(SECCOMP_SET_MODE_FILTER): %w", errno)
	}
	return nil
}
