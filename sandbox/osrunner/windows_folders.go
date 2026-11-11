//go:build windows

package osrunner

import (
	"path/filepath"
	"syscall"
	"unsafe"
)

// Win32 Known Folder lookup. The sandbox helper runs with a heavily filtered
// environment (PATH/HOME/LANG plus a small non-secret Windows allowlist), so
// TEMP/TMP/LOCALAPPDATA/USERPROFILE are not guaranteed to be present. Asking
// Windows for the user's LocalAppData/Profile directly makes the private temp
// directory independent of the inherited environment.

type winGUID struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

var (
	winShell32  = syscall.NewLazyDLL("shell32.dll")
	winOle32    = syscall.NewLazyDLL("ole32.dll")
	winKernel32 = syscall.NewLazyDLL("kernel32.dll")

	winSHGetKnownFolderPath = winShell32.NewProc("SHGetKnownFolderPath")
	winCoTaskMemFree        = winOle32.NewProc("CoTaskMemFree")
	winGetTempPath2W        = winKernel32.NewProc("GetTempPath2W")
	winGetTempPathW         = winKernel32.NewProc("GetTempPathW")
	winGetWindowsDirectoryW = winKernel32.NewProc("GetWindowsDirectoryW")
)

// FOLDERID_LocalAppData {F1B32785-6FBA-4FCF-9D55-7B8E7F157091}
var winFolderIDLocalAppData = winGUID{
	Data1: 0xF1B32785,
	Data2: 0x6FBA,
	Data3: 0x4FCF,
	Data4: [8]byte{0x9D, 0x55, 0x7B, 0x8E, 0x7F, 0x15, 0x70, 0x91},
}

// FOLDERID_Profile {5E6C858F-0E22-4760-9AFE-EA3317B67173}
var winFolderIDProfile = winGUID{
	Data1: 0x5E6C858F,
	Data2: 0x0E22,
	Data3: 0x4760,
	Data4: [8]byte{0x9A, 0xFE, 0xEA, 0x33, 0x17, 0xB6, 0x71, 0x73},
}

// winKnownFolder returns the current user's known-folder path, or "".
func winKnownFolder(id *winGUID) string {
	var path *uint16
	hr, _, _ := winSHGetKnownFolderPath.Call(
		uintptr(unsafe.Pointer(id)),
		0, // flags
		0, // current process token
		uintptr(unsafe.Pointer(&path)),
	)
	if hr != 0 || path == nil {
		return ""
	}
	defer winCoTaskMemFree.Call(uintptr(unsafe.Pointer(path)))
	return syscall.UTF16ToString((*[1 << 15]uint16)(unsafe.Pointer(path))[:])
}

// winKnownTempCandidates returns the Windows user temp directory derived from
// Known Folders. It survives an inherited environment that has TEMP/TMP and even
// LOCALAPPDATA stripped (the sandbox helper runs with a minimal env).
func winKnownTempCandidates() []string {
	var candidates []string
	if dir := winKnownFolder(&winFolderIDLocalAppData); dir != "" {
		candidates = append(candidates, filepath.Join(dir, "Temp"))
	}
	return candidates
}

// winGetTempPath returns the path from GetTempPath2W (Windows 11/Server 2022+),
// falling back to GetTempPathW. Both are the Windows-provided temp directory.
// The result can be C:\WINDOWS when the inherited environment is stripped; the
// caller filters the Windows directory before use.
func winGetTempPath() string {
	buf := make([]uint16, syscall.MAX_PATH)
	if r, _, _ := winGetTempPath2W.Call(uintptr(len(buf)), uintptr(unsafe.Pointer(&buf[0]))); r != 0 && r < uintptr(len(buf)) {
		return syscall.UTF16ToString(buf[:r])
	}
	if r, _, _ := winGetTempPathW.Call(uintptr(len(buf)), uintptr(unsafe.Pointer(&buf[0]))); r != 0 && r < uintptr(len(buf)) {
		return syscall.UTF16ToString(buf[:r])
	}
	return ""
}

// winWindowsDirectory returns the Windows directory from Win32, independent of
// the (possibly stripped) SystemRoot/WINDIR environment variables.
func winWindowsDirectory() string {
	buf := make([]uint16, syscall.MAX_PATH)
	r, _, _ := winGetWindowsDirectoryW.Call(uintptr(len(buf)), uintptr(unsafe.Pointer(&buf[0])))
	if r == 0 || r >= uintptr(len(buf)) {
		return ""
	}
	return syscall.UTF16ToString(buf[:r])
}
