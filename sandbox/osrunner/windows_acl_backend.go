//go:build windows

package osrunner

// 本文件是 Windows ACL 协调器的 Win32/文件系统实现：跨进程命名 mutex、状态文件
// 原子读写、原始 self-relative security descriptor 捕获/恢复、Everyone 删除子项
// 拒绝、Low 强制完整性标签与 capability SID 的加/撤 ACE。
//
// 只用标准库 syscall.NewLazyDLL（与 windows_syscall.go 一致），不引入新依赖。
// 任何 Win32 失败都返回错误；helper 层据此 fail closed（绝不 spawn 未受限进程）。

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"
)

// Win32 常量（数值与 windows.h/winnt.h 一致）。
const (
	winSYNCHRONIZE       = 0x00100000
	winWAITObject0       = 0x00000000
	winWAITAbandoned     = 0x00000080
	winWAITFailed        = 0xFFFFFFFF
	winINFINITE          = 0xFFFFFFFF
	winERRORFileNotFound = 2
)

var (
	// winKernel32 与 windows_folders.go 共用（同一包级 lazy DLL）。
	winProcCreateMutexW      = winKernel32.NewProc("CreateMutexW")
	winProcOpenMutexW        = winKernel32.NewProc("OpenMutexW")
	winProcWaitForSingleObj  = winKernel32.NewProc("WaitForSingleObject")
	winProcReleaseMutex      = winKernel32.NewProc("ReleaseMutex")
	winProcCloseHandle       = winKernel32.NewProc("CloseHandle")
	winProcGetSDLength       = winAdvapi32.NewProc("GetSecurityDescriptorLength")
	winProcSetFileSecurityW  = winAdvapi32.NewProc("SetFileSecurityW")
	winErrNamedMutexNotFound = errors.New("windows-acl: named mutex not found")
)

// winCreateNamedMutex 创建（或打开已存在的）命名 mutex，返回句柄。
func winCreateNamedMutex(name string) (syscall.Handle, error) {
	namePointer, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return 0, fmt.Errorf("CreateMutexW: encode name: %w", err)
	}
	handle, _, callErr := winProcCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(namePointer)))
	runtime.KeepAlive(namePointer)
	if handle == 0 {
		return 0, win32Error("CreateMutexW("+name+")", callErr)
	}
	return syscall.Handle(handle), nil
}

// winOpenNamedMutex 以 SYNCHRONIZE 打开一个已存在的命名 mutex。
func winOpenNamedMutex(name string) (syscall.Handle, error) {
	namePointer, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return 0, fmt.Errorf("OpenMutexW: encode name: %w", err)
	}
	handle, _, callErr := winProcOpenMutexW.Call(uintptr(winSYNCHRONIZE), 0, uintptr(unsafe.Pointer(namePointer)))
	runtime.KeepAlive(namePointer)
	if handle == 0 {
		errno, ok := callErr.(syscall.Errno)
		if ok && errno == syscall.Errno(winERRORFileNotFound) {
			return 0, winErrNamedMutexNotFound
		}
		if callErr == syscall.Errno(0) || callErr == nil {
			callErr = syscall.EINVAL
		}
		return 0, win32Error("OpenMutexW("+name+")", callErr)
	}
	return syscall.Handle(handle), nil
}

// winWaitNamedMutex 等待命名 mutex（接受 WAIT_ABANDONED：前一个持者崩溃时状态
// 文件才是事实来源，调用方在锁内重新 load/purge）。
func winWaitNamedMutex(handle syscall.Handle) error {
	result, _, callErr := winProcWaitForSingleObj.Call(uintptr(handle), uintptr(winINFINITE))
	switch uint32(result) {
	case winWAITObject0, winWAITAbandoned:
		return nil
	case winWAITFailed:
		return win32Error("WaitForSingleObject", callErr)
	default:
		return fmt.Errorf("WaitForSingleObject: unexpected result 0x%x", result)
	}
}

