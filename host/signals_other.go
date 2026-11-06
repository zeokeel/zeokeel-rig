//go:build !unix

package host

import "os"

// terminationSignals are the signals a headless product treats as a graceful
// shutdown request. Outside Unix the portable interrupt is the only one Go
// delivers.
var terminationSignals = []os.Signal{os.Interrupt}
