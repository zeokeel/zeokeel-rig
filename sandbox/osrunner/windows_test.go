package osrunner

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// 本文件只测试 Windows ACL runner 的纯计划逻辑：helper argv 解析、能力 SID
// 生成、模式到授权形状的映射、受限令牌 restricting 列表与 argv 前缀拼装。
// 它不带 build tag，因此在本 Linux 宿主上也会运行；真正的 Win32 令牌/ACL/spawn
// 测试在 windows_runtime_test.go（//go:build windows）。

// windowsCapabilitySIDPattern 是生成的能力 SID 的期望形状：一个拥有四个随机
// 子权限的 S-1-5-21 域 SID。
var windowsCapabilitySIDPattern = regexp.MustCompile(`^S-1-5-21-([1-9][0-9]{0,9}-){3}[1-9][0-9]{0,9}$`)

// windowsPolicy 构造一个带根与模式的策略。
// canonicalForTest 返回路径的系统规范形式（EvalSymlinks + Clean）。
//
// Windows 上 t.TempDir() 常返回 8.3 短名（C:\Users\RUNNER~1\...），而策略在
// 构造期会把它解析成长名（...\runneradmin\...）。断言必须用同一套规范化后的
// 期望值，否则在 CI 的真机上必然失败而本地 Linux 看不出来。
func canonicalForTest(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(real)
	}
	return filepath.Clean(path)
}

func windowsPolicy(t *testing.T, mode sandbox.Mode, roots ...string) *sandbox.Policy {
	t.Helper()
	return sandbox.New(sandbox.WithRoots(roots...), sandbox.WithMode(mode))
}

func TestWindowsParseHelperArgsWorkspaceWrite(t *testing.T) {
	plan, err := parseWinACLArgs([]string{
		"--workspace", `C:\work\a`,
		"--workspace", `C:\work\b`,
		"--mode", winModeWorkspaceWrite,
		"--", `C:\Windows\System32\cmd.exe`, "/c", "echo", "hi",
	})
	if err != nil {
		t.Fatalf("parseWinACLArgs: %v", err)
	}
	if got, want := strings.Join(plan.Workspaces, ","), `C:\work\a,C:\work\b`; got != want {
		t.Fatalf("Workspaces = %q, want %q", got, want)
	}
	if plan.Mode != winModeWorkspaceWrite {
		t.Fatalf("Mode = %q, want %q", plan.Mode, winModeWorkspaceWrite)
	}
	if plan.Probe {
		t.Fatal("Probe = true, want false")
	}
	if got, want := strings.Join(plan.Argv, "|"), `C:\Windows\System32\cmd.exe|/c|echo|hi`; got != want {
		t.Fatalf("Argv = %q, want %q", got, want)
	}
}

func TestWindowsParseHelperArgsReadOnly(t *testing.T) {
	plan, err := parseWinACLArgs([]string{
		"--workspace", `C:\work`,
		"--mode", winModeReadOnly,
		"--", "pwsh", "-NoProfile", "-Command", "Get-Location",
	})
	if err != nil {
		t.Fatalf("parseWinACLArgs: %v", err)
	}
	if plan.Mode != winModeReadOnly {
		t.Fatalf("Mode = %q, want %q", plan.Mode, winModeReadOnly)
	}
	// 只读模式下 --workspace 只用于校验：它必须被解析并保留。
	if len(plan.Workspaces) != 1 || plan.Workspaces[0] != `C:\work` {
		t.Fatalf("Workspaces = %v, want [C:\\work]", plan.Workspaces)
	}
}

func TestWindowsParseHelperArgsProtect(t *testing.T) {
	plan, err := parseWinACLArgs([]string{
		"--workspace", `C:\work`,
		"--protect", `C:\work\.git`,
		"--protect", `C:\work\secrets`,
		"--mode", winModeWorkspaceWrite,
		"--", "cmd.exe",
	})
	if err != nil {
		t.Fatalf("parseWinACLArgs: %v", err)
	}
	if got, want := strings.Join(plan.Protected, ","), `C:\work\.git,C:\work\secrets`; got != want {
		t.Fatalf("Protected = %q, want %q", got, want)
	}
	if len(plan.Workspaces) != 1 || plan.Workspaces[0] != `C:\work` {
		t.Fatalf("Workspaces = %v, want [C:\\work]", plan.Workspaces)
	}
	if got, want := strings.Join(plan.Argv, "|"), "cmd.exe"; got != want {
		t.Fatalf("Argv = %q, want %q", got, want)
	}

	// --protect 缺少值、空值、以及探测混入保护路径都必须 fail closed。
	for name, args := range map[string][]string{
		"missing value":   {"--workspace", `C:\work`, "--protect", "--mode", winModeWorkspaceWrite, "--", "cmd"},
		"empty value":     {"--workspace", `C:\work`, "--protect", "", "--mode", winModeWorkspaceWrite, "--", "cmd"},
		"probe protected": {"--probe", "--protect", `C:\work\.git`},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseWinACLArgs(args); err == nil {
				t.Fatalf("parseWinACLArgs(%q) succeeded, want fail closed", args)
			}
		})
	}
}

