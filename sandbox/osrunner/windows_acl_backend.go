//go:build windows

package osrunner

// 本文件是 Windows ACL 协调器的 Win32/文件系统实现：跨进程命名 mutex、状态文件
// 原子读写、原始 self-relative security descriptor 捕获/恢复、Everyone 删除子项
// 拒绝 + capability 允许 ACE + Low 强制完整性标签的**一次**合并设置、保护路径
// 子树里逐对象的 deny ACE。
//
// 只用标准库 syscall.NewLazyDLL（与 windows_syscall.go 一致），不引入新依赖。
// 任何 Win32 失败都返回错误；helper 层据此 fail closed（绝不 spawn 未受限进程）。
//
// # B1：DACL 与 SACL 合并成一次调用
//
// SetNamedSecurityInfoW 同时接受 pDacl 与 pSacl，只要有对应的 SECURITY_INFORMATION
// 位。因此 workspace 根的形状（Everyone 拒绝 + capability 允许 + Low 标签）用**一次**
// 调用建立，不存在"deny 已加、标签没加"或"DACL 改了、状态没落盘"的中间态。标签
// 需要 SeSecurityPrivilege/WRITE_OWNER：整次调用失败时退化为只设 DACL（与旧语义
// 一致：标签是纵深防御，缺它绝不放宽写授权），并按实际结果返回形状。

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

// winACLBytes 把 ACL 指针变成字节视图（长度取 AclSize）。
func winACLBytes(acl unsafe.Pointer) []byte {
	if acl == nil {
		return nil
	}
	size := int(*(*uint16)(unsafe.Add(acl, 2)))
	if size < winACLHeaderSize {
		return nil
	}
	return unsafe.Slice((*byte)(acl), size)
}

