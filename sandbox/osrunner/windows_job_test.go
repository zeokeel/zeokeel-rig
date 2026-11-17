package osrunner

// 本文件在任意宿主上单测 Windows 进程树治理的纯逻辑（windows_job.go）：结果模型、
// 退出码映射、可识别诊断文案、看门狗时限到 WaitForSingleObject 参数的映射、
// spawn 的 fail-closed 前置条件、CreateProcess 环境块编码，以及 JOBOBJECT_* 结构
// 布局镜像。真实 Job/spawn 行为见 windows_runtime_test.go（RIG_WINDOWS_SANDBOX_TEST=1）。

import (
	"strings"
	"testing"
	"time"
	"unicode/utf16"
	"unsafe"
)

func TestWinJobWaitMilliseconds(t *testing.T) {
	cases := []struct {
		name    string
		timeout time.Duration
		want    uint32
	}{
		{name: "no watchdog", timeout: 0, want: 0xFFFFFFFF},
		{name: "negative is infinite", timeout: -time.Second, want: 0xFFFFFFFF},
		{name: "sub-millisecond rounds up to 1", timeout: time.Nanosecond, want: 1},
		{name: "exactly one millisecond", timeout: time.Millisecond, want: 1},
		{name: "rounds up", timeout: 1500 * time.Microsecond, want: 2},
		{name: "whole seconds", timeout: 30 * time.Second, want: 30000},
		{name: "clamped below INFINITE", timeout: time.Duration(1 << 62), want: 0xFFFFFFFE},
	}
	for _, testCase := range cases {
		if got := winJobWaitMilliseconds(testCase.timeout); got != testCase.want {
			t.Fatalf("%s: winJobWaitMilliseconds(%v) = %#x, want %#x", testCase.name, testCase.timeout, got, testCase.want)
		}
	}
}

func TestWinShouldTerminateTree(t *testing.T) {
	if winShouldTerminateTree(0) {
		t.Fatal("winShouldTerminateTree(0) = true, want false (nothing to kill)")
	}
	for _, active := range []int{1, 2, 100} {
		if !winShouldTerminateTree(active) {
			t.Fatalf("winShouldTerminateTree(%d) = false, want true", active)
		}
	}
}

func TestWinOutcomeExitCode(t *testing.T) {
	if got := winOutcomeExitCode(winProcessOutcome{ExitCode: 3}); got != 3 {
		t.Fatalf("exit code = %d, want the command's own code 3", got)
	}
	// 超时：固定 124，绝不伪装成命令自己的退出码。
	timedOut := winProcessOutcome{ExitCode: 0, TimedOut: true, Timeout: 500 * time.Millisecond}
	if got := winOutcomeExitCode(timedOut); got != winTreeTerminatedExitCode {
		t.Fatalf("timeout exit code = %d, want %d", got, winTreeTerminatedExitCode)
	}
}

func TestWinTreeTerminationNotice(t *testing.T) {
	// 没有收掉任何东西：不打印噪声。
	quiet := winProcessOutcome{ExitCode: 0}
	if notice := winTreeTerminationNotice(quiet); notice != "" {
		t.Fatalf("notice = %q, want empty", notice)
	}
	// 顶层命令正常退出、后代被收掉：保持命令自己的退出码，但要有可识别文案。
	descendants := winProcessOutcome{ExitCode: 7, Tree: winTreeKillResult{Terminated: 3}}
	notice := winTreeTerminationNotice(descendants)
	if !strings.HasPrefix(notice, winTreeTerminationNoticePrefix) {
		t.Fatalf("notice = %q, want the %q prefix", notice, winTreeTerminationNoticePrefix)
	}
	if !strings.Contains(notice, "3 descendant process(es) killed") {
		t.Fatalf("notice = %q, want the descendant count", notice)
	}
	if strings.Contains(notice, "timeout") {
		t.Fatalf("notice = %q, want no timeout wording", notice)
	}
	// 超时：文案必须点明 timeout，并带上被终止的后代数。
	timedOut := winProcessOutcome{TimedOut: true, Timeout: 1500 * time.Millisecond, Tree: winTreeKillResult{Terminated: 2}}
	notice = winTreeTerminationNotice(timedOut)
	if !strings.HasPrefix(notice, winTreeTerminationNoticePrefix) || !strings.Contains(notice, "timeout after 1.5s") {
		t.Fatalf("notice = %q, want the timeout wording", notice)
	}
	if !strings.Contains(notice, "2 descendant process(es) killed") {
		t.Fatalf("notice = %q, want the descendant count", notice)
	}
	// 超时但树里只有顶层进程：still 要有 timeout 文案（不出现 0 个后代这种噪声）。
	onlyTop := winProcessOutcome{TimedOut: true, Timeout: time.Second}
	notice = winTreeTerminationNotice(onlyTop)
	if !strings.HasPrefix(notice, winTreeTerminationNoticePrefix) || !strings.Contains(notice, "timeout after 1s") {
		t.Fatalf("notice = %q, want the timeout wording", notice)
	}
	if strings.Contains(notice, "0 descendant") {
		t.Fatalf("notice = %q, want no zero-count noise", notice)
	}
	// 等待上限之后仍有进程存活：必须显式说出来（几乎不可能，但不能静默）。
	stuck := winProcessOutcome{ExitCode: 0, Tree: winTreeKillResult{Terminated: 4, Remaining: 2}}
	notice = winTreeTerminationNotice(stuck)
	if !strings.Contains(notice, "2 process(es) still alive") {
		t.Fatalf("notice = %q, want the stuck-process warning", notice)
	}
}

