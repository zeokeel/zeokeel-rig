//go:build windows

package osrunner

import (
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

// 本文件是 Windows ACL runner 的 Win32 绑定层：只用标准库 syscall +
// syscall.NewLazyDLL，不引入 golang.org/x/sys、不依赖 cgo、不加模块依赖。
//
// 每个 Win32 调用都必须检查返回值；失败时返回带 API 名与精确错误码的错误，
// 由 helper 层转换成 "windows-acl-run: <detail>" + 退出码 127（fail closed：
// 任何一步失败都绝不 spawn 未受限的子进程）。

// Win32 常量。数值与 windows.h/winnt.h 一致。
const (
	// OpenProcessToken 需要的访问位。
	winTokenAssignPrimary   = 0x0001
	winTokenDuplicate       = 0x0002
	winTokenQuery           = 0x0008
	winTokenAdjustDefault   = 0x0080
	winTokenAdjustSessionID = 0x0100

	// TOKEN_INFORMATION_CLASS。
	winTokenGroups         = 2
	winTokenDefaultDACL    = 6
	winTokenIntegrityLevel = 25

	// 令牌组属性。
	winSEGroupLogonID   = 0xC0000000
	winSEGroupIntegrity = 0x00000020

	// CreateRestrictedToken 标志。
	winDisableMaxPrivilege = 0x00000001
	winLUAToken            = 0x00000004
	winWriteRestricted     = 0x00000008

	// SECURITY_INFORMATION / SE_OBJECT_TYPE。
	winDACLSecurityInformation  = 0x00000004
	winLabelSecurityInformation = 0x00000010
	winSEFileObject             = 1

	// TOKEN_GROUPS 头部大小（DWORD GroupCount）与 SID_AND_ATTRIBUTES 跨度。
	winTokenGroupsHeaderSize = 8
	winSIDAndAttributesSize  = 16
	// winACLHeaderSize 是 ACL 头部字节数（AclRevision/Sbz1/AclSize/AceCount/Sbz2）。
	winACLHeaderSize = 8
	// winSIDMaxSize 是 SECURITY_MAX_SID_SIZE。
	winSIDMaxSize = 68

	// 访问掩码与继承标志。
	winFileAllAccess   = 0x001F01FF // FILE_ALL_ACCESS
	winFileDeleteChild = 0x00000040 // FILE_DELETE_CHILD
	// winGrantMask 是能力 SID 获得的“写+删+删子项”掩码：
	// FILE_GENERIC_WRITE - STANDARD_RIGHTS_WRITE + DELETE + FILE_DELETE_CHILD。
	// 刻意不含 WRITE_DAC/WRITE_OWNER：受限子进程不能改写 DACL 或取得所有权。
	winGrantMask        = 0x00110156
	winGrantInherit     = 3 // OBJECT_INHERIT_ACE | CONTAINER_INHERIT_ACE
	winContainerInherit = 2 // CONTAINER_INHERIT_ACE

	// ACL / 强制标签。
	winACLRevision                 = 2
	winSystemMandatoryLabelACEType = 17
	winMandatoryLabelNoWriteUp     = 1 // SYSTEM_MANDATORY_LABEL_NO_WRITE_UP

	// EXPLICIT_ACCESS 模式。
	winGrantAccess  = 1
	winDenyAccess   = 3
	winRevokeAccess = 4

	// TRUSTEE 形式与类型。
	winTrusteeIsSID     = 0
	winTrusteeIsUnknown = 0
)

var (
	winAdvapi32 = syscall.NewLazyDLL("advapi32.dll")

	winProcCreateRestrictedToken = winAdvapi32.NewProc("CreateRestrictedToken")
	winProcSetTokenInformation   = winAdvapi32.NewProc("SetTokenInformation")
	winProcGetNamedSecurityInfoW = winAdvapi32.NewProc("GetNamedSecurityInfoW")
	winProcSetNamedSecurityInfoW = winAdvapi32.NewProc("SetNamedSecurityInfoW")
	winProcSetEntriesInAclW      = winAdvapi32.NewProc("SetEntriesInAclW")
	winProcInitializeAcl         = winAdvapi32.NewProc("InitializeAcl")
	winProcAddMandatoryAce       = winAdvapi32.NewProc("AddMandatoryAce")
)

// win32Error 把一次失败的 Win32 调用包装成带 API 名的错误。
func win32Error(api string, err error) error {
	if err == nil {
		err = syscall.EINVAL
	}
	return fmt.Errorf("%s: %w", api, err)
}

// win32CodeError 把 Win32 返回值（DWORD 错误码）包装成错误。
func win32CodeError(api string, code uintptr) error {
	return win32Error(api, syscall.Errno(code))
}

// winLayoutSupported 是编译期常量：本文件的 Win32 结构体布局只对 64 位 Windows
// （amd64/arm64，LLP64：指针 8 字节）成立。其它 Windows 架构上 runner 必须
// fail closed，绝不能拿错误布局去改真实目录的 ACL。
//
// TRUSTEE_W 在 64 位上是 32 字节（ptstrName 在偏移 24），EXPLICIT_ACCESS_W 是
// 48 字节（TRUSTEE_W 在偏移 16）；winExplicitAccessFor 生成的正是这一布局。
const winLayoutSupported = unsafe.Sizeof(winTrustee{}) == 32 &&
	unsafe.Sizeof(winExplicitAccess{}) == 48 &&
	unsafe.Offsetof(winExplicitAccess{}.trustee) == 16 &&
	unsafe.Offsetof(winExplicitAccess{}.trustee.name) == 24

// winTrustee 是 TRUSTEE_W（amd64/arm64 上 32 字节）。
type winTrustee struct {
	multipleTrustee          uintptr
	multipleTrusteeOperation uint32
	trusteeForm              uint32
	trusteeType              uint32
	_                        uint32 // 对齐 ptstrName 到 8 字节
	name                     uintptr
}

// winExplicitAccess 是 EXPLICIT_ACCESS_W（amd64 上 48 字节）：
// 3 个 DWORD + 4 字节对齐 + TRUSTEE_W。
type winExplicitAccess struct {
	accessPermissions uint32
	accessMode        uint32
	inheritance       uint32
	_                 uint32
	trustee           winTrustee
}

// winExplicitAccessFor 构造一条针对 SID 的显式访问记录。
func winExplicitAccessFor(sid *syscall.SID, mode, permissions, inheritance uint32) winExplicitAccess {
	return winExplicitAccess{
		accessPermissions: permissions,
		accessMode:        mode,
		inheritance:       inheritance,
		trustee: winTrustee{
			trusteeForm: winTrusteeIsSID,
			trusteeType: winTrusteeIsUnknown,
			name:        uintptr(unsafe.Pointer(sid)),
		},
	}
}

// winCreateRestrictedToken 调用 CreateRestrictedToken。
func winCreateRestrictedToken(existing syscall.Token, flags uint32, restricting []*syscall.SID) (syscall.Token, error) {
	attributes := make([]syscall.SIDAndAttributes, len(restricting))
	for index, sid := range restricting {
		attributes[index] = syscall.SIDAndAttributes{Sid: sid}
	}
	var attributesPointer uintptr
	if len(attributes) > 0 {
		attributesPointer = uintptr(unsafe.Pointer(&attributes[0]))
	}
	var restricted syscall.Token
	result, _, callErr := winProcCreateRestrictedToken.Call(
		uintptr(existing),
		uintptr(flags),
		0, 0, // DisableSidCount, SidsToDisable
		0, 0, // DeletePrivilegeCount, PrivilegesToDelete
		uintptr(len(attributes)), attributesPointer,
		uintptr(unsafe.Pointer(&restricted)),
	)
	runtime.KeepAlive(attributes)
	runtime.KeepAlive(restricting)
	if result == 0 {
		return 0, win32Error("CreateRestrictedToken", callErr)
	}
	if restricted == 0 {
		return 0, errors.New("CreateRestrictedToken: nil token handle")
	}
	return restricted, nil
}

// winSetTokenInformation 调用 SetTokenInformation。
func winSetTokenInformation(token syscall.Token, class uint32, info unsafe.Pointer, size uint32) error {
	result, _, callErr := winProcSetTokenInformation.Call(
		uintptr(token),
		uintptr(class),
		uintptr(info),
		uintptr(size),
	)
	if result == 0 {
		return win32Error("SetTokenInformation", callErr)
	}
	return nil
}

// winSecurityDescriptor 持有 GetNamedSecurityInfoW 返回的安全描述符及其内部指针。
//
// 分配契约：只有 descriptor 可以用 LocalFree 释放；dacl/sacl 指向该分配内部，
// 必须等 SetEntriesInAclW 消费完、并且恢复安全描述符之后再释放 descriptor。
type winSecurityDescriptor struct {
	descriptor unsafe.Pointer
	owner      unsafe.Pointer
	group      unsafe.Pointer
	dacl       unsafe.Pointer
	sacl       unsafe.Pointer
}

// winGetNamedSecurityInfo 读取 path 的 DACL 与强制完整性标签。
func winGetNamedSecurityInfo(path string) (*winSecurityDescriptor, error) {
	pathPointer, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, fmt.Errorf("GetNamedSecurityInfoW: encode path %q: %w", path, err)
	}
	security := &winSecurityDescriptor{}
	result, _, _ := winProcGetNamedSecurityInfoW.Call(
		uintptr(unsafe.Pointer(pathPointer)),
		uintptr(winSEFileObject),
		uintptr(winDACLSecurityInformation|winLabelSecurityInformation),
		uintptr(unsafe.Pointer(&security.owner)),
		uintptr(unsafe.Pointer(&security.group)),
		uintptr(unsafe.Pointer(&security.dacl)),
		uintptr(unsafe.Pointer(&security.sacl)),
		uintptr(unsafe.Pointer(&security.descriptor)),
	)
	runtime.KeepAlive(pathPointer)
	if result != 0 {
		return nil, win32CodeError("GetNamedSecurityInfoW", result)
	}
	if security.descriptor == nil {
		return nil, errors.New("GetNamedSecurityInfoW: nil security descriptor")
	}
	return security, nil
}

