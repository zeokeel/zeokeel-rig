//go:build windows

package osrunner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

var (
	winCanonicalKernel32 = syscall.NewLazyDLL("kernel32.dll")

	winGetFinalPathNameByHandleW = winCanonicalKernel32.NewProc("GetFinalPathNameByHandleW")
	winGetLongPathNameW          = winCanonicalKernel32.NewProc("GetLongPathNameW")
)

const (
	// winVolumeNameDos 是 GetFinalPathNameByHandleW 的 FILE_NAME_NORMALIZED|VOLUME_NAME_DOS。
	winVolumeNameDos = 0x00000000
)

// winCanonicalPathForPlatform 把路径规范化成**唯一一种写法**，用于一切包含/相等
// 比较（工作区根、保护路径、临时目录）。
//
// 为什么必须是同一个函数：Windows 上同一个目录有很多等价写法——8.3 短名
// （`C:\Users\RUNNER~1\...` vs `C:\Users\runneradmin\...`）、大小写、`\\?\` 前缀、
// junction/符号链接。授权面（`--workspace`）与保护面（`--protect`）如果各自保留
// 自己的写法，`winPathWithinFold` 就会判不出"保护路径在工作区之内"，helper 于是
// **跳过保护**（fail-open，`.git` 变可写）。CI 上就是这样炸的：GitHub runner 的
// `%TEMP%` 是 8.3 短名 `C:\Users\RUNNER~1\...`，而策略根被解析成了长名。
//
// 实现优先级：
//  1. GetFinalPathNameByHandleW：按**打开后的句柄**取最终路径，一次解决 8.3、
//     大小写、reparse point（junction/符号链接）；返回值带 `\\?\` 前缀，剥掉；
//  2. GetLongPathNameW：句柄方式不可用时（例如没有读权限）至少把 8.3 展开成长名；
//  3. filepath.EvalSymlinks：最后兜底（至少去掉 "." / ".." 与已知链接）。
//
// 路径不存在时返回错误（调用方据此判定"不存在"，与 os.Stat 语义一致）。
func winCanonicalPathForPlatform(path string) (string, error) {
	cleaned := filepath.Clean(strings.TrimSpace(path))
	if cleaned == "" || cleaned == "." {
		return "", fmt.Errorf("windows-acl: cannot canonicalize empty path")
	}
	if resolved, err := winFinalPathByHandle(cleaned); err == nil && resolved != "" {
		return resolved, nil
	}
	if resolved, err := winLongPathName(cleaned); err == nil && resolved != "" {
		return filepath.Clean(resolved), nil
	}
	real, err := filepath.EvalSymlinks(cleaned)
	if err != nil {
		return "", err
	}
	return filepath.Clean(real), nil
}

// winFinalPathByHandle 用句柄取最终路径（最权威的规范化）。
func winFinalPathByHandle(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	handle := syscall.Handle(file.Fd())

	buffer := make([]uint16, syscall.MAX_LONG_PATH)
	length, _, callErr := winGetFinalPathNameByHandleW.Call(
		uintptr(handle),
		uintptr(unsafe.Pointer(&buffer[0])),
		uintptr(len(buffer)),
		winVolumeNameDos,
	)
	if length == 0 {
		return "", callErr
	}
	if int(length) >= len(buffer) {
		return "", fmt.Errorf("windows-acl: final path for %q exceeds %d UTF-16 units", path, len(buffer))
	}
	return winStripExtendedPrefix(syscall.UTF16ToString(buffer[:length])), nil
}

// winLongPathName 把 8.3 短名展开成长名。
func winLongPathName(path string) (string, error) {
	pointer, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	buffer := make([]uint16, syscall.MAX_LONG_PATH)
	length, _, callErr := winGetLongPathNameW.Call(
		uintptr(unsafe.Pointer(pointer)),
		uintptr(unsafe.Pointer(&buffer[0])),
		uintptr(len(buffer)),
	)
	if length == 0 {
		return "", callErr
	}
	if int(length) >= len(buffer) {
		return "", fmt.Errorf("windows-acl: long path for %q exceeds %d UTF-16 units", path, len(buffer))
	}
	return syscall.UTF16ToString(buffer[:length]), nil
}
