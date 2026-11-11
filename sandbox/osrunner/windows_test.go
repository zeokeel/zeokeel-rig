package osrunner

import (
	"errors"
	"io"
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
	readOnly, err := windowsRestrictingSIDStrings(winModeReadOnly, "S-1-5-5-0-1-2", "S-1-1-0", "S-1-5-21-1-2-3-4", "S-1-5-21-5-6-7-8")
	if err != nil {
		t.Fatalf("windowsRestrictingSIDStrings(read-only): %v", err)
	}
	if got, want := strings.Join(readOnly, ","), "S-1-5-5-0-1-2,S-1-1-0"; got != want {
		t.Fatalf("read-only restricting SIDs = %q, want %q", got, want)
	}

	workspaceWrite, err := windowsRestrictingSIDStrings(winModeWorkspaceWrite, "S-1-5-5-0-1-2", "S-1-1-0", "S-1-5-21-1-2-3-4", "S-1-5-21-5-6-7-8")
	if err != nil {
		t.Fatalf("windowsRestrictingSIDStrings(workspace-write): %v", err)
	}
	if got, want := strings.Join(workspaceWrite, ","), "S-1-5-5-0-1-2,S-1-1-0,S-1-5-21-1-2-3-4,S-1-5-21-5-6-7-8"; got != want {
		t.Fatalf("workspace-write restricting SIDs = %q, want %q", got, want)
	}

	if _, err := windowsRestrictingSIDStrings("danger", "a", "b", "c", "d"); err == nil {
		t.Fatal("windowsRestrictingSIDStrings(unknown mode) succeeded, want an error")
	}
}

func TestWindowsConfinedCommandWorkspaceWrite(t *testing.T) {
	root := t.TempDir()
	policy := windowsPolicy(t, sandbox.ModeWorkspaceWrite, root)
	confined, err := buildWindowsConfinedCommand(`C:\host\rig.exe`, []string{"cmd.exe", "/c", "echo hi"}, policy)
	if err != nil {
		t.Fatalf("buildWindowsConfinedCommand: %v", err)
	}
	wantPrefix := []string{`C:\host\rig.exe`, HelperArgWindowsACL, "--workspace", root, "--mode", winModeWorkspaceWrite, "--"}
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
	if got, want := strings.Join(workspaces, ","), strings.Join([]string{first, second}, ","); got != want {
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
	if confined.Enforcement != sandbox.EnforcementFull {
		t.Fatalf("Enforcement = %q, want full", confined.Enforcement)
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

	if _, err := windowsConfinedCommand(`rig.exe`, []string{"cmd.exe"}, []string{t.TempDir()}, "bogus-mode"); err == nil {
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