func TestWindowsConfinedCommandPassesProtectedPaths(t *testing.T) {
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", gitDir, err)
	}
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithProtectedPaths(filepath.Join(root, ".git")),
	)
	confined, err := buildWindowsConfinedCommand(`rig.exe`, []string{"cmd.exe"}, policy)
	if err != nil {
		t.Fatalf("buildWindowsConfinedCommand: %v", err)
	}
	protects := windowsFlagValues(confined.Argv, winFlagProtect)
	if want := canonicalForTest(gitDir); len(protects) != 1 || !strings.EqualFold(protects[0], want) {
		t.Fatalf("--protect values = %v, want [%q]", protects, want)
	}
	// 保护路径已被强制，但读/网络仍不受限，因此覆盖程度只能是 partial。
	if confined.Enforcement != sandbox.EnforcementPartial {
		t.Fatalf("Enforcement = %q, want partial", confined.Enforcement)
	}

	// read-only 没有可保护的写面：不下发 --protect。
	readOnly, err := buildWindowsConfinedCommand(`rig.exe`, []string{"cmd.exe"},
		sandbox.New(sandbox.WithRoots(root), sandbox.WithMode(sandbox.ModeReadOnly), sandbox.WithProtectedPaths(filepath.Join(root, ".git"))))
	if err != nil {
		t.Fatalf("buildWindowsConfinedCommand(read-only): %v", err)
	}
	if got := windowsFlagValues(readOnly.Argv, winFlagProtect); len(got) != 0 {
		t.Fatalf("read-only --protect values = %v, want none", got)
	}
}

// windowsFlagValues 提取 argv 里所有 "--flag value" 形式的 value（测试辅助）。
func windowsFlagValues(argv []string, flag string) []string {
	var values []string
	for index := 0; index+1 < len(argv); index++ {
		if argv[index] == flag {
			values = append(values, argv[index+1])
			index++
		}
	}
	return values
}

func TestWindowsParseHelperArgsProbe(t *testing.T) {
	plan, err := parseWinACLArgs([]string{winCommandProbe})
	if err != nil {
		t.Fatalf("parseWinACLArgs(probe): %v", err)
	}
	if !plan.Probe {
		t.Fatal("Probe = false, want true")
	}
	if len(plan.Workspaces) != 0 || plan.Mode != "" || len(plan.Argv) != 0 {
		t.Fatalf("probe plan carries run fields: %+v", plan)
	}
}

func TestWindowsParseHelperArgsFailClosed(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{name: "empty"},
		{name: "unknown flag", args: []string{"--frobnicate"}},
		{name: "missing flag value", args: []string{"--workspace"}},
		{name: "missing mode", args: []string{"--workspace", `C:\w`, "--", "cmd"}},
		{name: "missing workspace", args: []string{"--mode", winModeReadOnly, "--", "cmd"}},
		{name: "unknown mode", args: []string{"--workspace", `C:\w`, "--mode", "danger", "--", "cmd"}},
		{name: "duplicate mode", args: []string{"--workspace", `C:\w`, "--mode", winModeReadOnly, "--mode", winModeReadOnly, "--", "cmd"}},
		{name: "missing separator", args: []string{"--workspace", `C:\w`, "--mode", winModeReadOnly, "cmd"}},
		{name: "missing command", args: []string{"--workspace", `C:\w`, "--mode", winModeReadOnly, "--"}},
		{name: "empty workspace value", args: []string{"--workspace", "", "--mode", winModeReadOnly, "--", "cmd"}},
		{name: "probe with run args", args: []string{"--probe", "--workspace", `C:\w`, "--mode", winModeReadOnly, "--", "cmd"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := parseWinACLArgs(testCase.args); err == nil {
				t.Fatalf("parseWinACLArgs(%q) succeeded, want fail closed", testCase.args)
			}
		})
	}
}

