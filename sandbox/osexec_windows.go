//go:build windows

package sandbox

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

// prepareOSExec selects the Windows restricted-token backend and labels the
// writable roots as low integrity so the sandboxed child (also low integrity)
// may write them while medium-integrity locations stay read-only. icacls is a
// built-in Windows tool; no cgo and no third-party binary is involved.
func prepareOSExec(sandboxOS *OSExec, opts OSExecOptions) error {
	if backend := strings.TrimSpace(opts.Backend); backend != "" && backend != "restricted-token" {
		return fmt.Errorf("%w: unsupported Windows backend %q (want restricted-token)", ErrOSExecUnavailable, backend)
	}
	if _, err := exec.LookPath("icacls"); err != nil {
		return fmt.Errorf("%w: icacls not found in PATH; the Windows backend needs it to label writable roots", ErrOSExecUnavailable)
	}
	for _, dir := range sandboxOS.writable {
		cmd := exec.Command("icacls", dir, "/setintegritylevel", "(OI)(CI)L")
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("sandbox: set low integrity on %q: %v: %s", dir, err, strings.TrimSpace(string(output)))
		}
	}
	if _, err := windowsSandboxToken(); err != nil {
		return err
	}
	sandboxOS.backend = "restricted-token"
	return nil
}

// wrapOSCommand runs name/args with the restricted low-integrity token through
// the standard library's SysProcAttr.Token (CreateProcessAsUser), so no cgo or
// custom process plumbing is required.
func wrapOSCommand(e *OSExec, ctx context.Context, dir, name string, args []string) (*exec.Cmd, error) {
	token, err := windowsSandboxToken()
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Token: token}
	return cmd, nil
}

// windowsSandboxToken lazily builds one restricted, low-integrity primary token
// for the process lifetime. The handle is intentionally kept until exit: a
// primary token cannot be closed while children created with it may still be
// running, and Rig does not own an OSExec lifetime hook.
var windowsSandboxToken = sync.OnceValues(func() (syscall.Token, error) {
	currentProcess, err := syscall.GetCurrentProcess()
	if err != nil {
		return 0, fmt.Errorf("sandbox: current process handle: %w", err)
	}
	var current syscall.Token
	access := uint32(syscall.TOKEN_DUPLICATE | syscall.TOKEN_ASSIGN_PRIMARY | syscall.TOKEN_QUERY | syscall.TOKEN_ADJUST_DEFAULT)
	if err := syscall.OpenProcessToken(currentProcess, access, &current); err != nil {
		return 0, fmt.Errorf("sandbox: open current process token: %w", err)
	}
	defer syscall.CloseHandle(syscall.Handle(current)) //nolint:errcheck

	advapi32 := syscall.NewLazyDLL("advapi32.dll")
	createRestrictedToken := advapi32.NewProc("CreateRestrictedToken")
	var restricted syscall.Token
	const (
		disableMaxPrivilege = 0x1
		luaToken            = 0x4
	)
	result, _, callErr := createRestrictedToken.Call(
		uintptr(current),
		uintptr(disableMaxPrivilege|luaToken),
		0, 0, 0, 0, 0,
		uintptr(unsafe.Pointer(&restricted)),
	)
	if result == 0 {
		return 0, fmt.Errorf("sandbox: CreateRestrictedToken: %w", callErr)
	}
	if err := setLowIntegrity(restricted, advapi32); err != nil {
		syscall.CloseHandle(syscall.Handle(restricted)) //nolint:errcheck
		return 0, err
	}
	return restricted, nil
})

// setLowIntegrity sets TokenIntegrityLevel to S-1-16-4096 (low).
func setLowIntegrity(token syscall.Token, advapi32 *syscall.LazyDLL) error {
	convertStringSid := advapi32.NewProc("ConvertStringSidToSidW")
	lowSIDText, err := syscall.UTF16PtrFromString("S-1-16-4096")
	if err != nil {
		return fmt.Errorf("sandbox: encode low integrity SID: %w", err)
	}
	var sid *syscall.SID
	result, _, callErr := convertStringSid.Call(uintptr(unsafe.Pointer(lowSIDText)), uintptr(unsafe.Pointer(&sid)))
	if result == 0 {
		return fmt.Errorf("sandbox: ConvertStringSidToSidW: %w", callErr)
	}
	defer func() { _, _ = syscall.LocalFree(syscall.Handle(unsafe.Pointer(sid))) }()

	type tokenMandatoryLabel struct {
		Label syscall.SIDAndAttributes
	}
	label := tokenMandatoryLabel{Label: syscall.SIDAndAttributes{Sid: sid, Attributes: 0x20}} // SE_GROUP_INTEGRITY
	setTokenInformation := advapi32.NewProc("SetTokenInformation")
	const tokenIntegrityLevel = 25
	result, _, callErr = setTokenInformation.Call(
		uintptr(token),
		uintptr(tokenIntegrityLevel),
		uintptr(unsafe.Pointer(&label)),
		unsafe.Sizeof(label),
	)
	if result == 0 {
		return fmt.Errorf("sandbox: SetTokenInformation(TokenIntegrityLevel): %w", callErr)
	}
	return nil
}