// winReleaseNamedMutex 释放命名 mutex。
func winReleaseNamedMutex(handle syscall.Handle) error {
	result, _, callErr := winProcReleaseMutex.Call(uintptr(handle))
	if result == 0 {
		return win32Error("ReleaseMutex", callErr)
	}
	return nil
}

// winCloseHandle 关闭一个内核句柄。
func winCloseHandle(handle syscall.Handle) error {
	if handle == 0 {
		return nil
	}
	result, _, callErr := winProcCloseHandle.Call(uintptr(handle))
	if result == 0 {
		return win32Error("CloseHandle", callErr)
	}
	return nil
}

// winGetSecurityDescriptorLength 返回 self-relative descriptor 的字节长度。
func winGetSecurityDescriptorLength(descriptor unsafe.Pointer) (uint32, error) {
	if descriptor == nil {
		return 0, errors.New("GetSecurityDescriptorLength: nil descriptor")
	}
	length, _, callErr := winProcGetSDLength.Call(uintptr(descriptor))
	if length == 0 {
		return 0, win32Error("GetSecurityDescriptorLength", callErr)
	}
	return uint32(length), nil
}

// winSetFileSecurity 用完整 self-relative descriptor 恢复安全信息。
func winSetFileSecurity(path string, securityInfo uint32, descriptor unsafe.Pointer) error {
	pathPointer, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return fmt.Errorf("SetFileSecurityW: encode path %q: %w", path, err)
	}
	result, _, callErr := winProcSetFileSecurityW.Call(
		uintptr(unsafe.Pointer(pathPointer)),
		uintptr(securityInfo),
		uintptr(descriptor),
	)
	runtime.KeepAlive(pathPointer)
	if result == 0 {
		return win32Error("SetFileSecurityW", callErr)
	}
	return nil
}

// winWindowsACLBackend 是 winACLBackend 的真实 Win32 实现。
type winWindowsACLBackend struct {
	path      string
	pathHash  string
	statePath string
	// localAppData/tempDir 只保留诊断与测试可观察性。
	localAppData string
	tempDir      string
}

// newWinWindowsACLBackend 为一个已校验的 workspace 路径构造后端。
func newWinWindowsACLBackend(path string) *winWindowsACLBackend {
	local := os.Getenv("LOCALAPPDATA")
	temp := os.Getenv("TEMP")
	pathHash := winACLPathHash(path)
	return &winWindowsACLBackend{
		path:         path,
		pathHash:     pathHash,
		statePath:    winACLStateFilePath(local, temp, pathHash),
		localAppData: local,
		tempDir:      temp,
	}
}

// LoadState 实现 winACLBackend。
func (backend *winWindowsACLBackend) LoadState() (*winACLState, error) {
	if backend == nil || strings.TrimSpace(backend.statePath) == "" {
		return nil, errors.New("windows-acl: no writable state directory (LOCALAPPDATA/TEMP unavailable)")
	}
	data, err := os.ReadFile(backend.statePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("windows-acl: read state %q: %w", backend.statePath, err)
	}
	return parseWinACLState(data)
}

// SaveState 实现 winACLBackend：temp+rename 原子写。
func (backend *winWindowsACLBackend) SaveState(state *winACLState) error {
	if backend == nil || strings.TrimSpace(backend.statePath) == "" {
		return errors.New("windows-acl: no writable state directory (LOCALAPPDATA/TEMP unavailable)")
	}
	encoded, err := marshalWinACLState(state)
	if err != nil {
		return err
	}
	dir := filepath.Dir(backend.statePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("windows-acl: create state directory %q: %w", dir, err)
	}
	temporary, err := os.CreateTemp(dir, filepath.Base(backend.statePath)+".tmp-*")
	if err != nil {
		return fmt.Errorf("windows-acl: create temp state file: %w", err)
	}
	temporaryPath := temporary.Name()
	cleanup := func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}
	if _, err := temporary.Write(encoded); err != nil {
		cleanup()
		return fmt.Errorf("windows-acl: write temp state file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("windows-acl: sync temp state file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("windows-acl: close temp state file: %w", err)
	}
	if err := os.Rename(temporaryPath, backend.statePath); err != nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("windows-acl: replace state file: %w", err)
	}
	return nil
}

