//go:build windows

package osrunner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"unsafe"
)

// 本文件是 Windows 上的真实运行时测试：令牌/ACL/spawn 都真的执行。它带
// //go:build windows，在 Linux 宿主上不参与编译（交叉编译时只做类型检查）。
// 环境无法支撑 ACL 后端（例如受限 CI 容器）时以 t.Skip 说明原因，而不是让
// 宿主平台能力问题伪装成代码回归。

// requireWindowsACL 在 ACL 后端不可用的宿主上跳过测试。
func requireWindowsACL(t *testing.T) {
	t.Helper()
	if err := probeWindowsACL(); err != nil {
		t.Skipf("windows ACL backend unavailable on this host: %v", err)
	}
}

func TestWindowsRuntimeStructLayout(t *testing.T) {
	if got, want := unsafe.Sizeof(winTrustee{}), uintptr(32); got != want {
		t.Fatalf("winTrustee size = %d, want %d", got, want)
	}
	if got, want := unsafe.Sizeof(winExplicitAccess{}), uintptr(48); got != want {
		t.Fatalf("winExplicitAccess size = %d, want %d", got, want)
	}
	if offset := unsafe.Offsetof(winExplicitAccess{}.trustee.name); offset != 40 {
		t.Fatalf("ptstrName offset = %d, want 40 (EXPLICIT_ACCESS_W layout)", offset)
	}
}

func TestWindowsRuntimeCapabilitySIDParses(t *testing.T) {
	text, err := newCapabilitySID()
	if err != nil {
		t.Fatalf("newCapabilitySID: %v", err)
	}
	sid, err := winParseSID(text)
	if err != nil {
		t.Fatalf("winParseSID(%q): %v", text, err)
	}
	roundTrip, err := sid.String()
	if err != nil {
		t.Fatalf("SID.String: %v", err)
	}
	if roundTrip != text {
		t.Fatalf("SID round trip = %q, want %q", roundTrip, text)
	}
	if length := syscall.GetLengthSid(sid); length == 0 || length > winSIDMaxSize {
		t.Fatalf("GetLengthSid(capability) = %d, want 1..%d", length, winSIDMaxSize)
	}
}

func TestWindowsRuntimeRestrictedToken(t *testing.T) {
	requireWindowsACL(t)
	logon, err := winCurrentLogonSID()
	if err != nil {
		t.Fatalf("winCurrentLogonSID: %v", err)
	}
	restricting, err := windowsRestrictingSIDStrings(winModeReadOnly, logon, winEveryoneSIDString, "", "")
	if err != nil {
		t.Fatalf("windowsRestrictingSIDStrings: %v", err)
	}
	token, err := winNewRestrictedToken(restricting)
	if err != nil {
		t.Fatalf("winNewRestrictedToken: %v", err)
	}
	defer token.close()
	if token.handle == 0 {
		t.Fatal("winNewRestrictedToken returned a zero handle")
	}
	lowLabel, err := winParseSID(winLowIntegritySIDString)
	if err != nil {
		t.Fatalf("winParseSID(low): %v", err)
	}
	if err := winRestrictTokenIntegrity(token.handle, lowLabel); err != nil {
		t.Fatalf("winRestrictTokenIntegrity: %v", err)
	}
}

func TestWindowsRuntimeProbeHelper(t *testing.T) {
	requireWindowsACL(t)
	handled, code := runWindowsACLHelper([]string{HelperArgWindowsACL, winCommandProbe})
	if !handled {
		t.Fatal("runWindowsACLHelper did not handle the probe invocation")
	}
	if code != 0 {
		t.Fatalf("probe exit code = %d, want 0", code)
	}
}

func TestWindowsRuntimeHelperRejectsBadArgs(t *testing.T) {
	handled, code := runWindowsACLHelper([]string{HelperArgWindowsACL, "--bogus"})
	if !handled {
		t.Fatal("runWindowsACLHelper did not handle a helper invocation with bad args")
	}
	if code != winHelperFailureCode {
		t.Fatalf("bad-args exit code = %d, want %d", code, winHelperFailureCode)
	}
	if handled, _ := runWindowsACLHelper([]string{"--bogus"}); handled {
		t.Fatal("runWindowsACLHelper handled a non-helper invocation")
	}
}