func TestWinSpawnPrecondition(t *testing.T) {
	if err := winSpawnPrecondition(true, true); err != nil {
		t.Fatalf("winSpawnPrecondition(true, true) = %v, want nil", err)
	}
	// 没有 Job：fail closed，且错误必须说清"拒绝 spawn"而不是含糊失败。
	err := winSpawnPrecondition(false, true)
	if err == nil {
		t.Fatal("winSpawnPrecondition(false, true) = nil, want fail closed")
	}
	if !strings.Contains(err.Error(), "refusing to spawn") {
		t.Fatalf("error = %q, want it to say it refuses to spawn", err)
	}
	// 布局不匹配（32 位 Windows）：同样 fail closed，而且最先判定。
	for _, layoutOK := range []bool{true, false} {
		err := winSpawnPrecondition(false, layoutOK)
		if err == nil {
			t.Fatalf("winSpawnPrecondition(false, %t) = nil, want fail closed", layoutOK)
		}
	}
	if err := winSpawnPrecondition(true, false); err == nil || !strings.Contains(err.Error(), "struct layout") {
		t.Fatalf("winSpawnPrecondition(true, false) = %v, want a layout error", err)
	}
}

// winTestEnvBlock 把字符串按 CreateProcess 的规则编码成期望的环境块。
func winTestEnvBlock(entries ...string) []uint16 {
	var block []uint16
	for _, entry := range entries {
		for _, char := range entry {
			block = utf16.AppendRune(block, char)
		}
		block = utf16.AppendRune(block, 0)
	}
	return utf16.AppendRune(block, 0)
}

func TestWinBuildEnvironmentBlock(t *testing.T) {
	if got := mustEnvBlock(t, nil); len(got) != 2 || got[0] != 0 || got[1] != 0 {
		t.Fatalf("empty env block = %v, want two NULs", got)
	}
	// 必须按变量名（ASCII 大写）排序：Windows 文档要求，且同名变量的优先级依赖它。
	got := mustEnvBlock(t, []string{"b=2", "A=1", "c=3"})
	want := winTestEnvBlock("A=1", "b=2", "c=3")
	if !equalUTF16(got, want) {
		t.Fatalf("env block = %v, want %v", got, want)
	}
	// 大小写不同的同名变量保持输入顺序（稳定排序）。
	got = mustEnvBlock(t, []string{"Path=first", "PATH=second"})
	want = winTestEnvBlock("Path=first", "PATH=second")
	if !equalUTF16(got, want) {
		t.Fatalf("duplicate-key env block = %v, want %v", got, want)
	}
	// 空条目被跳过：空字符串会在块中间插入提前终止符，让后面的变量静默丢失。
	got = mustEnvBlock(t, []string{"", "A=1", ""})
	want = winTestEnvBlock("A=1")
	if !equalUTF16(got, want) {
		t.Fatalf("env block with empties = %v, want %v", got, want)
	}
	// 含 NUL 的条目必须报错（否则块会被截断）。
	if _, err := winBuildEnvironmentBlock([]string{"A=1\x00B=2"}); err == nil {
		t.Fatal("winBuildEnvironmentBlock(entry with NUL) = nil error, want an error")
	}
	// 非 ASCII 值按 UTF-16 编码（emoji 是代理对，占两个 code unit）。
	got = mustEnvBlock(t, []string{"RIG=🚀"})
	if want := winTestEnvBlock("RIG=🚀"); !equalUTF16(got, want) {
		t.Fatalf("env block = %v, want %v (surrogate pair)", got, want)
	}
	if len(got) != 4+2+1+1 {
		t.Fatalf("env block length = %d, want 8 (RIG= + surrogate pair + two NULs)", len(got))
	}
}

