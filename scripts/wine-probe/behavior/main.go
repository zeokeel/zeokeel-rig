//go:build windows

// behavior 测试 Wine 是否**真的强制**受限令牌语义：用 restricting SID 只留
// Everyone 的令牌去写一个当前用户可写的目录。Windows 上必须失败；Wine 若不
// 强制，就会成功——这决定 Wine 能否作为该后端的验证目标。
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

var (
	advapi32b = syscall.NewLazyDLL("advapi32.dll")
	kernel32b = syscall.NewLazyDLL("kernel32.dll")

	pOpenProcessToken      = advapi32b.NewProc("OpenProcessToken")
	pCreateRestrictedToken = advapi32b.NewProc("CreateRestrictedToken")
	pSetTokenInformation   = advapi32b.NewProc("SetTokenInformation")
	pCreateProcessAsUserW  = advapi32b.NewProc("CreateProcessAsUserW")
	pGetCurrentProcess2    = kernel32b.NewProc("GetCurrentProcess")
	pWaitForSingleObject   = kernel32b.NewProc("WaitForSingleObject")
	pGetExitCodeProcess    = kernel32b.NewProc("GetExitCodeProcess")

	procTokenAllAccess = uintptr(0x000F01FF)
)

func main() {
	base := os.Getenv("PROBE_DIR")
	if base == "" {
		base = "."
	}
	target := filepath.Join(base, "restricted-out.txt")
	_ = os.Remove(target)

	var cur syscall.Token
	ph, _, _ := pGetCurrentProcess2.Call()
	if rc, _, err := pOpenProcessToken.Call(ph, procTokenAllAccess, uintptr(unsafe.Pointer(&cur))); rc == 0 {
		fmt.Println("OpenProcessToken failed:", err)
		return
	}

	// restricting SID = Everyone (S-1-1-0)；正常令牌的 user SID 因此不参与最终判定。
	everyone := []byte{1, 1, 0, 0, 0, 0, 0, 5, 0, 0, 0, 0, 0, 0, 0, 0}
	var restricted syscall.Token
	rc, _, err := pCreateRestrictedToken.Call(uintptr(cur), 0x9, /*WRITE_RESTRICTED|DISABLE_MAX_PRIVILEGE*/
		0, 0, 1, uintptr(unsafe.Pointer(&everyone[0])), 0, 0, uintptr(unsafe.Pointer(&restricted)))
	if rc == 0 {
		fmt.Println("CreateRestrictedToken failed:", err)
		return
	}
	fmt.Println("CreateRestrictedToken: OK")

	// 尝试把令牌降到 Low 完整性级别（S-1-16-4096）。
	// TOKEN_MANDATORY_LABEL{ Label SID_AND_ATTRIBUTES{ Sid *SID; Attributes DWORD } }
	type sidAndAttributes struct {
		Sid        *byte
		Attributes uint32
		_          uint32
	}
	type mandatoryLabel struct {
		Label sidAndAttributes
		Sid   [16]byte
	}
	var low mandatoryLabel
	low.Sid = [16]byte{1, 1, 0, 0, 0, 0, 0, 16, 0x00, 0x10, 0, 0, 0, 0, 0, 0}
	low.Label.Sid = &low.Sid[0]
	low.Label.Attributes = 0x20                                         // SE_GROUP_INTEGRITY
	if rc, _, err := pSetTokenInformation.Call(uintptr(restricted), 25, /*TokenIntegrityLevel*/
		uintptr(unsafe.Pointer(&low)), uintptr(unsafe.Sizeof(low))); rc == 0 {
		fmt.Println("SetTokenInformation(TokenIntegrityLevel=Low): FAIL", err)
	} else {
		fmt.Println("SetTokenInformation(TokenIntegrityLevel=Low): OK")
	}

	cmdline, _ := syscall.UTF16PtrFromString(`C:\windows\system32\cmd.exe /c echo restricted-write > ` + target)
	var si syscall.StartupInfo
	si.Cb = uint32(unsafe.Sizeof(si))
	var pi syscall.ProcessInformation
	rc, _, err = pCreateProcessAsUserW.Call(
		uintptr(restricted), 0, uintptr(unsafe.Pointer(cmdline)), 0, 0, 0,
		0x08000000 /*CREATE_NO_WINDOW*/, 0, 0, uintptr(unsafe.Pointer(&si)), uintptr(unsafe.Pointer(&pi)))
	if rc == 0 {
		fmt.Println("CreateProcessAsUserW failed:", err)
		return
	}
	fmt.Println("CreateProcessAsUserW: OK")
	pWaitForSingleObject.Call(uintptr(pi.Process), 30000)
	var code uint32
	pGetExitCodeProcess.Call(uintptr(pi.Process), uintptr(unsafe.Pointer(&code)))
	fmt.Printf("child exit=%d\n", code)

	if _, statErr := os.Stat(target); statErr == nil {
		fmt.Println("RESULT: restricted token STILL WROTE the file -> Wine does NOT enforce restricting SIDs")
	} else {
		fmt.Println("RESULT: write denied (restricting SIDs enforced)")
	}
}
