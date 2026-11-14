//go:build windows

package procattr

import (
	"os/exec"
	"syscall"
	"testing"
)

// TestHideWindowSetsHideWindow is the Windows contract: a prepared command asks
// Windows for a hidden initial window.
func TestHideWindowSetsHideWindow(t *testing.T) {
	cmd := exec.Command("cmd.exe")
	HideWindow(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.HideWindow {
		t.Fatalf("HideWindow did not set SysProcAttr.HideWindow: %+v", cmd.SysProcAttr)
	}
}

// TestHideWindowKeepsExistingAttributes makes sure preparation does not clobber
// the sandbox token or creation flags a caller set first.
func TestHideWindowKeepsExistingAttributes(t *testing.T) {
	const creationFlags = 0x08000000 // CREATE_NO_WINDOW, as an existing caller's flag
	cmd := exec.Command("cmd.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: creationFlags}
	HideWindow(cmd)
	if !cmd.SysProcAttr.HideWindow {
		t.Fatalf("HideWindow did not set SysProcAttr.HideWindow")
	}
	if cmd.SysProcAttr.CreationFlags != creationFlags {
		t.Fatalf("HideWindow lost CreationFlags: %+v", cmd.SysProcAttr)
	}
}