func TestWindowsCapabilitySIDFormat(t *testing.T) {
	sid, err := newCapabilitySID()
	if err != nil {
		t.Fatalf("newCapabilitySID: %v", err)
	}
	if !windowsCapabilitySIDPattern.MatchString(sid) {
		t.Fatalf("newCapabilitySID = %q, want an S-1-5-21-<rand>-<rand>-<rand>-<rand> SID", sid)
	}
	other, err := newCapabilitySID()
	if err != nil {
		t.Fatalf("newCapabilitySID: %v", err)
	}
	if sid == other {
		t.Fatalf("two generated capability SIDs are equal: %q", sid)
	}
}

func TestWindowsCapabilitySIDFromReader(t *testing.T) {
	// 全零字节仍然产生合法的非零子权限（0 不是有意义的 SID 子权限）。
	sid, err := capabilitySIDFromReader(strings.NewReader(strings.Repeat("\x00", 16)))
	if err != nil {
		t.Fatalf("capabilitySIDFromReader: %v", err)
	}
	if !windowsCapabilitySIDPattern.MatchString(sid) {
		t.Fatalf("capabilitySIDFromReader = %q, want a valid capability SID", sid)
	}
	if got, want := sid, "S-1-5-21-1-1-1-1"; got != want {
		t.Fatalf("capabilitySIDFromReader(zeros) = %q, want %q", got, want)
	}
	if _, err := capabilitySIDFromReader(io.LimitReader(strings.NewReader("short"), 4)); err == nil {
		t.Fatal("capabilitySIDFromReader(short reader) succeeded, want an error")
	}
}

func TestWindowsGrantShape(t *testing.T) {
	cases := []struct {
		mode    string
		want    winGrantShape
		wantErr bool
	}{
		{mode: winModeReadOnly, want: winGrantShape{WorkspaceWrite: false, TempWrite: false, LowIntegrityToken: true}},
		{mode: winModeWorkspaceWrite, want: winGrantShape{WorkspaceWrite: true, TempWrite: true, LowIntegrityToken: true}},
		{mode: "danger-full-access", wantErr: true},
		{mode: "", wantErr: true},
	}
	for _, testCase := range cases {
		got, err := windowsGrantShape(testCase.mode)
		if testCase.wantErr {
			if err == nil {
				t.Fatalf("windowsGrantShape(%q) succeeded, want an error", testCase.mode)
			}
			continue
		}
		if err != nil {
			t.Fatalf("windowsGrantShape(%q): %v", testCase.mode, err)
		}
		if got != testCase.want {
			t.Fatalf("windowsGrantShape(%q) = %+v, want %+v", testCase.mode, got, testCase.want)
		}
	}
}

func TestWindowsRestrictingSIDStrings(t *testing.T) {
	logon, everyone := "S-1-5-5-0-1-2", "S-1-1-0"
	workspaceSIDs := []string{"S-1-5-21-1-2-3-4", "S-1-5-21-9-9-9-9"}
	tempSID := "S-1-5-21-5-6-7-8"
	readOnly, err := windowsRestrictingSIDStrings(winModeReadOnly, logon, everyone, workspaceSIDs, tempSID)
	if err != nil {
		t.Fatalf("windowsRestrictingSIDStrings(read-only): %v", err)
	}
	if got, want := strings.Join(readOnly, ","), logon+","+everyone; got != want {
		t.Fatalf("read-only restricting SIDs = %q, want %q", got, want)
	}

	// 每个工作区各有一个确定性能力 SID，都必须进入 restricting 列表。
	workspaceWrite, err := windowsRestrictingSIDStrings(winModeWorkspaceWrite, logon, everyone, workspaceSIDs, tempSID)
	if err != nil {
		t.Fatalf("windowsRestrictingSIDStrings(workspace-write): %v", err)
	}
	want := strings.Join([]string{logon, everyone, workspaceSIDs[0], workspaceSIDs[1], tempSID}, ",")
	if got := strings.Join(workspaceWrite, ","); got != want {
		t.Fatalf("workspace-write restricting SIDs = %q, want %q", got, want)
	}

	// 重复项（大小写不同）只保留一份。
	deduped, err := windowsRestrictingSIDStrings(winModeWorkspaceWrite, logon, everyone,
		[]string{workspaceSIDs[0], strings.ToLower(workspaceSIDs[0])}, tempSID)
	if err != nil {
		t.Fatalf("windowsRestrictingSIDStrings(dedup): %v", err)
	}
	if got, want := strings.Join(deduped, ","), strings.Join([]string{logon, everyone, workspaceSIDs[0], tempSID}, ","); got != want {
		t.Fatalf("deduped restricting SIDs = %q, want %q", got, want)
	}

	// 缺工作区 SID / 缺临时 SID / 未知模式 / 缺空列表都必须 fail closed。
	for name, call := range map[string]func() ([]string, error){
		"no workspace SID": func() ([]string, error) {
			return windowsRestrictingSIDStrings(winModeWorkspaceWrite, logon, everyone, nil, tempSID)
		},
		"empty workspace SID": func() ([]string, error) {
			return windowsRestrictingSIDStrings(winModeWorkspaceWrite, logon, everyone, []string{""}, tempSID)
		},
		"no temp SID": func() ([]string, error) {
			return windowsRestrictingSIDStrings(winModeWorkspaceWrite, logon, everyone, workspaceSIDs, "")
		},
		"no logon SID": func() ([]string, error) {
			return windowsRestrictingSIDStrings(winModeReadOnly, "", everyone, nil, "")
		},
		"unknown mode": func() ([]string, error) {
			return windowsRestrictingSIDStrings("danger", logon, everyone, workspaceSIDs, tempSID)
		},
	} {
		if _, err := call(); err == nil {
			t.Fatalf("%s: want fail closed, got nil error", name)
		}
	}
}

