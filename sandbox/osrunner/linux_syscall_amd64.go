//go:build linux && amd64

package osrunner

import (
	"syscall"
	"unsafe"
)

// Linux 4.13 起的 Landlock 系统调用号。amd64 与 arm64 都使用 asm-generic 表，
// 但仍按架构分文件：一旦某架构的编号或调用约定不同，只需改对应文件，上层规则
// 构建代码不受影响。
const (
	landlockCreateRulesetSyscall = 444
	landlockAddRuleSyscall       = 445
	landlockRestrictSelfSyscall  = 446
)

// landlockSupported 报告当前架构是否实现 Landlock 原始系统调用。
const landlockSupported = true

// landlockCreateRuleset 调用 landlock_create_ruleset(2)。
//
// attr 为 nil、size 为 0、flags 为 LANDLOCK_CREATE_RULESET_VERSION 时，返回值是
// 内核协商到的 ABI 版本；否则返回值是新建 ruleset 的文件描述符。
func landlockCreateRuleset(attr unsafe.Pointer, size uintptr, flags uint32) (int, error) {
	value, _, errno := syscall.RawSyscall(landlockCreateRulesetSyscall, uintptr(attr), size, uintptr(flags))
	if errno != 0 {
		return -1, errno
	}
	return int(value), nil
}

// landlockAddRule 调用 landlock_add_rule(2)，向 ruleset 添加一条 path-beneath 规则。
func landlockAddRule(rulesetFD int, ruleType uint32, attr unsafe.Pointer, flags uint32) error {
	_, _, errno := syscall.RawSyscall6(landlockAddRuleSyscall, uintptr(rulesetFD), uintptr(ruleType), uintptr(attr), uintptr(flags), 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// landlockRestrictSelf 调用 landlock_restrict_self(2)，把 ruleset 施加到当前进程。
func landlockRestrictSelf(rulesetFD int, flags uint32) error {
	_, _, errno := syscall.RawSyscall(landlockRestrictSelfSyscall, uintptr(rulesetFD), uintptr(flags), 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// landlockSetNoNewPrivileges 调用 prctl(PR_SET_NO_NEW_PRIVS, 1)。
//
// 它是 landlock_restrict_self(2) 的前置条件：没有 no_new_privs 时内核拒绝自我
// 限制，否则 setuid 程序可能重新获得被限制的能力。
func landlockSetNoNewPrivileges() error {
	const prSetNoNewPrivileges = 38
	_, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, prSetNoNewPrivileges, 1, 0, 0, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}