// winSetNamedSecurityInfo 应用 DACL/SACL 修改。
//
// dacl 与 sacl 为 nil 时对应部分不变（由 securityInfo 位决定哪些部分被写入）。
func winSetNamedSecurityInfo(path string, securityInfo uint32, dacl, sacl unsafe.Pointer) error {
	pathPointer, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return fmt.Errorf("SetNamedSecurityInfoW: encode path %q: %w", path, err)
	}
	result, _, _ := winProcSetNamedSecurityInfoW.Call(
		uintptr(unsafe.Pointer(pathPointer)),
		uintptr(winSEFileObject),
		uintptr(securityInfo),
		0, // psidOwner
		0, // psidGroup
		uintptr(dacl),
		uintptr(sacl),
	)
	runtime.KeepAlive(pathPointer)
	if result != 0 {
		return win32CodeError("SetNamedSecurityInfoW", result)
	}
	return nil
}

// winSetEntriesInAcl 把 entries 合并进 oldACL，返回新 ACL（调用方负责 LocalFree）。
func winSetEntriesInAcl(entries []winExplicitAccess, oldACL unsafe.Pointer) (unsafe.Pointer, error) {
	if len(entries) == 0 {
		return nil, errors.New("SetEntriesInAclW: no entries")
	}
	var newACL unsafe.Pointer
	result, _, _ := winProcSetEntriesInAclW.Call(
		uintptr(len(entries)),
		uintptr(unsafe.Pointer(&entries[0])),
		uintptr(oldACL),
		uintptr(unsafe.Pointer(&newACL)),
	)
	runtime.KeepAlive(entries)
	if result != 0 {
		return nil, win32CodeError("SetEntriesInAclW", result)
	}
	if newACL == nil {
		return nil, errors.New("SetEntriesInAclW: nil result ACL")
	}
	return newACL, nil
}

