//go:build windows

// probe 是 Windows 安全 API 的"Wine 兼容性探针"：逐一调用我们 ACL 后端依赖的
// Win32 API，打印成功/失败与错误码，用来判断 Wine 能否作为该后端的验证目标。
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

var (
	advapi32 = syscall.NewLazyDLL("advapi32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procGetNamedSecurityInfoW = advapi32.NewProc("GetNamedSecurityInfoW")
	procSetNamedSecurityInfoW = advapi32.NewProc("SetNamedSecurityInfoW")
	procCreateRestrictedToken = advapi32.NewProc("CreateRestrictedToken")
	procCreateProcessAsUserW  = advapi32.NewProc("CreateProcessAsUserW")
	procAdjustTokenPrivileges = advapi32.NewProc("AdjustTokenPrivileges")
	procSetEntriesInAclW      = advapi32.NewProc("SetEntriesInAclW")
	procGetTokenInformation   = advapi32.NewProc("GetTokenInformation")
	procCreateWellKnownSid    = advapi32.NewProc("CreateWellKnownSid")
	procGetCurrentProcess     = kernel32.NewProc("GetCurrentProcess")
	procOpenProcessToken      = advapi32.NewProc("OpenProcessToken")
	procCreateJobObjectW      = kernel32.NewProc("CreateJobObjectW")
	procAssignProcessToJob    = kernel32.NewProc("AssignProcessToJobObject")
	procLocalFree             = kernel32.NewProc("LocalFree")
)

const (
	daclSecurityInformation  = 0x00000004
	saclSecurityInformation  = 0x00000010
	labelSecurityInformation = 0x00000010
	ownerSecurityInformation = 0x00000001
	groupSecurityInformation = 0x00000002

	writeRestrictedTokenFlag = 0x00000008
	disableMaxPrivilegeFlag  = 0x00000001

	tokenIntegrityLevel = 25
	tokenAllAccess      = 0x000F01FF
)

func check(name string, ok bool, err error) {
	status := "OK"
	if !ok {
		status = "FAIL"
	}
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	fmt.Printf("%-44s %-4s %s\n", name, status, detail)
}

func errnoText(errno syscall.Errno) string {
	if errno == 0 {
		return ""
	}
	return errno.Error()
}

func main() {
	fmt.Println("==== wine security API probe ====")
	base := os.Getenv("PROBE_DIR")
	if base == "" {
		base = "."
	}
	dir := filepath.Join(base, "acl-target")
	_ = os.MkdirAll(dir, 0o755)
	target, _ := syscall.UTF16PtrFromString(dir)

	var sd *byte
	var dacl *byte

	// 1) GetNamedSecurityInfoW: DACL only (我们每目录读取基线所需)
	rc, _, errno := procGetNamedSecurityInfoW.Call(
		uintptr(unsafe.Pointer(target)), 1 /*SE_FILE_OBJECT*/, daclSecurityInformation,
		0, 0, uintptr(unsafe.Pointer(&dacl)), 0, uintptr(unsafe.Pointer(&sd)))
	check("GetNamedSecurityInfoW(DACL)", rc == 0, errno)
	if sd != nil {
		procLocalFree.Call(uintptr(unsafe.Pointer(sd)))
		sd, dacl = nil, nil
	}

	// 2) GetNamedSecurityInfoW: DACL + LABEL (我们读取"是否已有 Low 标签")
	rc, _, errno = procGetNamedSecurityInfoW.Call(
		uintptr(unsafe.Pointer(target)), 1, labelSecurityInformation,
		0, 0, 0, 0, uintptr(unsafe.Pointer(&sd)))
	check("GetNamedSecurityInfoW(LABEL)", rc == 0, errno)
	if sd != nil {
		procLocalFree.Call(uintptr(unsafe.Pointer(sd)))
		sd = nil
	}

	// 3) SetNamedSecurityInfoW: 只写 LABEL（Low 完整性标签；我们的 workspace-write 依赖）
	rc, _, errno = procSetNamedSecurityInfoW.Call(
		uintptr(unsafe.Pointer(target)), 1, labelSecurityInformation, 0, 0, 0, 0)
	check("SetNamedSecurityInfoW(LABEL)", rc == 0, errno)

	// 4) CreateRestrictedToken(WRITE_RESTRICTED | DISABLE_MAX_PRIVILEGE, restricting=Everyone)
	//    S-1-1-0 的二进制形式：revision=1, count=1, authority=5, subauthority=0
	sid := []byte{1, 1, 0, 0, 0, 0, 0, 5, 0, 0, 0, 0, 0, 0, 0, 0}
	var procToken syscall.Token
	procHandle, _, _ := procGetCurrentProcess.Call()
	ok, _, openErr := procOpenProcessToken.Call(procHandle, tokenAllAccess, uintptr(unsafe.Pointer(&procToken)))
	check("OpenProcessToken(TOKEN_ALL_ACCESS)", ok != 0, openErr)
	var token syscall.Token
	rc, _, errno = procCreateRestrictedToken.Call(
		uintptr(procToken),
		writeRestrictedTokenFlag|disableMaxPrivilegeFlag,
		0, 0, 1, uintptr(unsafe.Pointer(&sid[0])), 0, 0,
		uintptr(unsafe.Pointer(&token)))
	check("CreateRestrictedToken(WRITE_RESTRICTED)", rc != 0, errno)

	// 5) Job object（kill-on-close 后代进程模型）
	job, _, errno := procCreateJobObjectW.Call(0, 0)
	check("CreateJobObjectW", job != 0, errno)

	// 6) AdjustTokenPrivileges（我们目前未启用 SeChangeNotifyPrivilege）
	var prev [16]byte
	rc, _, errno = procAdjustTokenPrivileges.Call(
		uintptr(procToken), 0, 0, 0, uintptr(unsafe.Pointer(&prev[0])))
	check("AdjustTokenPrivileges", rc != 0, errno)

	// 7) SetEntriesInAclW（我们合并 DACL 时用）
	var newACL *byte
	var ea [32]byte // EXPLICIT_ACCESS_W 的最小尺寸足够触发参数校验
	rc, _, errno = procSetEntriesInAclW.Call(0, uintptr(unsafe.Pointer(&ea[0])), 0, uintptr(unsafe.Pointer(&newACL)))
	check("SetEntriesInAclW", rc == 0, errno)

	fmt.Println("==== probe done ====")
}
