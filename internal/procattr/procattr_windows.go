//go:build windows

package procattr

import (
	"os/exec"
	"syscall"
)

// HideWindow makes the console window Windows may create for the child start
// hidden (STARTF_USESHOWWINDOW with SW_HIDE), so a GUI host does not pop a black
// window for every command it runs.
//
// Call it after any SysProcAttr setup: it keeps the fields already set there
// (a sandbox token, creation flags) and only adds the hiding.
func HideWindow(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
}
