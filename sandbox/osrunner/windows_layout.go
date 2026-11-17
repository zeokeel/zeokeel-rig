package osrunner

import "unsafe"

// 本文件是 Win32 EXPLICIT_ACCESS_W / TRUSTEE_W 的 Go 镜像与布局常量。它**不带**
// build tag：windows_syscall.go 的布局守门常量与纯逻辑测试（任意宿主）都从这里
// 取偏移，避免"测试断言"与"守门常量"各写一份后再次漂移。
//
// 64 位 Windows（amd64/arm64，LLP64）上的布局：
//
//	TRUSTEE_W          32 字节：3 个指针大小字段 + 对齐填充 + ptstrName
//	EXPLICIT_ACCESS_W  48 字节：3 个 DWORD + 4 字节对齐 + TRUSTEE_W
//
// # 关键陷阱：多级 selector 的 unsafe.Offsetof
//
// unsafe.Offsetof 对 x.a.b 这种多级 selector **只返回最内层结构体内的偏移**，
// 不会累加外层偏移。go1.27.3 实测
//
//	unsafe.Offsetof(winExplicitAccess{}.trustee.name) == 24
//
// 也就是 TRUSTEE_W 内 ptstrName 的偏移，而不是相对 EXPLICIT_ACCESS_W 起点的 40。
// 因此 ptstrName 的绝对偏移必须**显式相加**两个一级偏移常量：
// winExplicitAccessTrusteeOffset + winTrusteeNameFieldOffset。
//
// 旧实现直接把 winExplicitAccess{}.trustee.name 与 40 比较：
//   - windows_runtime_test.go 里的断言在真机恒失败（24 != 40）；
//   - 同一个表达式被 windows_syscall.go 当作 winLayoutSupported 的守门常量，
//     恒为 false → Windows 后端整体 fail closed。
const (
	// winExpectedTrusteeNameOffset 是 EXPLICIT_ACCESS_W 内 ptstrName 的期望偏移。
	winExpectedTrusteeNameOffset = 40
)

// winTrustee 是 TRUSTEE_W（amd64/arm64 上 32 字节）。
type winTrustee struct {
	multipleTrustee          uintptr
	multipleTrusteeOperation uint32
	trusteeForm              uint32
	trusteeType              uint32
	_                        uint32 // 对齐 ptstrName 到 8 字节
	name                     uintptr
}

// winExplicitAccess 是 EXPLICIT_ACCESS_W（amd64/arm64 上 48 字节）：
// 3 个 DWORD + 4 字节对齐 + TRUSTEE_W。
type winExplicitAccess struct {
	accessPermissions uint32
	accessMode        uint32
	inheritance       uint32
	_                 uint32
	trustee           winTrustee
}

const (
	// winTrusteeSize / winExplicitAccessSize 是两个结构体的字节长度。
	winTrusteeSize        = unsafe.Sizeof(winTrustee{})
	winExplicitAccessSize = unsafe.Sizeof(winExplicitAccess{})
	// winExplicitAccessTrusteeOffset 是 TRUSTEE_W 在 EXPLICIT_ACCESS_W 内的偏移。
	winExplicitAccessTrusteeOffset = unsafe.Offsetof(winExplicitAccess{}.trustee)
	// winTrusteeNameFieldOffset 是 ptstrName 在 TRUSTEE_W 内的偏移。
	winTrusteeNameFieldOffset = unsafe.Offsetof(winTrustee{}.name)
	// winExplicitAccessTrusteeNameOffset 是 ptstrName 相对 EXPLICIT_ACCESS_W 起点的
	// 偏移：**显式相加**两个一级偏移，绝不使用多级 selector 的 Offsetof。
	winExplicitAccessTrusteeNameOffset = winExplicitAccessTrusteeOffset + winTrusteeNameFieldOffset
)
