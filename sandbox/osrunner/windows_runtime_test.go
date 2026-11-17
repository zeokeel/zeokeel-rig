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
// requireWindowsACL 是所有**真机强制**用例的统一门控。
//
// 除了"后端可用"，还要求显式设置 RIG_WINDOWS_SANDBOX_TEST=1：这些用例会真实
// 改写工作区 ACL、把令牌降到 Low、并启动子进程，不适合在普通的跨平台测试步骤里
// 顺带执行（那会让"纯逻辑"步骤变慢、且在 CI 上互相干扰）。CI 的
// windows-sandbox job 专门用一个步骤打开它来跑这些用例。
func requireWindowsACL(t *testing.T) {
	t.Helper()
	if os.Getenv("RIG_WINDOWS_SANDBOX_TEST") != "1" {
		t.Skip("set RIG_WINDOWS_SANDBOX_TEST=1 to run the real Windows ACL enforcement tests")
	}
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
	// 有界轮询：最后一个 helper 退出与目录句柄释放之间可能有一瞬，不把调度抖动当回归。
	if waited := winWaitForHostWrite(t, filepath.Join(workspace, "after-concurrent.txt")); waited > 100*time.Millisecond {
		t.Logf("workspace writable again after %s", waited)
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
// quoteWindowsPath 只在**需要**时给路径加引号。
//
// 测试命令走 `cmd.exe /c <字符串>`，而 helper 用 syscall.EscapeArg 把 argv 拼成
// 命令行：如果这里无条件加引号，EscapeArg 会把它转义成 \"，cmd.exe 不认识这种
// 转义，直接报 "The filename, directory name, or volume label syntax is
// incorrect."（真机 CI 上就是这样失败的）。无空格的临时路径保持不加引号即可。
func quoteWindowsPath(path string) string {
	if strings.ContainsAny(path, " \t") {
		return `"` + path + `"`
	}
	return path
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
	if waited := winWaitForHostWrite(t, filepath.Join(gitDir, "after.txt")); waited > 100*time.Millisecond {
		t.Logf("protected directory writable again after %s", waited)
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
	if waited := winWaitForHostWrite(t, filepath.Join(workspace, "host-write.txt")); waited > 100*time.Millisecond {
		t.Logf("workspace writable again after %s", waited)
	}
}

// winPollUntil 在 timeout 内每 interval 轮询一次 ok，返回它第一次为真的耗时。
//
// 为什么必须有它：Windows 的进程终止/句柄回收是**异步**的——TerminateJobObject 只
// 是"请求"，Job 记账、进程表项回收、目录句柄关闭都需要一点时间。这些真机用例要
// 断言的是"最终会消失"/"最终可写"，不是"调用返回的同一纳秒就消失"。立刻比较会把
// 正常的调度抖动变成 CI 红灯，而真机 CI 的 flake 成本远高于多等几十毫秒。
//
// 超时时把最后一次错误与实际等待时长写进失败信息，便于区分"慢"与"根本没发生"。
func winPollUntil(t *testing.T, what string, timeout, interval time.Duration, ok func() error) time.Duration {
	t.Helper()
	started := time.Now()
	var lastErr error
	for {
		lastErr = ok()
		elapsed := time.Since(started)
		if lastErr == nil {
			return elapsed
		}
		if elapsed >= timeout {
			t.Fatalf("%s: still not satisfied after %s: %v", what, elapsed, lastErr)
		}
		time.Sleep(interval)
	}
}

// winWaitForEmptyJob 轮询到 Job 里没有活跃进程（有界），返回等待耗时。
func winWaitForEmptyJob(t *testing.T, job *winSandboxJob) time.Duration {
	t.Helper()
	return winPollUntil(t, "job still has active process(es)", 2*time.Second, 10*time.Millisecond, func() error {
		active, err := job.activeProcesses()
		if err != nil {
			return err
		}
		if active != 0 {
			return fmt.Errorf("%d active process(es) remain", active)
		}
		return nil
	})
}

// winWaitForHostWrite 轮询到宿主能写工作区（有界）：ACL 恢复是同步的，但如果刚被
// 终止的后代仍持有目录句柄，个别情况下首次打开会有极短的共享冲突。
func winWaitForHostWrite(t *testing.T, path string) time.Duration {
	t.Helper()
	return winPollUntil(t, "workspace not restored for the host", 2*time.Second, 10*time.Millisecond, func() error {
		return os.WriteFile(path, []byte("ok"), 0o600)
	})
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

// winDetachedDescendantArgv 返回一条"脱离的后代 + 父命令多活一会儿"的命令行。
//
// 为什么父命令要自己 ping 一下：`start /b` 启动后代后会立刻返回，如果父命令紧接着
// 退出，helper 检查 Job 的瞬间与"后代被创建/记账"之间只剩微秒级窗口——CI 上偶发
// 观察到 0 个活跃进程，于是收树文案缺失、用例误报失败。让父命令多跑约 1 秒，后代
// 就**必然**已在 Job 里，用例测的才是真正的语义（"顶层退出时后代仍在跑 → 先收树
// 再恢复 ACL"），而不是调度运气。
//
// 后代是 30 秒的 ping：如果 helper 等它自己结束，elapsed 会接近 30s，用例的
// "< 20s" 断言会失败——这条对照仍然有效。
func winDetachedDescendantArgv() []string {
	return []string{winShellCommand(), "/c", "start /b ping -n 30 127.0.0.1 >NUL & ping -n 2 127.0.0.1 >NUL"}
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
	// 脱离的后代（30s ping）+ 父命令自己多跑约 1 秒：保证 helper 收树时后代一定在。
	argv := winDetachedDescendantArgv()
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
	// 这里用有界轮询（进程终止/记账是异步的），并在失败信息里带上实际等待时长。
	if waited := winWaitForEmptyJob(t, job); waited > 100*time.Millisecond {
		t.Logf("job became empty after %s", waited)
	}
}

// TestWindowsRuntimePlanKillsDescendantsBeforeRestore 覆盖 helper 的**顺序**：顶层
// 命令退出后先收树、再 cleanup（恢复 Everyone 拒绝 / Low 标签、删私有 temp）。
//
// 断言（顺序语义用**结果**表达，不靠时序猜）：
//   - `outcome.Tree.Terminated >= 1`：确实杀掉了后代；
//   - `outcome.Tree.Remaining == 0`：在 cleanup 之前就**等到了**它消失
//     （terminateTree 的 waitForEmptyTree 是 cleanup 的前置条件）；
//   - stderr 出现可识别的收树文案（模型/宿主可见的诊断契约）；
//   - 工作区在 helper 退出后对宿主仍可写（cleanup 确实执行了；有界轮询）；
//   - 整条命令没有等到后代自己结束（< 20s，而后代是 30s 的 ping）。
//
// 反向对照：没有后代的命令既不产生收树文案，`Tree` 也必须是全零——避免"总是打印"
// 与"总是计数"的假阳性。
func TestWindowsRuntimePlanKillsDescendantsBeforeRestore(t *testing.T) {
	if os.Getenv("RIG_WINDOWS_SANDBOX_TEST") != "1" {
		t.Skip("set RIG_WINDOWS_SANDBOX_TEST=1 to run the real Windows process-tree test")
	}
	requireWindowsACL(t)
	workspace := t.TempDir()
	plan := winACLPlan{
		Workspaces: []string{workspace},
		Mode:       winModeWorkspaceWrite,
		Argv:       winDetachedDescendantArgv(),
	}
	outcome, output, elapsed, code, err := winRunPlanCapturingStderrOutcome(plan)
	if err != nil {
		t.Fatalf("runWindowsACLPlanConfined: %v (stderr: %s)", err, output)
	}
	if code != 0 {
		t.Fatalf("child exit code = %d, want 0 (the detached descendant must not fail the launch); stderr: %s", code, output)
	}
	if elapsed > 20*time.Second {
		t.Fatalf("run took %s, want the descendant killed instead of awaited (tree = %+v)", elapsed, outcome.Tree)
	}
	// 顺序语义的直接证据（不依赖时序）：helper 在 cleanup 之前既**杀掉**了后代，
	// 又**等到它消失**（Remaining == 0）。这两条由 winRunConfinedCommand 的
	// terminateTree + waitForEmptyTree 保证，cleanup 只在其后执行。
	if outcome.Tree.Terminated < 1 {
		t.Fatalf("outcome.Tree = %+v, want at least one terminated descendant; stderr: %s", outcome.Tree, output)
	}
	if outcome.Tree.Remaining != 0 {
		t.Fatalf("outcome.Tree = %+v, want 0 remaining before the ACL is restored; stderr: %s", outcome.Tree, output)
	}
	if !strings.Contains(output, winTreeTerminationNoticePrefix) {
		t.Fatalf("stderr = %q, want the recognizable %q notice (tree = %+v, elapsed %s)",
			output, winTreeTerminationNoticePrefix, outcome.Tree, elapsed)
	}
	if !strings.Contains(output, "descendant process(es) killed") {
		t.Fatalf("stderr = %q, want the terminated-descendant count (tree = %+v)", output, outcome.Tree)
	}
	// cleanup 在收树之后执行：宿主（未受限令牌）仍然能写工作区。有界轮询 + 记录耗时，
	// 避免"刚终止的后代还持有目录句柄"造成的偶发共享冲突变成 flake。
	if waited := winWaitForHostWrite(t, filepath.Join(workspace, "after-descendants.txt")); waited > 100*time.Millisecond {
		t.Logf("workspace writable again after %s", waited)
	}
	// 反向对照：没有后代的命令不产生任何收树文案（避免"总是打印"的假阳性）。
	quietPlan := winACLPlan{
		Workspaces: []string{workspace},
		Mode:       winModeWorkspaceWrite,
		Argv:       []string{winShellCommand(), "/c", "exit 0"},
	}
	quietOutcome, quietOutput, _, _, err := winRunPlanCapturingStderrOutcome(quietPlan)
	if err != nil {
		t.Fatalf("quiet run: %v", err)
	}
	if quietOutcome.Tree.Terminated != 0 || quietOutcome.Tree.Remaining != 0 {
		t.Fatalf("quiet run tree = %+v, want no descendants at all", quietOutcome.Tree)
	}
	if strings.Contains(quietOutput, winTreeTerminationNoticePrefix) {
		t.Fatalf("stderr = %q, want no tree-termination notice for a command without descendants", quietOutput)
	}
}

// winRunPlanCapturingStderrOutcome 在进程内跑一次 plan，把 helper 写到 os.Stderr
// 的内容抓回来（helper 的失败/诊断契约都走 stderr），并把进程树结果一起返回：顺序
// 断言要用 Terminated/Remaining，而不是靠 stderr 文案猜。
func winRunPlanCapturingStderrOutcome(plan winACLPlan) (winProcessOutcome, string, time.Duration, int, error) {
	original := os.Stderr
	reader, writer, err := os.Pipe()
	if err != nil {
		return winProcessOutcome{}, "", 0, 0, err
	}
	os.Stderr = writer
	started := time.Now()
	outcome, code, runErr := runWindowsACLPlanConfinedOutcome(plan)
	elapsed := time.Since(started)
	os.Stderr = original
	_ = writer.Close()
	collected, readErr := io.ReadAll(reader)
	_ = reader.Close()
	if readErr != nil {
		return outcome, string(collected), elapsed, code, readErr
	}
	return outcome, string(collected), elapsed, code, runErr
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
	// 收树之后 Job 里不应再有活跃进程（有界轮询：进程回收是异步的）。
	if waited := winWaitForEmptyJob(t, job); waited > 100*time.Millisecond {
		t.Logf("job became empty after %s", waited)
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
	if waited := winWaitForEmptyJob(t, job); waited > 100*time.Millisecond {
		t.Logf("fresh job reported empty after %s", waited)
	}
	if notice := winTreeTerminationNotice(winProcessOutcome{}); notice != "" {
		t.Fatalf("notice for an idle job = %q, want empty", notice)
	}
}
