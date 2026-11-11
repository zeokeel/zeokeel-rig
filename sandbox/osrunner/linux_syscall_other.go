//go:build linux && !amd64 && !arm64

package osrunner

import (
	"errors"
	"unsafe"
)

// landlockSupported 报告当前架构是否实现 Landlock 原始系统调用。其它 Linux 架构
// 暂不提供原始系统调用号，因此 Landlock 后端整体不可用（fail closed）。
const landlockSupported = false

// errLandlockUnsupported 是未支持架构上 Landlock 原始系统调用的统一错误。
var errLandlockUnsupported = errors.New("landlock: unsupported architecture")

// landlockCreateRuleset 在未支持的架构上恒失败。
func landlockCreateRuleset(unsafe.Pointer, uintptr, uint32) (int, error) {
	return -1, errLandlockUnsupported
}

// landlockAddRule 在未支持的架构上恒失败。
func landlockAddRule(int, uint32, unsafe.Pointer, uint32) error {
	return errLandlockUnsupported
}

// landlockRestrictSelf 在未支持的架构上恒失败。
func landlockRestrictSelf(int, uint32) error {
	return errLandlockUnsupported
}

// landlockSetNoNewPrivileges 在未支持的架构上恒失败。
func landlockSetNoNewPrivileges() error {
	return errLandlockUnsupported
}