func TestWindowsConfinedCommandWorkspaceWrite(t *testing.T) {
	root := t.TempDir()
	policy := windowsPolicy(t, sandbox.ModeWorkspaceWrite, root)
	confined, err := buildWindowsConfinedCommand(`C:\host\rig.exe`, []string{"cmd.exe", "/c", "echo hi"}, policy)
	if err != nil {
		t.Fatalf("buildWindowsConfinedCommand: %v", err)
	}
	wantPrefix := []string{`C:\host\rig.exe`, HelperArgWindowsACL, "--workspace", canonicalForTest(root), "--mode", winModeWorkspaceWrite, "--"}
	if got, want := strings.Join(confined.Argv[:len(wantPrefix)], "\x00"), strings.Join(wantPrefix, "\x00"); got != want {
		t.Fatalf("Argv prefix = %q, want %q", confined.Argv[:len(wantPrefix)], wantPrefix)
	}
	if got, want := strings.Join(confined.Argv[len(wantPrefix):], "|"), "cmd.exe|/c|echo hi"; got != want {
		t.Fatalf("wrapped argv = %q, want %q", got, want)
	}
	if confined.Enforcement != sandbox.EnforcementPartial {
		t.Fatalf("Enforcement = %q, want partial", confined.Enforcement)
	}
	if got, want := strings.Join(confined.DenialSignatures, "|"), strings.Join(DenialSignatures(RunnerWindowsACL), "|"); got != want {
		t.Fatalf("DenialSignatures = %q, want %q", got, want)
	}
	if len(confined.DenialSignatures) == 0 {
		t.Fatal("DenialSignatures is empty")
	}
}

func TestWindowsConfinedCommandReadOnly(t *testing.T) {
	root := t.TempDir()
	policy := windowsPolicy(t, sandbox.ModeReadOnly, root)
	confined, err := buildWindowsConfinedCommand(`C:\host\rig.exe`, []string{"cmd.exe"}, policy)
	if err != nil {
		t.Fatalf("buildWindowsConfinedCommand: %v", err)
	}
	if !contains(confined.Argv, winModeReadOnly) {
		t.Fatalf("Argv = %v, want --mode read-only", confined.Argv)
	}
	if confined.Enforcement != sandbox.EnforcementPartial {
		t.Fatalf("Enforcement = %q, want partial", confined.Enforcement)
	}
}

func TestWindowsConfinedCommandMultipleRoots(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	policy := windowsPolicy(t, sandbox.ModeWorkspaceWrite, first, second)
	confined, err := buildWindowsConfinedCommand(`rig.exe`, []string{"cmd.exe"}, policy)
	if err != nil {
		t.Fatalf("buildWindowsConfinedCommand: %v", err)
	}
	workspaces := windowsWorkspaceFlags(confined.Argv)
	if got, want := strings.Join(workspaces, ","), strings.Join([]string{canonicalForTest(first), canonicalForTest(second)}, ","); got != want {
		t.Fatalf("workspace flags = %q, want %q", got, want)
	}
}

func TestWindowsConfinedCommandDangerFullAccessPassesThrough(t *testing.T) {
	policy := sandbox.New(sandbox.WithMode(sandbox.ModeDangerFullAccess))
	confined, err := buildWindowsConfinedCommand(`C:\host\rig.exe`, []string{"cmd.exe", "/c", "echo hi"}, policy)
	if err != nil {
		t.Fatalf("buildWindowsConfinedCommand: %v", err)
	}
	if got, want := strings.Join(confined.Argv, "|"), "cmd.exe|/c|echo hi"; got != want {
		t.Fatalf("Argv = %q, want %q", got, want)
	}
	// 透传没有被任何 runner 包装：必须是 EnforcementNone，而不是 Full。
	if confined.Enforcement != sandbox.EnforcementNone {
		t.Fatalf("Enforcement = %q, want %q", confined.Enforcement, sandbox.EnforcementNone)
	}
}