// RemoveState 实现 winACLBackend。
func (backend *winWindowsACLBackend) RemoveState() error {
	if backend == nil || strings.TrimSpace(backend.statePath) == "" {
		return nil
	}
	if err := os.Remove(backend.statePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("windows-acl: remove state %q: %w", backend.statePath, err)
	}
	return nil
}

// WithACLMutex 实现 winACLBackend：命名 mutex 内串行化状态变更。
func (backend *winWindowsACLBackend) WithACLMutex(pathHash string, fn func() error) error {
	if fn == nil {
		return nil
	}
	handle, err := winCreateNamedMutex(winACLStateMutexName(pathHash))
	if err != nil {
		return err
	}
	defer func() { _ = winCloseHandle(handle) }()
	if err := winWaitNamedMutex(handle); err != nil {
		return err
	}
	defer func() { _ = winReleaseNamedMutex(handle) }()
	return fn()
}

// CaptureOriginal 实现 winACLBackend：base64(self-relative descriptor bytes)。
func (backend *winWindowsACLBackend) CaptureOriginal() (string, error) {
	security, err := winGetNamedSecurityInfo(backend.path)
	if err != nil {
		return "", err
	}
	defer func() { _ = winLocalFree(security.descriptor) }()
	length, err := winGetSecurityDescriptorLength(security.descriptor)
	if err != nil {
		return "", err
	}
	bytes := unsafe.Slice((*byte)(security.descriptor), int(length))
	return base64.StdEncoding.EncodeToString(bytes), nil
}

// AddWorkspaceDeny 实现 winACLBackend：Everyone FILE_DELETE_CHILD 拒绝 ACE。
func (backend *winWindowsACLBackend) AddWorkspaceDeny() error {
	everyone, err := winParseSID(winEveryoneSIDString)
	if err != nil {
		return err
	}
	security, err := winGetNamedSecurityInfo(backend.path)
	if err != nil {
		return err
	}
	entries := []winExplicitAccess{
		winExplicitAccessFor(everyone, winDenyAccess, winFileDeleteChild, winContainerInherit),
	}
	newACL, err := winSetEntriesInAcl(entries, security.dacl)
	runtime.KeepAlive(entries)
	runtime.KeepAlive(everyone)
	if err != nil {
		_ = winLocalFree(security.descriptor)
		return err
	}
	defer func() { _ = winLocalFree(newACL) }()
	defer func() { _ = winLocalFree(security.descriptor) }()
	return winSetNamedSecurityInfo(backend.path, winDACLSecurityInformation, newACL, nil)
}

// ApplyLowLabel 实现 winACLBackend：尽力应用 Low 强制完整性标签。
func (backend *winWindowsACLBackend) ApplyLowLabel() (bool, error) {
	lowLabel, err := winParseSID(winLowIntegritySIDString)
	if err != nil {
		return false, err
	}
	labelACL, err := winBuildLowLabelACL(lowLabel)
	if err != nil {
		return false, err
	}
	if err := winSetNamedSecurityInfo(backend.path, winLabelSecurityInformation, nil, unsafe.Pointer(&labelACL[0])); err != nil {
		return false, err
	}
	runtime.KeepAlive(labelACL)
	return true, nil
}

// AddCapabilityACE 实现 winACLBackend：capability SID 写+删允许 ACE。
func (backend *winWindowsACLBackend) AddCapabilityACE(capabilitySID string) error {
	capability, err := winParseSID(capabilitySID)
	if err != nil {
		return err
	}
	security, err := winGetNamedSecurityInfo(backend.path)
	if err != nil {
		return err
	}
	entries := []winExplicitAccess{
		winExplicitAccessFor(capability, winGrantAccess, winGrantMask, winGrantInherit),
	}
	newACL, err := winSetEntriesInAcl(entries, security.dacl)
	runtime.KeepAlive(entries)
	runtime.KeepAlive(capability)
	if err != nil {
		_ = winLocalFree(security.descriptor)
		return err
	}
	defer func() { _ = winLocalFree(newACL) }()
	defer func() { _ = winLocalFree(security.descriptor) }()
	return winSetNamedSecurityInfo(backend.path, winDACLSecurityInformation, newACL, nil)
}

