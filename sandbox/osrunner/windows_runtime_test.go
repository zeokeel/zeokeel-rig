//go:build windows

package osrunner

import (
	"os"
	"path/filepath"
	"strings"
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
