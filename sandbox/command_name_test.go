package sandbox

// Internal tests for the platform-dependent command-name matching behind
// WithCommandAllowlist/WithCommandDenylist: the Windows decision is injected as a
// plain bool, so both the windows=true and windows=false branches run on any host
// (including Linux CI).
//
// This file is deliberately not named *_windows_test.go: Go treats that filename
// suffix as an implicit GOOS build constraint, which would skip the tests on
// non-Windows hosts and defeat the point of the injected platform flag.

import (
	"errors"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestNormalizeCommandName(t *testing.T) {
	tests := []struct {
		name    string
		command string
		windows bool
		want    string
	}{
		// Windows：不区分大小写、反复剥掉 .exe，归一化是幂等的（策略构造期与
		// 比较期各调用一次也不会多剥），短名不越界。
		{name: "windows bare name", command: "pwsh", windows: true, want: "pwsh"},
		{name: "windows exe suffix", command: "pwsh.exe", windows: true, want: "pwsh"},
		{name: "windows mixed case", command: "PowerShell.EXE", windows: true, want: "powershell"},
		{name: "windows already normalized", command: "powershell", windows: true, want: "powershell"},
		{name: "windows repeated suffix", command: "pwsh.exe.exe", windows: true, want: "pwsh"},
		{name: "windows short name", command: ".exe", windows: true, want: ".exe"},
		{name: "windows empty", command: "", windows: true, want: ""},
		// 非 Windows：原样返回，绝不剥 .exe，也不改大小写。
		{name: "unix keeps exe", command: "pwsh.exe", windows: false, want: "pwsh.exe"},
		{name: "unix keeps case", command: "PWSh.EXE", windows: false, want: "PWSh.EXE"},
		{name: "unix bare name", command: "pwsh", windows: false, want: "pwsh"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := normalizeCommandName(test.command, test.windows)
			if got != test.want {
				t.Fatalf("normalizeCommandName(%q, windows=%v) = %q, want %q",
					test.command, test.windows, got, test.want)
			}
		})
	}
}

func TestCommandNameMatches(t *testing.T) {
	tests := []struct {
		name      string
		candidate string
		command   string
		windows   bool
		want      bool
	}{
		// Windows：allowlist 的 "powershell" 与 argv[0] 的 "powershell.exe" 双向
		// 等价，且忽略大小写。
		{name: "windows allow bare vs exe", candidate: "powershell", command: "powershell.exe", windows: true, want: true},
		{name: "windows allow exe vs bare", candidate: "powershell.exe", command: "powershell", windows: true, want: true},
		{name: "windows mixed case", candidate: "POWERSHELL", command: "PowerShell.Exe", windows: true, want: true},
		{name: "windows pwsh exe vs bare", candidate: "pwsh.exe", command: "pwsh", windows: true, want: true},
		{name: "windows both bare", candidate: "pwsh", command: "pwsh", windows: true, want: true},
		{name: "windows different names", candidate: "pwsh", command: "powershell", windows: true, want: false},
		{name: "windows different stems", candidate: "pwsh.exe", command: "pwshx.exe", windows: true, want: false},
		// 非 Windows：按字面精确比较，.exe 不归一化，大小写敏感。
		{name: "unix exe not stripped", candidate: "powershell", command: "powershell.exe", windows: false, want: false},
		{name: "unix exe reverse not stripped", candidate: "powershell.exe", command: "powershell", windows: false, want: false},
		{name: "unix case sensitive", candidate: "PWSh", command: "pwsh", windows: false, want: false},
		{name: "unix exact", candidate: "git", command: "git", windows: false, want: true},
		{name: "unix exe exact", candidate: "git.exe", command: "git.exe", windows: false, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := commandNameMatches(test.candidate, test.command, test.windows)
			if got != test.want {
				t.Fatalf("commandNameMatches(%q, %q, windows=%v) = %v, want %v",
					test.candidate, test.command, test.windows, got, test.want)
			}
		})
	}
}