// winSIDBytes 返回 SID 字符串对应的 SID 结构字节（用于精确 ACE 比对）。
//
// 返回的切片指向 syscall.StringToSid 分配的 Go 内存，调用方必须保证在比较期间
// 它不被回收（比较是纯内存操作，调用点用 runtime.KeepAlive 兜底）。
func winSIDBytes(text string) ([]byte, error) {
	sid, err := winParseSID(text)
	if err != nil {
		return nil, err
	}
	length := int(syscall.GetLengthSid(sid))
	if length <= 0 || length > winSIDMaxSize {
		return nil, fmt.Errorf("windows-acl: SID %q has invalid length %d", text, length)
	}
	bytes := unsafe.Slice((*byte)(unsafe.Pointer(sid)), length)
	runtime.KeepAlive(sid)
	return bytes, nil
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

// ACLShape 实现 winACLBackend：读 DACL/标签，报告三部分形状是否存在。
func (backend *winWindowsACLBackend) ACLShape(capabilitySID string) (winACLShape, error) {
	shape := winACLShape{}
	security, err := winGetNamedSecurityInfo(backend.path)
	if err != nil {
		return shape, err
	}
	defer func() { _ = winLocalFree(security.descriptor) }()
	capability, err := winSIDBytes(capabilitySID)
	if err != nil {
		return shape, err
	}
	defer runtime.KeepAlive(capability)
	if dacl := winACLBytes(security.dacl); dacl != nil {
		if shape.CapabilityAllow, err = winACLHasExactACE(dacl, winACETypeAccessAllowed, winGrantInherit, winGrantMask, capability); err != nil {
			return shape, err
		}
		everyone, sidErr := winSIDBytes(winEveryoneSIDString)
		if sidErr != nil {
			return shape, sidErr
		}
		if shape.WorkspaceDeny, err = winACLHasExactACE(dacl, winACETypeAccessDenied, winContainerInherit, winFileDeleteChild, everyone); err != nil {
			return shape, err
		}
		runtime.KeepAlive(everyone)
	}
	if sacl := winACLBytes(security.sacl); sacl != nil {
		lowLabel, sidErr := winSIDBytes(winLowIntegritySIDString)
		if sidErr != nil {
			return shape, sidErr
		}
		if shape.LowLabel, err = winACLHasExactACE(sacl, winACETypeMandatoryLabel, winGrantInherit, winMandatoryLabelNoWriteUp, lowLabel); err != nil {
			return shape, err
		}
		runtime.KeepAlive(lowLabel)
	}
	return shape, nil
}

// ApplyShape 实现 winACLBackend：**一次** SetNamedSecurityInfoW 设置 DACL 与标签。
func (backend *winWindowsACLBackend) ApplyShape(request winShapeRequest) (winACLShape, error) {
	shape := winACLShape{}
	capability, err := winParseSID(request.CapabilitySID)
	if err != nil {
		return shape, err
	}
	security, err := winGetNamedSecurityInfo(backend.path)
	if err != nil {
		return shape, err
	}
	defer func() { _ = winLocalFree(security.descriptor) }()
	entries := make([]winExplicitAccess, 0, 2)
	if request.WorkspaceDeny {
		everyone, sidErr := winParseSID(winEveryoneSIDString)
		if sidErr != nil {
			return shape, sidErr
		}
		entries = append(entries, winExplicitAccessFor(everyone, winDenyAccess, winFileDeleteChild, winContainerInherit))
		defer runtime.KeepAlive(everyone)
	}
	entries = append(entries, winExplicitAccessFor(capability, winGrantAccess, winGrantMask, winGrantInherit))
	newACL, err := winSetEntriesInAcl(entries, security.dacl)
	runtime.KeepAlive(entries)
	runtime.KeepAlive(capability)
	if err != nil {
		return shape, err
	}
	defer func() { _ = winLocalFree(newACL) }()
	if !request.LowLabel {
		if err := winSetNamedSecurityInfo(backend.path, winDACLSecurityInformation, newACL, nil); err != nil {
			return shape, err
		}
		return winACLShape{CapabilityAllow: true, WorkspaceDeny: request.WorkspaceDeny}, nil
	}
	lowLabel, err := winParseSID(winLowIntegritySIDString)
	if err != nil {
		return shape, err
	}
	labelACL, err := winBuildLowLabelACL(lowLabel)
	if err != nil {
		return shape, err
	}
	// 一次调用同时写 DACL 与标签：没有 DACL/SACL 之间的中间态。
	combinedErr := winSetNamedSecurityInfo(backend.path, winDACLSecurityInformation|winLabelSecurityInformation, newACL, unsafe.Pointer(&labelACL[0]))
	runtime.KeepAlive(labelACL)
	runtime.KeepAlive(lowLabel)
	if combinedErr == nil {
		return winACLShape{CapabilityAllow: true, WorkspaceDeny: request.WorkspaceDeny, LowLabel: true}, nil
	}
	// 标签写不进去（缺少 SeSecurityPrivilege/WRITE_OWNER）：退化为只设 DACL。
	// 受限令牌与 capability ACE 仍是有效边界，绝不因为标签失败放宽写授权。
	if daclErr := winSetNamedSecurityInfo(backend.path, winDACLSecurityInformation, newACL, nil); daclErr != nil {
		return shape, fmt.Errorf("%w (DACL-only fallback also failed: %v)", combinedErr, daclErr)
	}
	return winACLShape{CapabilityAllow: true, WorkspaceDeny: request.WorkspaceDeny}, nil
}

// RestoreShape 实现 winACLBackend：恢复 per-run 部分，保留缓存的 capability ACE。
//
// 目标 DACL = 快照 DACL ⊕ 本会话 capability 允许 ACE（缓存资产不撤）；目标
// SACL = 快照 SACL（原始标签；原始没有 SACL 时传 NULL，即移除标签）。
func (backend *winWindowsACLBackend) RestoreShape(state *winACLState, capabilitySID string, restoreLabel bool) error {
	if state == nil || strings.TrimSpace(state.Descriptor) == "" {
		return errors.New("windows-acl: no original security descriptor to restore")
	}
	decoded, err := base64.StdEncoding.DecodeString(state.Descriptor)
	if err != nil {
		return fmt.Errorf("windows-acl: decode original descriptor: %w", err)
	}
	buffer := make([]byte, len(decoded))
	copy(buffer, decoded)
	// saclPointer/DACL 指针都指向 buffer：显式 KeepAlive 到函数返回，避免 GC 在
	// Win32 调用期间回收它。
	defer runtime.KeepAlive(buffer)
	sections, err := winParseDescriptorSections(buffer)
	if err != nil {
		return err
	}
	base := unsafe.Pointer(&buffer[0])
	securityInfo := uint32(winDACLSecurityInformation)
	var saclPointer unsafe.Pointer
	if restoreLabel {
		securityInfo |= winLabelSecurityInformation
		if sections.SACLOffset > 0 {
			saclPointer = unsafe.Add(base, sections.SACLOffset)
		}
	}
	if sections.DACLOffset == 0 {
		// 原始 DACL 为 NULL（不受限目录）：恢复 NULL，缓存 ACE 只能放弃——下一次
		// 运行会按 ACLShape 重新加回来。
		return winSetNamedSecurityInfo(backend.path, securityInfo, nil, saclPointer)
	}
	capability, err := winParseSID(capabilitySID)
	if err != nil {
		return err
	}
	entries := []winExplicitAccess{winExplicitAccessFor(capability, winGrantAccess, winGrantMask, winGrantInherit)}
	newACL, err := winSetEntriesInAcl(entries, unsafe.Add(base, sections.DACLOffset))
	runtime.KeepAlive(entries)
	if err != nil {
		return err
	}
	defer func() { _ = winLocalFree(newACL) }()
	if err := winSetNamedSecurityInfo(backend.path, securityInfo, newACL, saclPointer); err != nil {
		if !restoreLabel {
			return err
		}
		daclErr := winSetNamedSecurityInfo(backend.path, winDACLSecurityInformation, newACL, nil)
		if daclErr != nil {
			return fmt.Errorf("%w (DACL restore also failed: %v)", err, daclErr)
		}
		return fmt.Errorf("restore DACL succeeded but integrity label restore failed: %w", err)
	}
	return nil
}

// RestoreOriginal 实现 winACLBackend：用快照完整恢复 DACL（+标签）。
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

// Object 实现 winACLBackend：返回保护子树里某个对象的 ACE 接缝。
func (backend *winWindowsACLBackend) Object(path string) winProtectObject {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	return &winWindowsProtectObject{path: path}
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

// winWindowsProtectObject 是保护子树里单个对象（文件或目录）的 ACE 实现。
//
// 每条 ACE 的形状都是确定的（capability SID + winProtectDenyMask + OI|CI +
// ACCESS_DENIED），因此幂等判定可以只比四项；撤销用 REVOKE_ACCESS 精确命中这个
// SID（capability SID 由路径派生、只有本协调器使用，不会误删用户/系统 ACE）。
type winWindowsProtectObject struct {
	path string
}

var _ winProtectObject = (*winWindowsProtectObject)(nil)

// DenyPresent 实现 winProtectObject。
func (object *winWindowsProtectObject) DenyPresent(capabilitySID string) (bool, error) {
	sidBytes, err := winSIDBytes(capabilitySID)
	if err != nil {
		return false, err
	}
	defer runtime.KeepAlive(sidBytes)
	security, err := winGetNamedSecurityInfo(object.path)
	if err != nil {
		return false, err
	}
	defer func() { _ = winLocalFree(security.descriptor) }()
	return winACLHasExactACE(winACLBytes(security.dacl), winACETypeAccessDenied, winProtectDenyFlags, winProtectDenyMask, sidBytes)
}

// ApplyDeny 实现 winProtectObject。
func (object *winWindowsProtectObject) ApplyDeny(capabilitySID string) error {
	capability, err := winParseSID(capabilitySID)
	if err != nil {
		return err
	}
	security, err := winGetNamedSecurityInfo(object.path)
	if err != nil {
		return err
	}
	entries := []winExplicitAccess{
		winExplicitAccessFor(capability, winDenyAccess, winProtectDenyMask, winProtectDenyFlags),
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
	return winSetNamedSecurityInfo(object.path, winDACLSecurityInformation, newACL, nil)
}

// RevokeDeny 实现 winProtectObject。
func (object *winWindowsProtectObject) RevokeDeny(capabilitySID string) error {
	capability, err := winParseSID(capabilitySID)
	if err != nil {
		return err
	}
	security, err := winGetNamedSecurityInfo(object.path)
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
	return winSetNamedSecurityInfo(object.path, winDACLSecurityInformation, newACL, nil)
}
