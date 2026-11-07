//go:build linux

package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"syscall"
	"unsafe"
)

// Linux Landlock/seccomp syscall numbers are stable across the mainstream
// architectures (the generic syscall table; mips variants use 5000+ and are not
// supported natively — they fall back to the bwrap backend or fail closed).
const (
	sysLandlockCreateRuleset = 444
	sysLandlockAddRule       = 445
	sysLandlockRestrictSelf  = 446

	landlockCreateRulesetVersion = 1
	landlockRulePathBeneath      = 1

	landlockAccessWriteFile  = 1 << 1
	landlockAccessRemoveDir  = 1 << 4
	landlockAccessRemoveFile = 1 << 5
	landlockAccessMakeChar   = 1 << 6
	landlockAccessMakeDir    = 1 << 7
	landlockAccessMakeReg    = 1 << 8
	landlockAccessMakeSock   = 1 << 9
	landlockAccessMakeFifo   = 1 << 10
	landlockAccessMakeBlock  = 1 << 11
	landlockAccessMakeSym    = 1 << 12
	landlockAccessRefer      = 1 << 13
	landlockAccessTruncate   = 1 << 14

	oPath = 0x200000 // O_PATH, not exposed by the syscall package on all arches
)

type landlockRulesetAttr struct {
	handledAccessFS uint64
}

type landlockPathBeneathAttr struct {
	allowedAccess uint64
	parentFD      int32
	_             uint32
}

// init is the sandbox runner: when the parent re-execs this binary with the
// hidden marker, apply the sandbox before any other package/user code runs and
// replace the process with the real command.
func init() {
	if len(os.Args) < 4 || os.Args[1] != nativeSandboxHelperArg {
		return
	}
	runNativeSandboxHelper(os.Args[2], os.Args[3:])
}

// runNativeSandboxHelper never returns: it applies Landlock + seccomp and
// execs the requested command, or exits 126 with a diagnostic on stderr.
func runNativeSandboxHelper(specJSON string, command []string) {
	fail := func(err error) {
		fmt.Fprintln(os.Stderr, "rig os sandbox:", err)
		os.Exit(126)
	}
	var spec nativeSandboxSpec
	if err := json.Unmarshal([]byte(specJSON), &spec); err != nil {
		fail(fmt.Errorf("decode spec: %w", err))
	}
	if err := preflightNativeSandbox(spec.AllowNetwork); err != nil {
		fail(err)
	}
	if len(command) < 2 || command[0] != "--" || command[1] == "" {
		fail(errors.New("missing command"))
	}
	argv := command[1:]
	target, err := exec.LookPath(argv[0])
	if err != nil {
		fail(fmt.Errorf("resolve %q: %w", argv[0], err))
	}
	if err := applyNativeLandlock(spec.Writable); err != nil {
		fail(err)
	}
	if !spec.AllowNetwork {
		if err := applySeccompNetworkFilter(); err != nil {
			fail(err)
		}
	}
	if err := syscall.Exec(target, argv, os.Environ()); err != nil {
		fail(fmt.Errorf("exec %q: %w", target, err))
	}
}

// preflightNativeSandbox verifies kernel support before a command is spawned, so
// NewOSExec fails closed instead of producing a process that dies later.
func preflightNativeSandbox(allowNetwork bool) error {
	abi, err := landlockABI()
	if err != nil {
		return fmt.Errorf("%w: Landlock unavailable (%v); pass Backend %q to use bubblewrap", ErrOSExecUnavailable, err, bwrapBackend)
	}
	_ = abi
	if !allowNetwork && !nativeSandboxSeccompSupported() {
		return fmt.Errorf("%w: the Landlock backend's network filter supports linux/amd64 and linux/arm64, not %s; pass Backend %q", ErrOSExecUnavailable, runtime.GOARCH, bwrapBackend)
	}
	return nil
}

// landlockABI queries the kernel Landlock ABI (>=1 means supported).
func landlockABI() (int, error) {
	abi, _, errno := syscall.Syscall(sysLandlockCreateRuleset, 0, 0, landlockCreateRulesetVersion)
	if errno != 0 {
		return 0, errno
	}
	if abi < 1 {
		return 0, fmt.Errorf("unexpected ABI %d", abi)
	}
	return int(abi), nil
}

// applyNativeLandlock restricts writes to the configured paths; reads stay
// available (same read semantics as the previous ro-bind / sandbox).
func applyNativeLandlock(writable []string) error {
	abi, err := landlockABI()
	if err != nil {
		return fmt.Errorf("landlock: %w", err)
	}
	handled := uint64(landlockAccessWriteFile | landlockAccessRemoveDir | landlockAccessRemoveFile |
		landlockAccessMakeChar | landlockAccessMakeDir | landlockAccessMakeReg |
		landlockAccessMakeSock | landlockAccessMakeFifo | landlockAccessMakeBlock | landlockAccessMakeSym)
	if abi >= 2 {
		handled |= landlockAccessRefer
	}
	if abi >= 3 {
		handled |= landlockAccessTruncate
	}
	attr := landlockRulesetAttr{handledAccessFS: handled}
	rulesetFD, _, errno := syscall.Syscall(sysLandlockCreateRuleset, uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	if errno != 0 {
		return fmt.Errorf("create ruleset: %w", errno)
	}
	defer syscall.Close(int(rulesetFD)) //nolint:errcheck

	paths := slices.Clone(writable)
	paths = append(paths, "/tmp", "/dev/null", "/dev/tty", "/dev/zero", "/dev/urandom")
	seen := make(map[string]bool, len(paths))
	for _, path := range paths {
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		if err := addLandlockPathRule(int(rulesetFD), path, abi, handled); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // optional path such as /dev/tty on a minimal host
			}
			return err
		}
	}
	if _, _, errno := syscall.Syscall(sysLandlockRestrictSelf, rulesetFD, 0, 0); errno != 0 {
		return fmt.Errorf("restrict self: %w", errno)
	}
	return nil
}