func TestWindowsRuntimeWorkspaceWriteAllowsWorkspace(t *testing.T) {
	requireWindowsACL(t)
	workspace := t.TempDir()
	target := filepath.Join(workspace, "inside.txt")
	// cmd 的重定向必须写在同一个命令字符串里，否则 ">" 只是一个普通参数。
	plan := winACLPlan{
		Workspaces: []string{workspace},
		Mode:       winModeWorkspaceWrite,
		Argv:       []string{winShellCommand(), "/c", "echo inside> " + quoteWindowsPath(target)},
	}
	code, err := runWindowsACLPlanConfined(plan)
	if err != nil {
		t.Fatalf("runWindowsACLPlanConfined: %v", err)
	}
	if code != 0 {
		t.Fatalf("child exit code = %d, want 0", code)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("workspace write was not allowed: %v", err)
	}
}

func TestWindowsRuntimeWorkspaceWriteDeniesOutside(t *testing.T) {
	requireWindowsACL(t)
	workspace := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "denied.txt")
	plan := winACLPlan{
		Workspaces: []string{workspace},
		Mode:       winModeWorkspaceWrite,
		Argv:       []string{winShellCommand(), "/c", "echo denied> " + quoteWindowsPath(target)},
	}
	code, err := runWindowsACLPlanConfined(plan)
	if err != nil {
		t.Fatalf("runWindowsACLPlanConfined: %v", err)
	}
	if code == 0 {
		t.Fatalf("write outside the workspace succeeded (exit 0), want a denial")
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatalf("write outside the workspace created %q, want denial", target)
	}
}

func TestWindowsRuntimeReadOnlyDeniesWorkspaceWrite(t *testing.T) {
	requireWindowsACL(t)
	workspace := t.TempDir()
	existing := filepath.Join(workspace, "existing.txt")
	if err := os.WriteFile(existing, []byte("hello\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	readPlan := winACLPlan{
		Workspaces: []string{workspace},
		Mode:       winModeReadOnly,
		Argv:       []string{winShellCommand(), "/c", "type " + quoteWindowsPath(existing)},
	}
	if code, err := runWindowsACLPlanConfined(readPlan); err != nil {
		t.Fatalf("read-only read: %v", err)
	} else if code != 0 {
		t.Fatalf("read-only read exit code = %d, want 0", code)
	}

	target := filepath.Join(workspace, "denied.txt")
	writePlan := winACLPlan{
		Workspaces: []string{workspace},
		Mode:       winModeReadOnly,
		Argv:       []string{winShellCommand(), "/c", "echo denied> " + quoteWindowsPath(target)},
	}
	if code, err := runWindowsACLPlanConfined(writePlan); err != nil {
		t.Fatalf("read-only write: %v", err)
	} else if code == 0 {
		t.Fatal("read-only mode allowed a workspace write")
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatalf("read-only mode created %q, want denial", target)
	}
}

func TestWindowsRuntimeReadOnlyLeavesNoGrant(t *testing.T) {
	requireWindowsACL(t)
	workspace := t.TempDir()
	plan := winACLPlan{
		Workspaces: []string{workspace},
		Mode:       winModeReadOnly,
		Argv:       []string{winShellCommand(), "/c", "exit 0"},
	}
	if _, err := runWindowsACLPlanConfined(plan); err != nil {
		t.Fatalf("runWindowsACLPlanConfined: %v", err)
	}
	// 只读模式不授予任何目录写权限：用当前（未受限）令牌仍能正常写入并清理，
	// 说明 helper 没有把目录改坏。
	target := filepath.Join(workspace, "after.txt")
	if err := os.WriteFile(target, []byte("ok"), 0o600); err != nil {
		t.Fatalf("workspace became unwritable after a read-only run: %v", err)
	}
}

func TestWindowsRuntimeRefusesTempRootInsideWorkspace(t *testing.T) {
	// helper 的私有临时目录来自 os.TempDir，无法从测试注入、也不应位于工作区
	// 内部；这里验证决定该行为的纯函数。工作区恰好包含 Temp 的宿主会被 helper
	// 以 windows-acl-run: 前置失败拒绝运行（fail closed）。
	workspace := t.TempDir()
	if windowsTempRootUsable([]string{workspace}, filepath.Join(workspace, "tmp")) {
		t.Fatal("windowsTempRootUsable accepted a temp root inside the workspace")
	}
	if !windowsTempRootUsable([]string{workspace}, filepath.Join(filepath.Dir(workspace), "other-tmp")) {
		t.Fatal("windowsTempRootUsable rejected a disjoint temp root")
	}
}

// TestWindowsRuntimeConcurrentWorkspaceWrites 是并发 ACL 修复的真实回归：
// 4 个 helper 同时在同一个 workspace 授权/写文件/退出，任何一个都不能因为另一个
// 的 grant/restore 而失败。状态文件在全部 helper 退出后必须被删除，目录恢复可写。
//
// 需要真实 Win32 ACL 后端，默认跳过：
//
//	RIG_WINDOWS_SANDBOX_TEST=1 go test ./sandbox/osrunner -run TestWindowsRuntimeConcurrentWorkspaceWrites -v
func TestWindowsRuntimeConcurrentWorkspaceWrites(t *testing.T) {
	if os.Getenv("RIG_WINDOWS_SANDBOX_TEST") != "1" {
		t.Skip("set RIG_WINDOWS_SANDBOX_TEST=1 to run the real concurrent Windows ACL test")
	}
	requireWindowsACL(t)
	workspace := t.TempDir()
	const workers = 4
	var wait sync.WaitGroup
	codes := make(chan int, workers)
	errs := make(chan error, workers)
	for index := 0; index < workers; index++ {
		target := filepath.Join(workspace, fmt.Sprintf("concurrent-%d.txt", index))
		wait.Add(1)
		go func(index int, target string) {
			defer wait.Done()
			plan := winACLPlan{
				Workspaces: []string{workspace},
				Mode:       winModeWorkspaceWrite,
				Argv: []string{winShellCommand(), "/c",
					fmt.Sprintf("echo worker-%d> %s", index, quoteWindowsPath(target))},
			}
			code, err := runWindowsACLPlanConfined(plan)
			if err != nil {
				errs <- fmt.Errorf("worker %d: %w", index, err)
				return
			}
			codes <- code
		}(index, target)
	}
	wait.Wait()
	close(codes)
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent helper failed: %v", err)
	}
	for code := range codes {
		if code != 0 {
			t.Fatalf("concurrent helper exit code = %d, want 0", code)
		}
	}
	for index := 0; index < workers; index++ {
		target := filepath.Join(workspace, fmt.Sprintf("concurrent-%d.txt", index))
		if _, err := os.Stat(target); err != nil {
			t.Fatalf("worker %d did not write its file: %v", index, err)
		}
	}
	// None of the helpers may leave the coordination state behind (the last one
	// restores the original descriptor and removes the file).
	pathHash := winACLPathHash(filepath.Clean(workspace))
	statePath := winACLStateFilePath(os.Getenv("LOCALAPPDATA"), os.Getenv("TEMP"), pathHash)
	if statePath != "" {
		if _, err := os.Stat(statePath); !os.IsNotExist(err) {
			t.Fatalf("ACL state file survived the last helper: %s (err %v)", statePath, err)
		}
	}
	// The directory must be restored: a normal (unrestricted) write still works.
	if err := os.WriteFile(filepath.Join(workspace, "after-concurrent.txt"), []byte("ok"), 0o600); err != nil {
		t.Fatalf("workspace not restored after concurrent helpers: %v", err)
	}
}