func TestWindowsConfinedCommandFailsClosed(t *testing.T) {
	if _, err := buildWindowsConfinedCommand(`rig.exe`, []string{"cmd.exe"}, nil); err == nil {
		t.Fatal("buildWindowsConfinedCommand(nil policy) succeeded, want fail closed")
	} else if !errors.Is(err, sandbox.ErrSandboxUnavailable) {
		t.Fatalf("nil policy error = %v, want ErrSandboxUnavailable", err)
	}

	noRoots := windowsPolicy(t, sandbox.ModeWorkspaceWrite)
	if _, err := buildWindowsConfinedCommand(`rig.exe`, []string{"cmd.exe"}, noRoots); err == nil {
		t.Fatal("buildWindowsConfinedCommand(no roots) succeeded, want fail closed")
	} else if !errors.Is(err, sandbox.ErrSandboxUnavailable) {
		t.Fatalf("no-roots error = %v, want ErrSandboxUnavailable", err)
	}

	if _, err := buildWindowsConfinedCommand(`rig.exe`, nil, windowsPolicy(t, sandbox.ModeReadOnly, t.TempDir())); err == nil {
		t.Fatal("buildWindowsConfinedCommand(empty argv) succeeded, want fail closed")
	}

	if _, err := windowsConfinedCommand(`rig.exe`, []string{"cmd.exe"}, []string{t.TempDir()}, nil, "bogus-mode"); err == nil {
		t.Fatal("windowsConfinedCommand(unknown mode) succeeded, want fail closed")
	} else if !errors.Is(err, sandbox.ErrSandboxUnavailable) {
		t.Fatalf("unknown mode error = %v, want ErrSandboxUnavailable", err)
	}
}

func TestWindowsTempRootUsable(t *testing.T) {
	cases := []struct {
		name      string
		workspace []string
		temp      string
		want      bool
	}{
		{name: "disjoint", workspace: []string{filepath.FromSlash("/work/src")}, temp: filepath.FromSlash("/tmp"), want: true},
		{name: "temp inside workspace", workspace: []string{filepath.FromSlash("/work")}, temp: filepath.FromSlash("/work/tmp"), want: false},
		{name: "temp equals workspace", workspace: []string{filepath.FromSlash("/work")}, temp: filepath.FromSlash("/work"), want: false},
		{name: "prefix is not containment", workspace: []string{filepath.FromSlash("/work")}, temp: filepath.FromSlash("/workshop/tmp"), want: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := windowsTempRootUsable(testCase.workspace, testCase.temp); got != testCase.want {
				t.Fatalf("windowsTempRootUsable(%v, %q) = %v, want %v", testCase.workspace, testCase.temp, got, testCase.want)
			}
		})
	}
}

func TestWindowsPathWithin(t *testing.T) {
	cases := []struct {
		name      string
		root      string
		candidate string
		want      bool
	}{
		{name: "same", root: filepath.FromSlash("/work"), candidate: filepath.FromSlash("/work"), want: true},
		{name: "child", root: filepath.FromSlash("/work"), candidate: filepath.FromSlash("/work/a/b"), want: true},
		{name: "prefix only", root: filepath.FromSlash("/work"), candidate: filepath.FromSlash("/workshop"), want: false},
		{name: "parent", root: filepath.FromSlash("/work"), candidate: filepath.FromSlash("/"), want: false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := windowsPathWithin(testCase.root, testCase.candidate); got != testCase.want {
				t.Fatalf("windowsPathWithin(%q, %q) = %v, want %v", testCase.root, testCase.candidate, got, testCase.want)
			}
		})
	}
}

// windowsWorkspaceFlags 提取 argv 里所有 --workspace 的值（测试辅助）。
func windowsWorkspaceFlags(argv []string) []string {
	var out []string
	for index := 0; index+1 < len(argv); index++ {
		if argv[index] == winFlagWorkspace {
			out = append(out, argv[index+1])
		}
	}
	return out
}

// normWinPath 让 Windows 路径断言在 Linux 测试宿主上也能跨分隔符比较。
func normWinPath(value string) string {
	value = strings.ReplaceAll(filepath.Clean(value), "\\", "/")
	return strings.ToLower(strings.TrimRight(value, "/"))
}

