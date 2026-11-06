//go:build unix

package host

import (
	"os"
	"syscall"
)

// terminationSignals are the signals a headless product treats as a graceful
// shutdown request.
var terminationSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}