// TestWindowsRuntimeKnownFolders 是 GetWindowsDirectoryW / GetTempPath2W 的
// 回归测试：两个 Win32 调用的参数顺序与本包其它 API 相反（buffer 在前、容量
// 在后），顺序写反时 Win32 会把容量 260（0x104）当成输出缓冲区指针，整个
// 进程以 0xC0000005 崩溃——这正是 v0.15.1 起 Windows ACL 探测失败、所有命令
// 执行通道不可用的根因。GetTempPath2W 在旧系统上不存在，winGetTempPath 必须
// 先 Find 再 Call，不能靠 panic 兜底。
func TestWindowsRuntimeKnownFolders(t *testing.T) {
	dir := winWindowsDirectory()
	if dir == "" {
		t.Fatal("winWindowsDirectory returned an empty path")
	}
	if !filepath.IsAbs(dir) {
		t.Fatalf("winWindowsDirectory returned a non-absolute path: %q", dir)
	}
	if temp := winGetTempPath(); temp != "" && !filepath.IsAbs(temp) {
		t.Fatalf("winGetTempPath returned a non-absolute path: %q", temp)
	}
}

// winShellCommand 返回系统命令解释器路径。
func winShellCommand() string {
	if command := strings.TrimSpace(os.Getenv("COMSPEC")); command != "" {
		return command
	}
	return "cmd.exe"
}

// quoteWindowsPath 给路径加上 cmd 需要的双引号。
func quoteWindowsPath(path string) string {
	return `"` + path + `"`
}
