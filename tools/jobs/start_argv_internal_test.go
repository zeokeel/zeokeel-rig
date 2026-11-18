package jobs

import (
	"errors"
	"github.com/zeokeel/zeokeel-rig/internal/procattr"
	"strings"
	"testing"
)

// TestStartArgvUnix pins the existing /bin/sh -c behavior.
func TestStartArgvUnix(t *testing.T) {
	argv, kind, err := startArgv("linux", "echo hi", nil)
	if err != nil {
		t.Fatalf("startArgv(linux) error: %v", err)
	}
	want := []string{"/bin/sh", "-c", "echo hi"}
	if len(argv) != len(want) || argv[0] != want[0] || argv[1] != want[1] || argv[2] != want[2] {
		t.Fatalf("argv = %v, want %v", argv, want)
	}
	if kind != "bash" {
		t.Fatalf("kind = %q, want bash", kind)
	}
}

// TestStartArgvWindowsPrefersPowerShell covers the new Windows branch: pwsh
// first, then Windows PowerShell.
func TestStartArgvWindowsPrefersPowerShell(t *testing.T) {
	lookPath := func(name string) (string, error) {
		switch name {
		case "pwsh":
			return `C:\Program Files\PowerShell\7\pwsh.exe`, nil
		case "powershell":
			return `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`, nil
		default:
			return "", errors.New("not found")
		}
	}
	argv, kind, err := startArgv("windows", "Get-Date", lookPath)
	if err != nil {
		t.Fatalf("startArgv(windows) error: %v", err)
	}
	if argv[0] != `C:\Program Files\PowerShell\7\pwsh.exe` || argv[1] != "-NoProfile" ||
		argv[2] != "-NonInteractive" || argv[3] != "-Command" {
		t.Fatalf("argv = %v, want pwsh -NoProfile -NonInteractive -Command <script>", argv)
	}
	// 脚本必须被 UTF-8 前置语句包裹（否则中文输出在管道里是 GBK 字节），
	// 且包裹后仍以用户脚本结尾——前置语句不能改变脚本语义。
	wrapped := procattr.WrapPwshScript("Get-Date")
	if argv[4] != wrapped {
		t.Fatalf("argv[4] = %q, want the wrapped script %q", argv[4], wrapped)
	}
	if !strings.HasSuffix(argv[4], "Get-Date") {
		t.Fatalf("wrapped script does not end with the user script: %q", argv[4])
	}
	if kind != "pwsh" {
		t.Fatalf("kind = %q, want pwsh", kind)
	}
}

// TestStartArgvWindowsFallbackAndError covers the Windows PowerShell and the
// no-shell error path.
func TestStartArgvWindowsFallbackAndError(t *testing.T) {
	argv, kind, err := startArgv("windows", "Get-Date", func(name string) (string, error) {
		if name == "powershell" {
			return `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`, nil
		}
		return "", errors.New("not found")
	})
	if err != nil || kind != "pwsh" || argv[0] != `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe` {
		t.Fatalf("fallback argv=%v kind=%q err=%v", argv, kind, err)
	}
	if argv[4] != procattr.WrapPwshScript("Get-Date") {
		t.Fatalf("fallback argv[4] = %q, want the UTF-8 wrapped script", argv[4])
	}

	if _, _, err := startArgv("windows", "x", func(string) (string, error) { return "", errors.New("missing") }); err == nil {
		t.Fatal("startArgv(windows, no shell) succeeded, want an error")
	}
}

// TestStartArgvCommandPromptSetsUTF8CodePage 断言 cmd.exe 分支先切到 UTF-8 代码页，
// 且用 `&` 而不是 `&&`——用户命令退出码非零时仍应是"最后一条命令的退出码"。
func TestStartArgvCommandPromptSetsUTF8CodePage(t *testing.T) {
	argv, kind, err := startArgv("windows", "echo 中文", func(name string) (string, error) {
		if name == "cmd.exe" {
			return `C:\Windows\System32\cmd.exe`, nil
		}
		return "", errors.New("not found")
	})
	if err != nil || kind != "pwsh" {
		t.Fatalf("argv=%v kind=%q err=%v", argv, kind, err)
	}
	if len(argv) != 5 || argv[3] != "/c" {
		t.Fatalf("argv = %v, want cmd.exe /d /s /c <script>", argv)
	}
	if argv[4] != "chcp 65001>NUL & echo 中文" {
		t.Fatalf("argv[4] = %q, want the UTF-8 code page switch prefixed with &", argv[4])
	}
}