// mustEnvBlock 调用 winBuildEnvironmentBlock 并在失败时终止测试。
func mustEnvBlock(t *testing.T, environment []string) []uint16 {
	t.Helper()
	block, err := winBuildEnvironmentBlock(environment)
	if err != nil {
		t.Fatalf("winBuildEnvironmentBlock(%v): %v", environment, err)
	}
	return block
}

// equalUTF16 比较两个 UTF-16 块（nil 与空视为不同长度）。
func equalUTF16(left, right []uint16) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// TestWinJobLayoutMirrors 固定 JOBOBJECT_* 的 64 位布局（windows_layout.go）。
func TestWinJobLayoutMirrors(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("Job 对象布局只对 64 位宿主（LLP64）有意义")
	}
	if size := unsafe.Sizeof(winJobBasicLimitInformation{}); size != 64 {
		t.Fatalf("JOBOBJECT_BASIC_LIMIT_INFORMATION size = %d, want 64", size)
	}
	if size := unsafe.Sizeof(winJobExtendedLimitInformation{}); size != 144 {
		t.Fatalf("JOBOBJECT_EXTENDED_LIMIT_INFORMATION size = %d, want 144", size)
	}
	if size := unsafe.Sizeof(winJobBasicAccountingInformation{}); size != 48 {
		t.Fatalf("JOBOBJECT_BASIC_ACCOUNTING_INFORMATION size = %d, want 48", size)
	}
	if winJobAccountingActiveProcessesOffset != 40 {
		t.Fatalf("ActiveProcesses offset = %d, want 40", winJobAccountingActiveProcessesOffset)
	}
	if !winJobLayoutSupported {
		t.Fatal("winJobLayoutSupported = false on a 64-bit host")
	}
	// LimitFlags 必须落在改结构可写的位置上：用它验证字段顺序没被改坏。
	limits := winJobExtendedLimitInformation{}
	limits.BasicLimitInformation.LimitFlags = winJobObjectLimitKillOnJobClose
	if limits.BasicLimitInformation.LimitFlags != 0x00002000 {
		t.Fatalf("LimitFlags = %#x, want 0x00002000", limits.BasicLimitInformation.LimitFlags)
	}
	if offset := unsafe.Offsetof(winJobExtendedLimitInformation{}.BasicLimitInformation.LimitFlags); offset != 16 {
		t.Fatalf("LimitFlags offset = %d, want 16", offset)
	}
}

// TestWinJobTerminationNoticeIsRecognizable 固定"可识别文本"这条契约：宿主的归因
// 逻辑（以及用户肉眼）靠这个前缀判断"命令不是自己失败，而是被沙箱收了树"。
func TestWinJobTerminationNoticeIsRecognizable(t *testing.T) {
	if winTreeTerminationNoticePrefix != "windows-acl: process tree terminated by sandbox" {
		t.Fatalf("notice prefix = %q, want the documented literal", winTreeTerminationNoticePrefix)
	}
	if winTreeTerminatedExitCode != 124 {
		t.Fatalf("tree-terminated exit code = %d, want 124", winTreeTerminatedExitCode)
	}
}

// TestWinEnvironmentBlockFor 固定"nil 表示继承、非 nil 表示替换"这条语义：
// 传一个空块会让孩子进程得到空环境（PATH 都没了），必须靠 NULL 指针表达继承。
func TestWinEnvironmentBlockFor(t *testing.T) {
	block, err := winEnvironmentBlockFor(nil)
	if err != nil {
		t.Fatalf("winEnvironmentBlockFor(nil): %v", err)
	}
	if block != nil {
		t.Fatalf("winEnvironmentBlockFor(nil) = %v, want nil (inherit the helper environment)", block)
	}
	block, err = winEnvironmentBlockFor([]string{})
	if err != nil {
		t.Fatalf("winEnvironmentBlockFor(empty): %v", err)
	}
	if block == nil || len(block) != 2 || block[0] != 0 || block[1] != 0 {
		t.Fatalf("winEnvironmentBlockFor(empty) = %v, want an empty replacement block", block)
	}
	block, err = winEnvironmentBlockFor([]string{"B=2", "A=1"})
	if err != nil {
		t.Fatalf("winEnvironmentBlockFor(entries): %v", err)
	}
	if want := winTestEnvBlock("A=1", "B=2"); !equalUTF16(block, want) {
		t.Fatalf("block = %v, want %v", block, want)
	}
	if _, err := winEnvironmentBlockFor([]string{"A=1\x00"}); err == nil {
		t.Fatal("winEnvironmentBlockFor(entry with NUL) = nil error, want an error")
	}
}