// winLocalFree 释放 Win32 分配的内存。
func winLocalFree(pointer unsafe.Pointer) error {
	if pointer == nil {
		return nil
	}
	remaining, err := syscall.LocalFree(syscall.Handle(pointer))
	if remaining != 0 {
		return win32Error("LocalFree", err)
	}
	return nil
}

// winInitializeACL 在调用方提供的缓冲区上初始化一个 ACL。
func winInitializeACL(acl unsafe.Pointer, size uint32) error {
	result, _, callErr := winProcInitializeAcl.Call(
		uintptr(acl),
		uintptr(size),
		uintptr(winACLRevision),
	)
	if result == 0 {
		return win32Error("InitializeAcl", callErr)
	}
	return nil
}

// winAddMandatoryAce 往 ACL 追加一条强制完整性标签 ACE。
func winAddMandatoryAce(acl unsafe.Pointer, flags, policy uint32, label *syscall.SID) error {
	result, _, callErr := winProcAddMandatoryAce.Call(
		uintptr(acl),
		uintptr(winACLRevision),
		uintptr(flags),
		uintptr(policy),
		uintptr(unsafe.Pointer(label)),
	)
	runtime.KeepAlive(label)
	if result == 0 {
		return win32Error("AddMandatoryAce", callErr)
	}
	return nil
}

// winBuildLowLabelACL 构造只含一条继承性 Low 标签 ACE 的 ACL（调用方持有缓冲区，
// SetNamedSecurityInfoW 会复制它，因此不需要 LocalAlloc）。
func winBuildLowLabelACL(lowLabel *syscall.SID) ([]byte, error) {
	sidLength := int(syscall.GetLengthSid(lowLabel))
	if sidLength <= 0 || sidLength > winSIDMaxSize {
		return nil, errors.New("AddMandatoryAce: invalid Low integrity label SID")
	}
	// ACL 头 8 字节 + ACE 头 4 字节 + 掩码 4 字节 + SID。
	size := winACLHeaderSize + 8 + sidLength
	buffer := make([]byte, size)
	acl := unsafe.Pointer(&buffer[0])
	if err := winInitializeACL(acl, uint32(size)); err != nil {
		return nil, err
	}
	if err := winAddMandatoryAce(acl, winGrantInherit, winMandatoryLabelNoWriteUp, lowLabel); err != nil {
		return nil, err
	}
	runtime.KeepAlive(buffer)
	return buffer, nil
}

// winReadPointer 从缓冲区 offset 处读取一个指针值。
func winReadPointer(buffer []byte, offset int) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&buffer[offset]))
}

// winReadUint32 从缓冲区 offset 处读取一个小端 uint32。
func winReadUint32(buffer []byte, offset int) uint32 {
	return binary.LittleEndian.Uint32(buffer[offset:])
}
