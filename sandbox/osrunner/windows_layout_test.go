package osrunner

// 本文件在任意宿主上断言 Win32 EXPLICIT_ACCESS_W / TRUSTEE_W 的布局镜像（B4）。
//
// 它守的是一个只在真机才暴露的坑：unsafe.Offsetof 对多级 selector 只返回**最内层**
// 结构体内的偏移。旧实现把 unsafe.Offsetof(winExplicitAccess{}.trustee.name) 与 40
// 比较，go1.27 实测是 24，于是
//   - windows_runtime_test.go 的布局断言在真机恒失败；
//   - 同一个表达式被 windows_syscall.go 当作 winLayoutSupported 守门常量，恒为
//     false，Windows ACL 后端整体 fail closed。
//
// 现在两端都用"显式相加的两个一级偏移"，本测试同时固定 Go 的语义与这个和。

import (
	"testing"
	"unsafe"
)

func TestWinExplicitAccessOffsetsExplicitSum(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("EXPLICIT_ACCESS_W 布局守门只对 64 位宿主（LLP64）有意义")
	}
	// 构造一次以固定字段顺序；同时让静态检查看到每个字段都被使用（布局镜像在
	// 非 Windows 构建里没有别处的读者）。
	probe := winExplicitAccess{
		accessPermissions: 1,
		accessMode:        2,
		inheritance:       3,
		trustee: winTrustee{
			multipleTrustee:          0,
			multipleTrusteeOperation: 4,
			trusteeForm:              5,
			trusteeType:              6,
			name:                     7,
		},
	}
	_ = probe.accessPermissions
	_ = probe.accessMode
	_ = probe.inheritance
	_ = probe.trustee.multipleTrustee
	_ = probe.trustee.multipleTrusteeOperation
	_ = probe.trustee.trusteeForm
	_ = probe.trustee.trusteeType
	_ = probe.trustee.name

	if winTrusteeSize != 32 {
		t.Fatalf("winTrustee size = %d, want 32", winTrusteeSize)
	}
	if winExplicitAccessSize != 48 {
		t.Fatalf("winExplicitAccess size = %d, want 48", winExplicitAccessSize)
	}
	if winExplicitAccessTrusteeOffset != 16 {
		t.Fatalf("TRUSTEE_W offset = %d, want 16", winExplicitAccessTrusteeOffset)
	}
	if winTrusteeNameFieldOffset != 24 {
		t.Fatalf("ptstrName offset inside TRUSTEE_W = %d, want 24", winTrusteeNameFieldOffset)
	}

	// 多级 selector 只返回最内层偏移：这正是旧断言/守门常量写错的地方。
	nested := unsafe.Offsetof(winExplicitAccess{}.trustee.name)
	if nested != winTrusteeNameFieldOffset {
		t.Fatalf("unsafe.Offsetof(x.trustee.name) = %d, want %d (只返回 TRUSTEE_W 内的偏移); "+
			"Go 的语义变了，windows_syscall.go 的 winLayoutSupported 需要重新审视", nested, winTrusteeNameFieldOffset)
	}

	// ptstrName 相对 EXPLICIT_ACCESS_W 起点的偏移必须显式相加，等于 Win32 的 40。
	if winExplicitAccessTrusteeNameOffset != winExpectedTrusteeNameOffset {
		t.Fatalf("explicit ptstrName offset = %d, want %d",
			winExplicitAccessTrusteeNameOffset, winExpectedTrusteeNameOffset)
	}
	if want := uintptr(winExplicitAccessTrusteeOffset + winTrusteeNameFieldOffset); winExplicitAccessTrusteeNameOffset != want {
		t.Fatalf("explicit ptstrName offset = %d, want %d (trustee+name)", winExplicitAccessTrusteeNameOffset, want)
	}
	// 两个一级偏移相加必须严格大于最内层偏移（否则说明两个 selector 落到了同一层，
	// 显式相加失去意义）。
	if winExplicitAccessTrusteeNameOffset <= winTrusteeNameFieldOffset {
		t.Fatalf("explicit sum %d is not greater than the nested offset %d",
			winExplicitAccessTrusteeNameOffset, winTrusteeNameFieldOffset)
	}
}

// TestWinStartupInfoEXLayout 固定 STARTUPINFOEXW 的 64 位布局：Cb/Fags/ShowWindow/
// 三个标准句柄/AttributeList 的偏移必须与 winnt.h 一致，否则 CreateProcessAsUserW
// 会拿到错位的参数（真机上表现为 ERROR_INVALID_PARAMETER 或更糟）。
func TestWinStartupInfoEXLayout(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("STARTUPINFOEXW 布局只对 64 位宿主（LLP64）有意义")
	}
	// 构造一次以固定字段顺序（同时让静态检查看到每个字段都被使用）。
	probe := winStartupInfoEX{
		Cb:            112,
		Desktop:       nil,
		Title:         nil,
		X:             1,
		Y:             2,
		XSize:         3,
		YSize:         4,
		XCountChars:   5,
		YCountChars:   6,
		FillAttribute: 7,
		Flags:         8,
		ShowWindow:    9,
		StdInput:      10,
		StdOutput:     11,
		StdErr:        12,
		AttributeList: nil,
	}
	if probe.Cb != 112 || probe.Flags != 8 || probe.ShowWindow != 9 || probe.StdErr != 12 {
		t.Fatalf("layout probe = %+v", probe)
	}
	if winStartupInfoEXSize != 112 {
		t.Fatalf("STARTUPINFOEXW size = %d, want 112", winStartupInfoEXSize)
	}
	cases := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{name: "Cb", got: unsafe.Offsetof(winStartupInfoEX{}.Cb), want: 0},
		{name: "Flags", got: unsafe.Offsetof(winStartupInfoEX{}.Flags), want: 60},
		{name: "ShowWindow", got: unsafe.Offsetof(winStartupInfoEX{}.ShowWindow), want: 64},
		{name: "StdInput", got: unsafe.Offsetof(winStartupInfoEX{}.StdInput), want: 80},
		{name: "StdOutput", got: unsafe.Offsetof(winStartupInfoEX{}.StdOutput), want: 88},
		{name: "StdErr", got: unsafe.Offsetof(winStartupInfoEX{}.StdErr), want: 96},
		{name: "AttributeList", got: unsafe.Offsetof(winStartupInfoEX{}.AttributeList), want: 104},
	}
	for _, testCase := range cases {
		if testCase.got != testCase.want {
			t.Fatalf("%s offset = %d, want %d", testCase.name, testCase.got, testCase.want)
		}
	}
	if !winStartupInfoEXSupported {
		t.Fatal("winStartupInfoEXSupported = false on a 64-bit host")
	}
	// 常量必须是 winnt.h 的字面值：EXTENDED_STARTUPINFO_PRESENT 与
	// PROC_THREAD_ATTRIBUTE_HANDLE_LIST 都是"写错就静默不生效"的那种。
	if winExtendedStartupInfoPresent != 0x00080000 {
		t.Fatalf("EXTENDED_STARTUPINFO_PRESENT = %#x, want 0x00080000", winExtendedStartupInfoPresent)
	}
	if winProcThreadAttributeHandleList != 0x00020002 {
		t.Fatalf("PROC_THREAD_ATTRIBUTE_HANDLE_LIST = %#x, want 0x00020002", winProcThreadAttributeHandleList)
	}
}