// RevokeCapabilityACE 实现 winACLBackend：只撤销该 SID 的 ACE（REVOKE_ACCESS）。
func (backend *winWindowsACLBackend) RevokeCapabilityACE(capabilitySID string) error {
	capability, err := winParseSID(capabilitySID)
	if err != nil {
		return err
	}
	security, err := winGetNamedSecurityInfo(backend.path)
	if err != nil {
		return err
	}
	entries := []winExplicitAccess{
		winExplicitAccessFor(capability, winRevokeAccess, 0, winGrantInherit),
	}
	newACL, err := winSetEntriesInAcl(entries, security.dacl)
	runtime.KeepAlive(entries)
	runtime.KeepAlive(capability)
	if err != nil {
		_ = winLocalFree(security.descriptor)
		return err
	}
	defer func() { _ = winLocalFree(newACL) }()
	defer func() { _ = winLocalFree(security.descriptor) }()
	return winSetNamedSecurityInfo(backend.path, winDACLSecurityInformation, newACL, nil)
}

// RestoreOriginal 实现 winACLBackend：恢复原始 descriptor；标签恢复失败时退化为
// 仅恢复 DACL（DACL 是授权边界，必须恢复），并把标签错误上报给 cleanup。
func (backend *winWindowsACLBackend) RestoreOriginal(state *winACLState, restoreLabel bool) error {
	if state == nil || strings.TrimSpace(state.Descriptor) == "" {
		return errors.New("windows-acl: no original security descriptor to restore")
	}
	decoded, err := base64.StdEncoding.DecodeString(state.Descriptor)
	if err != nil {
		return fmt.Errorf("windows-acl: decode original descriptor: %w", err)
	}
	// A Go byte buffer is enough: SetFileSecurityW copies/reads the self-relative
	// descriptor during the call, so KeepAlive covers the pointer lifetime.
	buffer := make([]byte, len(decoded))
	copy(buffer, decoded)
	descriptor := unsafe.Pointer(&buffer[0])
	defer runtime.KeepAlive(buffer)

	securityInfo := uint32(winDACLSecurityInformation)
	if restoreLabel {
		securityInfo |= winLabelSecurityInformation
	}
	if err := winSetFileSecurity(backend.path, securityInfo, descriptor); err != nil {
		if !restoreLabel {
			return err
		}
		daclErr := winSetFileSecurity(backend.path, winDACLSecurityInformation, descriptor)
		if daclErr != nil {
			return fmt.Errorf("%w (DACL restore also failed: %v)", err, daclErr)
		}
		return fmt.Errorf("restore DACL succeeded but integrity label restore failed: %w", err)
	}
	return nil
}

// MarkerAlive 实现 winACLBackend：OpenMutex 不存在 = 已死；其它错误按“存活”处理
// （宁可多保留一个 grant 的状态，也不在无法判断时错误恢复/删除状态）。
func (backend *winWindowsACLBackend) MarkerAlive(marker string) bool {
	handle, err := winOpenNamedMutex(marker)
	if err != nil {
		return !errors.Is(err, winErrNamedMutexNotFound)
	}
	_ = winCloseHandle(handle)
	return true
}

// CreateMarker 实现 winACLBackend：创建并持有 grant marker mutex。
func (backend *winWindowsACLBackend) CreateMarker(name string) (any, error) {
	handle, err := winCreateNamedMutex(name)
	if err != nil {
		return nil, err
	}
	return handle, nil
}

// CloseMarker 实现 winACLBackend。
func (backend *winWindowsACLBackend) CloseMarker(handle any) {
	if named, ok := handle.(syscall.Handle); ok {
		_ = winCloseHandle(named)
	}
}