func TestContainsCommandName(t *testing.T) {
	tests := []struct {
		name    string
		names   []string
		command string
		windows bool
		want    bool
	}{
		// Windows：allowlist/denylist 存 "pwsh"，argv[0] 传来 "pwsh.exe"（反之亦然）。
		{name: "windows bare list vs exe command", names: []string{"pwsh"}, command: "pwsh.exe", windows: true, want: true},
		{name: "windows exe list vs bare command", names: []string{"pwsh.exe", "powershell.exe"}, command: "powershell", windows: true, want: true},
		{name: "windows mixed case list", names: []string{"PWSH.EXE", "powershell.exe"}, command: "pwsh", windows: true, want: true},
		{name: "windows miss", names: []string{"pwsh"}, command: "powershell", windows: true, want: false},
		{name: "windows empty list", names: nil, command: "pwsh", windows: true, want: false},
		// 非 Windows：.exe 不归一化。
		{name: "unix exe not matched", names: []string{"pwsh"}, command: "pwsh.exe", windows: false, want: false},
		{name: "unix bare not matched", names: []string{"pwsh.exe"}, command: "pwsh", windows: false, want: false},
		{name: "unix exact", names: []string{"pwsh"}, command: "pwsh", windows: false, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := containsCommandName(test.names, test.command, test.windows)
			if got != test.want {
				t.Fatalf("containsCommandName(%v, %q, windows=%v) = %v, want %v",
					test.names, test.command, test.windows, got, test.want)
			}
		})
	}
}

func TestNormalizeCommandNames(t *testing.T) {
	tests := []struct {
		name     string
		commands []string
		windows  bool
		want     []string
	}{
		{
			name:     "windows strips exe and case",
			commands: []string{" pwsh.exe ", "./pwsh", "PWSH.EXE", "", "/opt/tools/pwsh.exe", "powershell"},
			windows:  true,
			want:     []string{"pwsh", "powershell"},
		},
		{
			name:     "unix keeps names",
			commands: []string{"pwsh", "pwsh.exe", "PWSH.EXE", " powershell.exe ", "/opt/tools/pwsh.exe"},
			windows:  false,
			// 既有行为：基名、去空白、按小写 key 去重，但保留首次写法的原样。
			want: []string{"pwsh", "pwsh.exe", "powershell.exe"},
		},
		{
			name:     "empty input",
			commands: nil,
			windows:  true,
			want:     []string{},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := normalizeCommandNames(test.commands, test.windows)
			if !slices.Equal(got, test.want) {
				t.Fatalf("normalizeCommandNames(%v, windows=%v) = %v, want %v",
					test.commands, test.windows, got, test.want)
			}
		})
	}
}

