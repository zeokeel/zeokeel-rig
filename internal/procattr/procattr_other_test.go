//go:build !windows

package procattr

import (
	"os/exec"
	"syscall"
	"testing"
)

// TestHideWindowIsNoOpOffWindows keeps the helper free of POSIX side effects:
// a child on another platform has no console window, and any process attributes
// the caller already set stay untouched.
func TestHideWindowIsNoOpOffWindows(t *testing.T) {
	cmd := exec.Command("true")
	HideWindow(cmd)
	if cmd.SysProcAttr != nil {
		t.Fatalf("HideWindow set SysProcAttr on a non-Windows platform: %+v", cmd.SysProcAttr)
	}

	existing := &syscall.SysProcAttr{Setpgid: true}
	cmd = exec.Command("true")
	cmd.SysProcAttr = existing
	HideWindow(cmd)
	if cmd.SysProcAttr != existing || !existing.Setpgid {
		t.Fatalf("HideWindow changed existing process attributes: %+v", cmd.SysProcAttr)
	}
}
