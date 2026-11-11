package terminal

import "testing"

// TestDefaultShellPath pins the platform defaults: /bin/sh on Unix and Windows
// PowerShell on Windows (pipe transport keeps the tool usable without ConPTY).
func TestDefaultShellPath(t *testing.T) {
	cases := []struct {
		goos string
		want string
	}{
		{"linux", "/bin/sh"},
		{"darwin", "/bin/sh"},
		{"freebsd", "/bin/sh"},
		{"windows", "powershell.exe"},
		{"Windows", "powershell.exe"},
	}
	for _, tc := range cases {
		if got := defaultShellPath(tc.goos); got != tc.want {
			t.Errorf("defaultShellPath(%q) = %q, want %q", tc.goos, got, tc.want)
		}
	}
}
