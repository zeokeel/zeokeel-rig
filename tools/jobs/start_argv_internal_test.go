package jobs

import (
	"errors"
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
		argv[2] != "-NonInteractive" || argv[3] != "-Command" || argv[4] != "Get-Date" {
		t.Fatalf("argv = %v, want pwsh -NoProfile -NonInteractive -Command Get-Date", argv)
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

	if _, _, err := startArgv("windows", "x", func(string) (string, error) { return "", errors.New("missing") }); err == nil {
		t.Fatal("startArgv(windows, no shell) succeeded, want an error")
	}
}
