//go:build windows

package osrunner

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
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
	// ptstrName 的绝对偏移必须用**显式相加**的两个一级偏移（windows_layout.go）：
	// 多级 selector 的 unsafe.Offsetof 只返回最内层偏移（go1.27 实测 24，不是 40），
	// 直接拿它和 40 比较会让本断言在真机恒失败、并让 winLayoutSupported 恒为 false。
	if offset := winExplicitAccessTrusteeNameOffset; offset != winExpectedTrusteeNameOffset {
		t.Fatalf("ptstrName offset = %d, want %d (EXPLICIT_ACCESS_W layout)", offset, winExpectedTrusteeNameOffset)
	}
	if nested := unsafe.Offsetof(winExplicitAccess{}.trustee.name); nested != winTrusteeNameFieldOffset {
		t.Fatalf("nested selector offset = %d, want %d (TRUSTEE_W-internal)", nested, winTrusteeNameFieldOffset)
	}
	if !winLayoutSupported {
		t.Fatal("winLayoutSupported = false on a 64-bit Windows host")
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
	restricting, err := windowsRestrictingSIDStrings(winModeReadOnly, logon, winEveryoneSIDString, nil, "")
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
	// 缓存语义：最后一个 helper 退出后状态文件仍然存在，但必须是一条"无 grant、
	// per-run 部分已恢复"的缓存记录（描述符 + protect 账本）。绝不允许留下活 grant。
	pathHash := winACLPathHash(filepath.Clean(workspace))
	statePath := winACLStateFilePath(os.Getenv("LOCALAPPDATA"), os.Getenv("TEMP"), pathHash)
	if statePath != "" {
		data, err := os.ReadFile(statePath)
		if err != nil {
			t.Fatalf("read ACL state %s: %v", statePath, err)
		}
		state, err := parseWinACLState(data)
		if err != nil {
			t.Fatalf("parseWinACLState: %v", err)
		}
		if len(state.Grants) != 0 || state.DenyAdded || state.LabelApplied {
			t.Fatalf("state after the last helper = %+v, want a grant-free cache record", state)
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

// TestWindowsRuntimeProtectDeniesExistingAndNewEntries 是保护路径强制的真机回归：
// .git 里**已经存在**的文件与之后新建的文件都必须写不进去（继承 ACE 只覆盖新建
// 对象，所以后端必须遍历既有子树），工作区其它位置仍然可写；helper 退出后
// per-run 部分被恢复（宿主自己仍然能写 .git）。
//
//	RIG_WINDOWS_SANDBOX_TEST=1 go test ./sandbox/osrunner -run TestWindowsRuntimeProtect -v
func TestWindowsRuntimeProtectDeniesExistingAndNewEntries(t *testing.T) {
	requireWindowsACL(t)
	workspace := t.TempDir()
	gitDir := filepath.Join(workspace, ".git")
	if err := os.MkdirAll(filepath.Join(gitDir, "objects"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	existing := filepath.Join(gitDir, "HEAD")
	if err := os.WriteFile(existing, []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	// 1) 改写已存在的 .git 文件必须被拒绝。
	deniedPlan := winACLPlan{
		Workspaces: []string{workspace},
		Protected:  []string{gitDir},
		Mode:       winModeWorkspaceWrite,
		Argv:       []string{winShellCommand(), "/c", "echo hacked> " + quoteWindowsPath(existing)},
	}
	if code, err := runWindowsACLPlanConfined(deniedPlan); err != nil {
		t.Fatalf("run (existing protected file): %v", err)
	} else if code == 0 {
		t.Fatal("writing an existing protected file succeeded, want denial")
	}
	if content, err := os.ReadFile(existing); err != nil {
		t.Fatalf("read fixture: %v", err)
	} else if string(content) != "ref: refs/heads/main\n" {
		t.Fatalf("protected file content changed: %q", content)
	}

	// 2) 在保护目录里新建文件同样必须被拒绝。
	target := filepath.Join(gitDir, "objects", "new.txt")
	createPlan := winACLPlan{
		Workspaces: []string{workspace},
		Protected:  []string{gitDir},
		Mode:       winModeWorkspaceWrite,
		Argv:       []string{winShellCommand(), "/c", "echo new> " + quoteWindowsPath(target)},
	}
	if code, err := runWindowsACLPlanConfined(createPlan); err != nil {
		t.Fatalf("run (new protected file): %v", err)
	} else if code == 0 {
		t.Fatal("creating a file inside a protected directory succeeded, want denial")
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatalf("protected directory accepted a new file: %q", target)
	}

	// 3) 工作区里保护路径之外仍然可写。
	outside := filepath.Join(workspace, "allowed.txt")
	allowedPlan := winACLPlan{
		Workspaces: []string{workspace},
		Protected:  []string{gitDir},
		Mode:       winModeWorkspaceWrite,
		Argv:       []string{winShellCommand(), "/c", "echo ok> " + quoteWindowsPath(outside)},
	}
	if code, err := runWindowsACLPlanConfined(allowedPlan); err != nil {
		t.Fatalf("run (allowed file): %v", err)
	} else if code != 0 {
		t.Fatalf("workspace write outside the protected path failed with %d", code)
	}

	// 4) helper 退出后宿主（未受限令牌）仍然能写 .git：per-run 的 Everyone 拒绝与
	//    Low 标签必须已经恢复，只剩针对 capability SID 的缓存 ACE（对用户无影响）。
	if err := os.WriteFile(filepath.Join(gitDir, "after.txt"), []byte("ok"), 0o600); err != nil {
		t.Fatalf("protected directory became unwritable for the host after the run: %v", err)
	}
	// 5) 形状不变量：capability 允许 ACE 缓存保留，per-run 部分已恢复。
	backend := newWinWindowsACLBackend(workspace)
	sid, err := winWorkspaceCapabilitySID(filepath.Clean(workspace))
	if err != nil {
		t.Fatalf("winWorkspaceCapabilitySID: %v", err)
	}
	shape, err := backend.ACLShape(sid)
	if err != nil {
		t.Fatalf("ACLShape: %v", err)
	}
	if !shape.CapabilityAllow {
		t.Fatal("cached capability allow ACE is missing after the run")
	}
	if shape.WorkspaceDeny || shape.LowLabel {
		t.Fatalf("per-run parts were not restored: deny=%t label=%t", shape.WorkspaceDeny, shape.LowLabel)
	}
}

// TestWindowsRuntimeProtectRejectsInvalidPaths 固定 protect 的 fail-closed 校验：
// 不存在的路径、非目录、以及工作区根本身都必须拒绝运行（不 spawn）。
func TestWindowsRuntimeProtectRejectsInvalidPaths(t *testing.T) {
	requireWindowsACL(t)
	workspace := t.TempDir()
	file := filepath.Join(workspace, "file.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	cases := map[string]string{
		"missing":        filepath.Join(workspace, "missing"),
		"file not dir":   file,
		"workspace root": workspace,
	}
	for name, protected := range cases {
		plan := winACLPlan{
			Workspaces: []string{workspace},
			Protected:  []string{protected},
			Mode:       winModeWorkspaceWrite,
			Argv:       []string{winShellCommand(), "/c", "exit 0"},
		}
		if _, err := runWindowsACLPlanConfined(plan); err == nil {
			t.Fatalf("%s: runWindowsACLPlanConfined succeeded, want fail closed", name)
		}
	}
}

// TestWindowsRuntimeProtectOutsideWorkspaceIsSkipped 固定"授权面之外的保护路径
// 跳过而不是失败"：它本来就不能写，跳过不会放宽任何东西。
func TestWindowsRuntimeProtectOutsideWorkspaceIsSkipped(t *testing.T) {
	requireWindowsACL(t)
	workspace := t.TempDir()
	outside := t.TempDir()
	plan := winACLPlan{
		Workspaces: []string{workspace},
		Protected:  []string{outside},
		Mode:       winModeWorkspaceWrite,
		Argv:       []string{winShellCommand(), "/c", "echo ok> " + quoteWindowsPath(filepath.Join(workspace, "ok.txt"))},
	}
	code, err := runWindowsACLPlanConfined(plan)
	if err != nil {
		t.Fatalf("runWindowsACLPlanConfined: %v", err)
	}
	if code != 0 {
		t.Fatalf("child exit code = %d, want 0", code)
	}
}

// TestWindowsRuntimeReadOnlyRejectsInvalidProtect 固定 read-only 仍然校验 --protect
// （虽然不会下发、也不会产生 ACE）。
func TestWindowsRuntimeReadOnlyRejectsInvalidProtect(t *testing.T) {
	requireWindowsACL(t)
	workspace := t.TempDir()
	plan := winACLPlan{
		Workspaces: []string{workspace},
		Protected:  []string{filepath.Join(workspace, "missing")},
		Mode:       winModeReadOnly,
		Argv:       []string{winShellCommand(), "/c", "exit 0"},
	}
	if _, err := runWindowsACLPlanConfined(plan); err == nil {
		t.Fatal("read-only run with a missing --protect path succeeded, want fail closed")
	}
}

// TestWindowsRuntimeCachedStateSurvivesLastHelper 固定缓存语义的状态文件不变量：
// 最后一个 helper 退出后状态文件仍是"无 grant 的缓存记录"（描述符 + protect 账本），
// 目录对宿主可写。
func TestWindowsRuntimeCachedStateSurvivesLastHelper(t *testing.T) {
	requireWindowsACL(t)
	workspace := t.TempDir()
	gitDir := filepath.Join(workspace, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	plan := winACLPlan{
		Workspaces: []string{workspace},
		Protected:  []string{gitDir},
		Mode:       winModeWorkspaceWrite,
		Argv:       []string{winShellCommand(), "/c", "exit 0"},
	}
	if code, err := runWindowsACLPlanConfined(plan); err != nil {
		t.Fatalf("runWindowsACLPlanConfined: %v", err)
	} else if code != 0 {
		t.Fatalf("child exit code = %d, want 0", code)
	}
	pathHash := winACLPathHash(filepath.Clean(workspace))
	statePath := winACLStateFilePath(os.Getenv("LOCALAPPDATA"), os.Getenv("TEMP"), pathHash)
	if statePath == "" {
		t.Skip("no writable state directory on this host")
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read ACL state: %v", err)
	}
	state, err := parseWinACLState(data)
	if err != nil {
		t.Fatalf("parseWinACLState: %v", err)
	}
	if len(state.Grants) != 0 || state.DenyAdded || state.LabelApplied {
		t.Fatalf("state after the last helper = %+v, want a grant-free cache record", state)
	}
	if len(state.Protected) != 1 || !strings.EqualFold(state.Protected[0], gitDir) {
		t.Fatalf("protect ledger = %v, want [%q]", state.Protected, gitDir)
	}
	if err := os.WriteFile(filepath.Join(workspace, "host-write.txt"), []byte("ok"), 0o600); err != nil {
		t.Fatalf("workspace not writable for the host after the run: %v", err)
	}
}

// winTestRestrictedToken 构造一个只读 restricting 列表的受限令牌（Job/进程树用例
// 只需要"能跑一个系统命令"，不需要写授权）。
func winTestRestrictedToken(t *testing.T) *windowsToken {
	t.Helper()
	logon, err := winCurrentLogonSID()
	if err != nil {
		t.Fatalf("winCurrentLogonSID: %v", err)
	}
	restricting, err := windowsRestrictingSIDStrings(winModeReadOnly, logon, winEveryoneSIDString, nil, "")
	if err != nil {
		t.Fatalf("windowsRestrictingSIDStrings: %v", err)
	}
	token, err := winNewRestrictedToken(restricting)
	if err != nil {
		t.Fatalf("winNewRestrictedToken: %v", err)
	}
	return token
}

// TestWindowsRuntimeJobTerminatesDetachedDescendants 是"后代随 helper 一起死"的
// 真机回归（直接驱动原语）：`start /b` 让 ping 脱离顶层命令存活，顶层命令立即退出；
// winRunConfinedCommand 必须收掉这棵残留的树、报出可识别文案，并让 Job 变空。
//
//	RIG_WINDOWS_SANDBOX_TEST=1 go test ./sandbox/osrunner -run TestWindowsRuntimeJobTerminatesDetachedDescendants -v
func TestWindowsRuntimeJobTerminatesDetachedDescendants(t *testing.T) {
	if os.Getenv("RIG_WINDOWS_SANDBOX_TEST") != "1" {
		t.Skip("set RIG_WINDOWS_SANDBOX_TEST=1 to run the real Windows process-tree test")
	}
	requireWindowsACL(t)
	token := winTestRestrictedToken(t)
	defer token.close()
	job, err := winNewSandboxJob()
	if err != nil {
		t.Fatalf("winNewSandboxJob: %v", err)
	}
	defer job.close()
	// start /b + ping 30 秒：顶层命令（cmd /c start …）立刻返回，ping 成为脱离的后代。
	argv := []string{winShellCommand(), "/c", "start /b ping -n 30 127.0.0.1"}
	started := time.Now()
	outcome, err := winRunConfinedCommand(argv, token.handle, nil, job, 0)
	if err != nil {
		t.Fatalf("winRunConfinedCommand: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 20*time.Second {
		t.Fatalf("run took %s: the helper waited for the detached descendant instead of killing it", elapsed)
	}
	if outcome.ExitCode != 0 || outcome.TimedOut {
		t.Fatalf("outcome = %+v, want the top-level command's own exit code 0", outcome)
	}
	if outcome.Tree.Terminated < 1 {
		t.Fatalf("outcome.Tree = %+v, want at least one terminated descendant", outcome.Tree)
	}
	notice := winTreeTerminationNotice(outcome)
	if !strings.Contains(notice, winTreeTerminationNoticePrefix) || !strings.Contains(notice, "descendant process(es) killed") {
		t.Fatalf("notice = %q, want the recognizable tree-termination text", notice)
	}
	// 收树之后 Job 必须是空的：cleanup 恢复 per-run 形状时不能有任何后代还活着。
	if active, err := job.activeProcesses(); err != nil {
		t.Fatalf("activeProcesses: %v", err)
	} else if active != 0 {
		t.Fatalf("job still has %d active process(es) after the run", active)
	}
}

// TestWindowsRuntimePlanKillsDescendantsBeforeRestore 覆盖 helper 的**顺序**：顶层
// 命令退出后先收树、再 cleanup（恢复 Everyone 拒绝 / Low 标签、删私有 temp）。
//
// 断言：stderr 出现可识别的收树文案；工作区在 helper 退出后对宿主仍可写（cleanup
// 确实执行了）；整条命令没有等到后代自己结束（< 20s，而后代是 30s 的 ping）。
func TestWindowsRuntimePlanKillsDescendantsBeforeRestore(t *testing.T) {
	if os.Getenv("RIG_WINDOWS_SANDBOX_TEST") != "1" {
		t.Skip("set RIG_WINDOWS_SANDBOX_TEST=1 to run the real Windows process-tree test")
	}
	requireWindowsACL(t)
	workspace := t.TempDir()
	plan := winACLPlan{
		Workspaces: []string{workspace},
		Mode:       winModeWorkspaceWrite,
		Argv:       []string{winShellCommand(), "/c", "start /b ping -n 30 127.0.0.1"},
	}
	output, elapsed, code, err := winRunPlanCapturingStderr(plan)
	if err != nil {
		t.Fatalf("runWindowsACLPlanConfined: %v", err)
	}
	if code != 0 {
		t.Fatalf("child exit code = %d, want 0 (the detached descendant must not fail the launch); stderr: %s", code, output)
	}
	if elapsed > 20*time.Second {
		t.Fatalf("run took %s, want the descendant killed instead of awaited", elapsed)
	}
	if !strings.Contains(output, winTreeTerminationNoticePrefix) {
		t.Fatalf("stderr = %q, want the recognizable %q notice", output, winTreeTerminationNoticePrefix)
	}
	if !strings.Contains(output, "descendant process(es) killed") {
		t.Fatalf("stderr = %q, want the terminated-descendant count", output)
	}
	// cleanup 在收树之后执行：宿主（未受限令牌）仍然能写工作区。
	if err := os.WriteFile(filepath.Join(workspace, "after-descendants.txt"), []byte("ok"), 0o600); err != nil {
		t.Fatalf("workspace not restored after killing descendants: %v", err)
	}
	// 反向对照：没有后代的命令不产生任何收树文案（避免"总是打印"的假阳性）。
	quietPlan := winACLPlan{
		Workspaces: []string{workspace},
		Mode:       winModeWorkspaceWrite,
		Argv:       []string{winShellCommand(), "/c", "exit 0"},
	}
	quietOutput, _, _, err := winRunPlanCapturingStderr(quietPlan)
	if err != nil {
		t.Fatalf("quiet run: %v", err)
	}
	if strings.Contains(quietOutput, winTreeTerminationNoticePrefix) {
		t.Fatalf("stderr = %q, want no tree-termination notice for a command without descendants", quietOutput)
	}
}

// winRunPlanCapturingStderr 在进程内跑一次 plan 并把 helper 写到 os.Stderr 的内容
// 抓回来（helper 的失败/诊断契约都走 stderr）。
func winRunPlanCapturingStderr(plan winACLPlan) (string, time.Duration, int, error) {
	original := os.Stderr
	reader, writer, err := os.Pipe()
	if err != nil {
		return "", 0, 0, err
	}
	os.Stderr = writer
	started := time.Now()
	code, runErr := runWindowsACLPlanConfined(plan)
	elapsed := time.Since(started)
	os.Stderr = original
	_ = writer.Close()
	collected, readErr := io.ReadAll(reader)
	_ = reader.Close()
	if readErr != nil {
		return string(collected), elapsed, code, readErr
	}
	return string(collected), elapsed, code, runErr
}

// TestWindowsRuntimeTreeTerminationOutcome 固定"收树"的可辨认结果：顶层命令还在跑
// 时看门狗到点 → Job 终止整棵树 → outcome.TimedOut + 退出码 124 + 可识别文案，
// 绝不把"被沙箱终止"伪装成命令自己的退出码。
func TestWindowsRuntimeTreeTerminationOutcome(t *testing.T) {
	requireWindowsACL(t)
	token := winTestRestrictedToken(t)
	defer token.close()
	job, err := winNewSandboxJob()
	if err != nil {
		t.Fatalf("winNewSandboxJob: %v", err)
	}
	defer job.close()
	start := time.Now()
	outcome, err := winRunConfinedCommand(
		[]string{winShellCommand(), "/c", "ping -n 20 127.0.0.1 >NUL"},
		token.handle, nil, job, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("winRunConfinedCommand: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Fatalf("tree termination took %s, want the 500ms watchdog plus a bounded wait", elapsed)
	}
	if !outcome.TimedOut {
		t.Fatalf("outcome = %+v, want TimedOut", outcome)
	}
	if code := winOutcomeExitCode(outcome); code != winTreeTerminatedExitCode {
		t.Fatalf("exit code = %d, want %d (tree terminated by sandbox)", code, winTreeTerminatedExitCode)
	}
	notice := winTreeTerminationNotice(outcome)
	if !strings.Contains(notice, winTreeTerminationNoticePrefix) || !strings.Contains(notice, "timeout after") {
		t.Fatalf("notice = %q, want the recognizable tree-termination text", notice)
	}
	// 收树之后 Job 里不应再有活跃进程。
	if active, err := job.activeProcesses(); err != nil {
		t.Fatalf("activeProcesses: %v", err)
	} else if active != 0 {
		t.Fatalf("job still has %d active process(es) after termination", active)
	}
}

// TestWindowsRuntimeProbeUsesJob 固定"探测也走 Job 路径"：Job 不可用的宿主必须在
// New() 阶段就 fail closed（而不是每条命令随机失败）。这里只能验证探测本身仍然
// 成功、并且确实创建过 Job（无法在可用的宿主上模拟不可用）。
func TestWindowsRuntimeProbeUsesJob(t *testing.T) {
	requireWindowsACL(t)
	job, err := winNewSandboxJob()
	if err != nil {
		t.Fatalf("winNewSandboxJob: %v", err)
	}
	defer job.close()
	if active, err := job.activeProcesses(); err != nil {
		t.Fatalf("activeProcesses: %v", err)
	} else if active != 0 {
		t.Fatalf("a fresh job reports %d active process(es), want 0", active)
	}
	if notice := winTreeTerminationNotice(winProcessOutcome{}); notice != "" {
		t.Fatalf("notice for an idle job = %q, want empty", notice)
	}
}