// TestWinTempBaseCandidatesOrderAndDedup 固定候选优先级：显式覆盖 > Windows
// 自带 temp（extra）> os.TempDir；大小写不同的同一目录只出现一次。
func TestWinTempBaseCandidatesOrderAndDedup(t *testing.T) {
	env := map[string]string{
		"RIG_SANDBOX_TEMP": `D:\SandboxTemp`,
		"SystemRoot":       `C:\WINDOWS`,
	}
	getenv := func(key string) string { return env[key] }
	got := winTempBaseCandidates(getenv, `C:\WINDOWS`,
		`C:\Users\Lost\AppData\Local\Temp`,
		`c:\users\lost\appdata\local\temp`)
	if len(got) != 2 {
		t.Fatalf("candidates = %v, want 2 (explicit + Windows temp; system root filtered)", got)
	}
	if normWinPath(got[0]) != normWinPath(`D:\SandboxTemp`) {
		t.Fatalf("candidate[0] = %q, want the explicit RIG_SANDBOX_TEMP", got[0])
	}
	if normWinPath(got[1]) != normWinPath(`C:\Users\Lost\AppData\Local\Temp`) {
		t.Fatalf("candidate[1] = %q, want the Windows temp directory", got[1])
	}
}

// TestWinTempBaseCandidatesSkipsSystemRoot 覆盖报告里的根因：GetTempPath 在
// 环境被裁剪时会回退到 C:\WINDOWS，绝不能把它当临时基址。
func TestWinTempBaseCandidatesSkipsSystemRoot(t *testing.T) {
	env := map[string]string{"SystemRoot": `C:\WINDOWS`}
	getenv := func(key string) string { return env[key] }
	got := winTempBaseCandidates(getenv, `C:\WINDOWS`, `C:\Users\Lost\AppData\Local\Temp`)
	if len(got) != 1 {
		t.Fatalf("candidates = %v, want only the Windows temp directory", got)
	}
	for _, candidate := range got {
		if winPathWithinFold(`C:\WINDOWS`, candidate) {
			t.Fatalf("candidate %q is under SystemRoot; must never be used as temp base", candidate)
		}
	}
	if normWinPath(got[0]) != normWinPath(`C:\Users\Lost\AppData\Local\Temp`) {
		t.Fatalf("candidate[0] = %q, want the Windows user temp", got[0])
	}
}

// TestWinTempBaseCandidatesUsesWindowsTempWithEmptyEnv covers the fully
// stripped helper environment: the Windows-provided extra must still be the
// candidate, never an empty list.
func TestWinTempBaseCandidatesUsesWindowsTempWithEmptyEnv(t *testing.T) {
	got := winTempBaseCandidates(func(string) string { return "" }, "", `C:\Users\Lost\AppData\Local\Temp`)
	if len(got) != 1 || normWinPath(got[0]) != normWinPath(`C:\Users\Lost\AppData\Local\Temp`) {
		t.Fatalf("candidates = %v, want the Windows temp fallback", got)
	}
}

// TestWinPathWithinFold 固定大小写不敏感与双分隔符的包含判断。
func TestWinPathWithinFold(t *testing.T) {
	cases := []struct {
		root, candidate string
		want            bool
	}{
		{`C:\WINDOWS`, `C:\WINDOWS`, true},
		{`C:\WINDOWS`, `c:\windows\Temp`, true},
		{`C:\WINDOWS`, `C:/Windows/Temp`, true},
		{`C:\WINDOWS`, `C:\WindowsApps`, false},
		{`C:\Windows`, `C:\Users\Lost\AppData\Local\Temp`, false},
		{``, `C:\Users`, false},
	}
	for _, tc := range cases {
		if got := winPathWithinFold(tc.root, tc.candidate); got != tc.want {
			t.Errorf("winPathWithinFold(%q, %q) = %v, want %v", tc.root, tc.candidate, got, tc.want)
		}
	}
}