// TestCheckCommandWindowsSemantics 用注入的 windows=true 在 Linux 上端到端复现
// 审计缺陷场景：宿主 allowlist 写 ["pwsh", "powershell"]，pwsh 工具把 argv[0]
// 传成 "pwsh.exe"/"powershell.exe"。修复前会被误判为 not in the allowlist，
// 修复后放行；windows=false 保持严格字面匹配（.exe 不归一化）。
func TestCheckCommandWindowsSemantics(t *testing.T) {
	root := t.TempDir()
	policy := New(WithRoots(root), WithCommandAllowlist("pwsh", "powershell"))

	// Windows：大小写与 .exe 后缀都不影响 allowlist 匹配（双向等价）。
	for _, argv := range [][]string{
		{"powershell"},
		{"powershell.exe"},
		{"PowerShell.EXE"},
		{"pwsh"},
		{"pwsh.exe"},
		{"PWSH.EXE", "-NoProfile", "-Command", "Write-Output hi"},
	} {
		if err := policy.checkCommand(argv, "", true); err != nil {
			t.Fatalf("checkCommand(%v, windows=true) = %v, want allowed", argv, err)
		}
	}
	if err := policy.checkCommand([]string{"cmd.exe"}, "", true); err == nil || !errors.Is(err, ErrDenied) {
		t.Fatalf("checkCommand(cmd.exe, windows=true) = %v, want ErrDenied", err)
	}

	// 反向：allowlist 写 ".exe" 时，argv[0] 传裸名也在 Windows 上匹配。
	reverse := New(WithRoots(root), WithCommandAllowlist("powershell.exe"))
	if err := reverse.checkCommand([]string{"powershell"}, "", true); err != nil {
		t.Fatalf("checkCommand(powershell, windows=true) with allowlist %q = %v, want allowed", "powershell.exe", err)
	}

	// 非 Windows：既有行为不变——.exe 不归一化。
	for _, argv := range [][]string{{"pwsh"}, {"powershell"}} {
		if err := policy.checkCommand(argv, "", false); err != nil {
			t.Fatalf("checkCommand(%v, windows=false) = %v, want allowed", argv, err)
		}
	}
	for _, argv := range [][]string{{"pwsh.exe"}, {"PowerShell.EXE"}} {
		if err := policy.checkCommand(argv, "", false); err == nil || !errors.Is(err, ErrDenied) {
			t.Fatalf("checkCommand(%v, windows=false) = %v, want ErrDenied (.exe stays literal)", argv, err)
		}
	}

	// denylist 使用同样的 Windows 语义：allowlist 收 "powershell"，denylist 写
	// "PowerShell.EXE" 时，argv[0] 为 "powershell.exe" 仍被 deny 拦下。
	denied := New(WithRoots(root), WithCommandAllowlist("powershell"), WithCommandDenylist("PowerShell.EXE"))
	err := denied.checkCommand([]string{"powershell.exe"}, "", true)
	if err == nil || !errors.Is(err, ErrDenied) || !strings.Contains(err.Error(), "denied by the policy") {
		t.Fatalf("checkCommand(powershell.exe, windows=true) = %v, want policy denial from the denylist", err)
	}
	// 非 Windows：denylist 里的 "PowerShell.EXE" 不会匹配裸名 "powershell"。
	if err := denied.checkCommand([]string{"powershell"}, "", false); err != nil {
		t.Fatalf("checkCommand(powershell, windows=false) = %v, want allowed (denylist .exe entry stays literal)", err)
	}

	// danger-full-access 分支同样按 Windows 语义匹配显式 denylist。
	full := New(WithRoots(root), WithMode(ModeDangerFullAccess), WithCommandDenylist("powershell.exe"))
	err = full.checkCommand([]string{"PowerShell.EXE"}, "", true)
	if err == nil || !errors.Is(err, ErrDenied) {
		t.Fatalf("danger-full-access checkCommand(PowerShell.EXE, windows=true) = %v, want ErrDenied", err)
	}
}

// TestCheckCommandExeSuffixPerPlatform 验证生产接线：CheckCommand 内部从
// runtime.GOOS 计算平台语义。windows=true 的分支只有 Windows 宿主能端到端观察到，
// 因此按平台断言；非 Windows 宿主上同一表项断言 .exe 不被剥离（行为不变）。
func TestCheckCommandExeSuffixPerPlatform(t *testing.T) {
	root := t.TempDir()
	windows := runtime.GOOS == "windows"

	allow := New(WithRoots(root), WithCommandAllowlist("powershell"))
	err := allow.CheckCommand([]string{"PowerShell.EXE"}, "")
	if windows {
		if err != nil {
			t.Fatalf("CheckCommand(PowerShell.EXE) on Windows = %v, want allowed via allowlist entry %q", err, "powershell")
		}
	} else if err == nil || !errors.Is(err, ErrDenied) {
		t.Fatalf("CheckCommand(PowerShell.EXE) outside Windows = %v, want ErrDenied (.exe must not be normalized)", err)
	}

	deny := New(WithRoots(root), WithCommandAllowlist("powershell"), WithCommandDenylist("POWERSHELL.EXE"))
	err = deny.CheckCommand([]string{"powershell"}, "")
	if windows {
		if err == nil || !errors.Is(err, ErrDenied) {
			t.Fatalf("CheckCommand(powershell) with denylist POWERSHELL.EXE on Windows = %v, want ErrDenied", err)
		}
	} else if err != nil {
		t.Fatalf("CheckCommand(powershell) outside Windows = %v, want allowed (denylist entry with .exe stays literal)", err)
	}
}
