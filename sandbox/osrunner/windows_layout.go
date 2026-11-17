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

// # Job 对象布局（JOBOBJECT_*）
//
// 后面几个结构镜像 kernel32 的 JOBOBJECT_*，同样放到本文件（不带 build tag），
// 这样 sizeof/偏移可以在任意 64 位宿主上断言，而不必依赖 Windows 真机。
//
// 64 位 Windows（LLP64）上：
//
//	JOBOBJECT_BASIC_LIMIT_INFORMATION        64 字节
//	JOBOBJECT_EXTENDED_LIMIT_INFORMATION    144 字节（64 + IO_COUNTERS 48 + 4×SIZE_T）
//	JOBOBJECT_BASIC_ACCOUNTING_INFORMATION   48 字节
//
// 32 位 Windows 上 SIZE_T 是 4 字节，布局完全不同：那几个宿主必须 fail closed
// （见 windows_job_object.go 用 winJobLayoutSupported 做守门）。

// winJobObjectLimitKillOnJobClose 是 JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE：Job 句柄
// 的最后一个引用关闭时，内核终止 Job 里的所有进程。
const winJobObjectLimitKillOnJobClose = 0x00002000

// winJobBasicLimitInformation 是 JOBOBJECT_BASIC_LIMIT_INFORMATION。
type winJobBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	_                       uint32 // 对齐后面的 SIZE_T
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	_                       uint32 // 对齐 Affinity
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

// winJobIOCounters 是 IO_COUNTERS（6 个 ULONGLONG）。
type winJobIOCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

// winJobExtendedLimitInformation 是 JOBOBJECT_EXTENDED_LIMIT_INFORMATION。
type winJobExtendedLimitInformation struct {
	BasicLimitInformation winJobBasicLimitInformation
	IoInfo                winJobIOCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// winJobBasicAccountingInformation 是 JOBOBJECT_BASIC_ACCOUNTING_INFORMATION。
type winJobBasicAccountingInformation struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

// winJobAccountingActiveProcessesOffset 是 ActiveProcesses 在记账结构里的偏移。
const winJobAccountingActiveProcessesOffset = unsafe.Offsetof(winJobBasicAccountingInformation{}.ActiveProcesses)

// winJobLayoutSupported 是编译期常量：Job 对象的结构布局只对 64 位 Windows 成立。
// 与 winLayoutSupported 一起构成 Windows 后端的布局守门（任一为 false 必须
// fail closed，绝不能拿错误布局去 SetInformationJobObject）。
const winJobLayoutSupported = unsafe.Sizeof(winJobBasicLimitInformation{}) == 64 &&
	unsafe.Sizeof(winJobExtendedLimitInformation{}) == 144 &&
	unsafe.Sizeof(winJobBasicAccountingInformation{}) == 48 &&
	winJobAccountingActiveProcessesOffset == 40

// # 受限 spawn 的 STARTUPINFOEXW 布局
//
// 镜像 STARTUPINFOEXW（STARTUPINFO + lpAttributeList）。这么做是为了用
// PROC_THREAD_ATTRIBUTE_HANDLE_LIST 精确指定"子进程只继承这三个标准句柄"——
// 与 os/exec 的做法一致。不用 handle list 时 bInheritHandles=TRUE 会把 helper
// 进程里**所有**可继承句柄都交给受沙子进程（Go 在 Windows 上默认把文件句柄创建为
// 可继承，靠 handle list 挡住），那是一条不该有的信息/篡改通道。
//
// 64 位 Windows 上 STARTUPINFO 是 104 字节，STARTUPINFOEXW 是 112 字节。
type winStartupInfoEX struct {
	Cb            uint32
	_             *uint16 // lpReserved
	Desktop       *uint16
	Title         *uint16
	X             uint32
	Y             uint32
	XSize         uint32
	YSize         uint32
	XCountChars   uint32
	YCountChars   uint32
	FillAttribute uint32
	Flags         uint32
	ShowWindow    uint16
	_             uint16
	_             *byte // lpReserved2
	StdInput      uintptr
	StdOutput     uintptr
	StdErr        uintptr
	// AttributeList 让本结构成为 STARTUPINFOEXW。
	AttributeList unsafe.Pointer
}

const (
	// winExtendedStartupInfoPresent 是 EXTENDED_STARTUPINFO_PRESENT。
	winExtendedStartupInfoPresent = 0x00080000
	// winProcThreadAttributeHandleList 是 PROC_THREAD_ATTRIBUTE_HANDLE_LIST
	// （ProcThreadAttributeHandleList=2 与 0x00020000 的宏组合）。
	winProcThreadAttributeHandleList = 0x00020002
	// winStartupInfoEXSize 是 STARTUPINFOEXW 的字节长度。
	winStartupInfoEXSize = unsafe.Sizeof(winStartupInfoEX{})
	// winStartupInfoEXSupported 是编译期守门：只在 64 位 Windows 布局下成立。
	winStartupInfoEXSupported = winStartupInfoEXSize == 112 &&
		unsafe.Offsetof(winStartupInfoEX{}.Flags) == 60 &&
		unsafe.Offsetof(winStartupInfoEX{}.ShowWindow) == 64 &&
		unsafe.Offsetof(winStartupInfoEX{}.StdInput) == 80 &&
		unsafe.Offsetof(winStartupInfoEX{}.StdErr) == 96 &&
		unsafe.Offsetof(winStartupInfoEX{}.AttributeList) == 104
)