// TestWindowsWorkspaceCapabilitySIDIsDeterministic 固定 B2 的前提：工作区能力
// SID 由规范化路径派生，同一目录（含大小写/分隔符不同写法）跨进程/跨会话必须
// 得到同一个 SID，不同目录必须不同。
func TestWindowsWorkspaceCapabilitySIDIsDeterministic(t *testing.T) {
	root := filepath.FromSlash(`/work/project`)
	sid, err := winWorkspaceCapabilitySID(root)
	if err != nil {
		t.Fatalf("winWorkspaceCapabilitySID: %v", err)
	}
	if !windowsCapabilitySIDPattern.MatchString(sid) {
		t.Fatalf("capability SID = %q, want S-1-5-21-<a>-<b>-<c>-<d>", sid)
	}
	again, err := winWorkspaceCapabilitySID(root)
	if err != nil {
		t.Fatalf("winWorkspaceCapabilitySID(again): %v", err)
	}
	if again != sid {
		t.Fatalf("capability SID is not stable: %q != %q", again, sid)
	}
	// 大小写与分隔符不同、尾部斜杠：都是同一个目录。
	for _, variant := range []string{
		strings.ToUpper(root),
		strings.ReplaceAll(root, "/", `\`),
		root + "/",
		"  " + root + "  ",
	} {
		other, err := winWorkspaceCapabilitySID(variant)
		if err != nil {
			t.Fatalf("winWorkspaceCapabilitySID(%q): %v", variant, err)
		}
		if other != sid {
			t.Fatalf("capability SID for %q = %q, want %q", variant, other, sid)
		}
	}
	other, err := winWorkspaceCapabilitySID(filepath.FromSlash(`/work/other`))
	if err != nil {
		t.Fatalf("winWorkspaceCapabilitySID(other): %v", err)
	}
	if other == sid {
		t.Fatal("different directories share a capability SID")
	}
	if _, err := winWorkspaceCapabilitySID("   "); err == nil {
		t.Fatal("winWorkspaceCapabilitySID(empty) = nil error, want fail closed")
	}
}

// TestWinPathKey 固定比较键：大小写折叠、分隔符统一、去尾部斜杠、保留根。
func TestWinPathKey(t *testing.T) {
	cases := []struct {
		left, right string
		equal       bool
	}{
		{`C:\Work\Project`, `c:\work\project`, true},
		{`C:\Work\Project`, `C:/Work/Project`, true},
		{`C:\Work\Project\`, `C:\Work\Project`, true},
		{`C:\Work`, `C:\Workshop`, false},
		{`C:\Work\A`, `C:\Work\B`, false},
		{``, ``, true},
	}
	for _, testCase := range cases {
		got := winPathKey(testCase.left) == winPathKey(testCase.right)
		if got != testCase.equal {
			t.Fatalf("winPathKey(%q) == winPathKey(%q) = %t, want %t", testCase.left, testCase.right, got, testCase.equal)
		}
	}
	if key := winPathKey(`C:\`); key == "" || key == "c:" {
		t.Fatalf("winPathKey(%q) = %q, want the drive root preserved", `C:\`, key)
	}
	if winPathKey("") != "" {
		t.Fatal("winPathKey(\"\") = non-empty")
	}
}

// TestWinResolveProtectedPaths 固定 --protect 的校验/规范化契约（fail closed 优先）。
func TestWinResolveProtectedPaths(t *testing.T) {
	workspace := t.TempDir()
	gitDir := filepath.Join(workspace, ".git")
	secrets := filepath.Join(workspace, "secrets")
	for _, dir := range []string{gitDir, secrets} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %q: %v", dir, err)
		}
	}
	outside := t.TempDir()

	resolution, err := winResolveProtectedPaths(
		[]string{" " + gitDir + " ", gitDir + string(filepath.Separator), strings.ToUpper(gitDir), secrets},
		[]string{workspace},
	)
	if err != nil {
		t.Fatalf("winResolveProtectedPaths: %v", err)
	}
	// 大小写/尾部斜杠/重复项只保留一份；顺序保持首次出现的次序。
	if len(resolution.Enforced) != 2 {
		t.Fatalf("enforced = %+v, want two deduped targets", resolution.Enforced)
	}
	if resolution.Enforced[0].Path != gitDir || resolution.Enforced[1].Path != secrets {
		t.Fatalf("enforced paths = %q/%q, want %q/%q",
			resolution.Enforced[0].Path, resolution.Enforced[1].Path, gitDir, secrets)
	}
	if got := resolution.For(workspace); len(got) != 2 {
		t.Fatalf("For(workspace) = %v, want both paths", got)
	}
	if got := resolution.For(t.TempDir()); len(got) != 0 {
		t.Fatalf("For(other workspace) = %v, want none", got)
	}

	// 落在授权面之外的 protect 路径：跳过 + 一行诊断，不是错误。
	outsideResolution, err := winResolveProtectedPaths([]string{outside}, []string{workspace})
	if err != nil {
		t.Fatalf("winResolveProtectedPaths(outside): %v", err)
	}
	if len(outsideResolution.Enforced) != 0 {
		t.Fatalf("enforced = %+v, want none", outsideResolution.Enforced)
	}
	if len(outsideResolution.Notes) != 1 || !strings.Contains(outsideResolution.Notes[0], "outside every granted workspace") {
		t.Fatalf("notes = %v, want one diagnostic line", outsideResolution.Notes)
	}

	// 不存在的路径 fail closed（静默忽略会让调用方以为受保护）。
	if _, err := winResolveProtectedPaths([]string{filepath.Join(workspace, "missing")}, []string{workspace}); err == nil {
		t.Fatal("winResolveProtectedPaths(missing) = nil error, want fail closed")
	}
	// 不是目录的路径 fail closed。
	file := filepath.Join(workspace, "file.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write %q: %v", file, err)
	}
	if _, err := winResolveProtectedPaths([]string{file}, []string{workspace}); err == nil {
		t.Fatal("winResolveProtectedPaths(file) = nil error, want fail closed")
	}
	// 相对路径 fail closed（语义取决于 helper 的 CWD）。
	if _, err := winResolveProtectedPaths([]string{".git"}, []string{workspace}); err == nil {
		t.Fatal("winResolveProtectedPaths(relative) = nil error, want fail closed")
	}
	// 保护整个工作区根是自相矛盾的（既授权写整棵树、又整棵树只读）。
	if _, err := winResolveProtectedPaths([]string{workspace}, []string{workspace}); err == nil {
		t.Fatal("winResolveProtectedPaths(workspace root) = nil error, want fail closed")
	}
	// 空列表是合法的：没有保护路径就是没有。
	empty, err := winResolveProtectedPaths(nil, []string{workspace})
	if err != nil {
		t.Fatalf("winResolveProtectedPaths(nil): %v", err)
	}
	if len(empty.Enforced) != 0 || len(empty.Notes) != 0 {
		t.Fatalf("empty resolution = %+v", empty)
	}
}

// TestWinResolveProtectedPathsNestedWorkspace 固定"归属最具体工作区"的选择：
// 嵌套工作区时 deny ACE 跟着更精确的那把钥匙走（两者都在 restricting 列表里，
// 因此安全性不受影响，但这让 ACE 能跨运行复用）。
func TestWinResolveProtectedPathsNestedWorkspace(t *testing.T) {
	outer := t.TempDir()
	inner := filepath.Join(outer, "inner")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	protected := filepath.Join(inner, ".git")
	if err := os.MkdirAll(protected, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	resolution, err := winResolveProtectedPaths([]string{protected}, []string{outer, inner})
	if err != nil {
		t.Fatalf("winResolveProtectedPaths: %v", err)
	}
	if len(resolution.Enforced) != 1 || resolution.Enforced[0].Workspace != inner {
		t.Fatalf("enforced = %+v, want the innermost workspace %q", resolution.Enforced, inner)
	}
	if got := resolution.For(outer); len(got) != 0 {
		t.Fatalf("For(outer) = %v, want none", got)
	}
	if got := resolution.For(inner); len(got) != 1 {
		t.Fatalf("For(inner) = %v, want one path", got)
	}
}

// TestWindowsConfinedCommandProtectInsideWorkspaceOnly 固定"保护路径只对授权面内
// 生效"的调用形状：授权面之外的 protect 仍然下发给 helper，由 helper 决定跳过
// （计划层不 stat，无法判断存在性），但**不会**成为 --workspace。
func TestWindowsConfinedCommandProtectOutsideWorkspace(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	policy := sandbox.New(
		sandbox.WithRoots(root),
		sandbox.WithMode(sandbox.ModeWorkspaceWrite),
		sandbox.WithProtectedPaths(outside),
	)
	confined, err := buildWindowsConfinedCommand(`rig.exe`, []string{"cmd.exe"}, policy)
	if err != nil {
		t.Fatalf("buildWindowsConfinedCommand: %v", err)
	}
	workspaces := windowsFlagValues(confined.Argv, winFlagWorkspace)
	if wantRoot := canonicalForTest(root); len(workspaces) != 1 || workspaces[0] != wantRoot {
		t.Fatalf("--workspace values = %v, want [%q]", workspaces, wantRoot)
	}
	protects := windowsFlagValues(confined.Argv, winFlagProtect)
	if wantOutside := canonicalForTest(outside); len(protects) != 1 || !strings.EqualFold(protects[0], wantOutside) {
		t.Fatalf("--protect values = %v, want [%q]", protects, wantOutside)
	}
	// helper 会把它判为"授权面之外"并跳过（保护不了本来就不能写的东西）。
	resolution, err := winResolveProtectedPaths(protects, workspaces)
	if err != nil {
		t.Fatalf("winResolveProtectedPaths: %v", err)
	}
	if len(resolution.Enforced) != 0 || len(resolution.Notes) != 1 {
		t.Fatalf("resolution = %+v, want a skip with one diagnostic", resolution)
	}
}