// addLandlockPathRule allows the configured write access beneath path.
func addLandlockPathRule(rulesetFD int, path string, abi int, handled uint64) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	allowed := handled
	if !info.IsDir() {
		// File rules only accept file-applicable access bits.
		allowed = landlockAccessWriteFile
		if abi >= 3 && info.Mode().IsRegular() {
			allowed |= landlockAccessTruncate
		}
	}
	fd, err := syscall.Open(path, oPath|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open O_PATH %q: %w", path, err)
	}
	defer syscall.Close(fd) //nolint:errcheck
	rule := landlockPathBeneathAttr{allowedAccess: allowed, parentFD: int32(fd)}
	if _, _, errno := syscall.Syscall6(sysLandlockAddRule, uintptr(rulesetFD), landlockRulePathBeneath, uintptr(unsafe.Pointer(&rule)), 0, 0, 0); errno != 0 {
		return fmt.Errorf("add rule %q: %w", path, errno)
	}
	return nil
}

// Seccomp constants (stable UAPI values).
const (
	bpfLD  = 0x00
	bpfW   = 0x00
	bpfABS = 0x20
	bpfJMP = 0x05
	bpfJEQ = 0x10
	bpfK   = 0x00
	bpfRET = 0x06

	seccompRetAllow   = 0x7fff0000
	seccompRetErrno   = 0x00050000 | 0x1 // SECCOMP_RET_ERRNO | EPERM
	seccompModeFilter = 2

	prSetNoNewPrivs = 38
	prSetSeccomp    = 22
)

type seccompFilter struct {
	code uint16
	jt   uint8
	jf   uint8
	k    uint32
}

type seccompProg struct {
	len    uint16
	filter *seccompFilter
}

// ioUringSetupNR returns the io_uring_setup syscall number (not exposed by the
// syscall package). Blocking it prevents socket creation without socket(2).
func ioUringSetupNR() (uint32, error) {
	switch runtime.GOARCH {
	case "amd64", "arm64":
		return 425, nil
	default:
		return 0, fmt.Errorf("unsupported architecture %s", runtime.GOARCH)
	}
}

// auditArch returns the seccomp_data.arch constant for the running ABI.
func auditArch() (uint32, error) {
	switch runtime.GOARCH {
	case "amd64":
		return 0xc000003e, nil // AUDIT_ARCH_X86_64
	case "arm64":
		return 0xc00000b7, nil // AUDIT_ARCH_AARCH64
	default:
		return 0, fmt.Errorf("unsupported architecture %s", runtime.GOARCH)
	}
}

// applySeccompNetworkFilter denies creating AF_INET/AF_INET6 sockets and denies
// the io_uring setup path (which could otherwise create sockets without the
// socket syscall). AF_UNIX/AF_NETLINK stay allowed for local IPC/DNS plumbing.
func applySeccompNetworkFilter() error {
	arch, err := auditArch()
	if err != nil {
		return err
	}
	if err := prctl(prSetNoNewPrivs, 1); err != nil {
		return fmt.Errorf("set no_new_privs: %w", err)
	}
	socketNR := uint32(syscall.SYS_SOCKET)
	uringNR, err := ioUringSetupNR()
	if err != nil {
		return err
	}
	program := []seccompFilter{
		{code: bpfLD | bpfW | bpfABS, k: 4},                 // A = seccomp_data.arch
		{code: bpfJMP | bpfJEQ | bpfK, jf: 8, k: arch},      // wrong ABI -> idx10 ERRNO
		{code: bpfLD | bpfW | bpfABS, k: 0},                 // A = nr
		{code: bpfJMP | bpfJEQ | bpfK, jt: 6, k: uringNR},   // io_uring_setup -> idx10
		{code: bpfJMP | bpfJEQ | bpfK, jf: 4, k: socketNR},  // not socket -> idx9 ALLOW
		{code: bpfLD | bpfW | bpfABS, k: 16},                // A = arg0 (domain)
		{code: bpfJMP | bpfJEQ | bpfK, jt: 3, k: 2},         // AF_INET -> idx10
		{code: bpfJMP | bpfJEQ | bpfK, jt: 2, jf: 1, k: 10}, // AF_INET6 -> idx10 else idx9
		{code: bpfRET | bpfK, k: seccompRetErrno},           // idx8 (unused)
		{code: bpfRET | bpfK, k: seccompRetAllow},           // idx9
		{code: bpfRET | bpfK, k: seccompRetErrno},           // idx10
	}
	prog := seccompProg{len: uint16(len(program)), filter: &program[0]}
	if err := prctl(prSetSeccomp, seccompModeFilter, uintptr(unsafe.Pointer(&prog))); err != nil {
		return fmt.Errorf("install seccomp filter: %w", err)
	}
	return nil
}

// prctl wraps prctl(2) with the common two-argument form.
func prctl(option int, args ...uintptr) error {
	var a2, a3, a4, a5 uintptr
	switch len(args) {
	case 1:
		a2 = args[0]
	case 2:
		a2, a3 = args[0], args[1]
	case 3:
		a2, a3, a4 = args[0], args[1], args[2]
	case 4:
		a2, a3, a4, a5 = args[0], args[1], args[2], args[3]
	}
	_, _, errno := syscall.Syscall6(syscall.SYS_PRCTL, uintptr(option), a2, a3, a4, a5, 0)
	if errno != 0 {
		return errno
	}
	return nil
}
