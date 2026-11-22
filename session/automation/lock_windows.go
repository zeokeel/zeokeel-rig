//go:build windows

package automation

// Windows lock backend: LockFileEx with LOCKFILE_FAIL_IMMEDIATELY. The OS
// releases the lock when the process exits or the handle closes, so a crashed
// process can never leave the lock held.
//
// LockFileEx lives in golang.org/x/sys/windows (the stdlib syscall package does
// not expose it); the module is already a dependency of this repo.

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockFile takes the exclusive byte-range lock.
func lockFile(path string) (*os.File, bool, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, fileMode)
	if err != nil {
		return nil, false, err
	}
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK | windows.LOCKFILE_FAIL_IMMEDIATELY)
	if err := windows.LockFileEx(windows.Handle(file.Fd()), flags, 0, 1, 0, new(windows.Overlapped)); err != nil {
		_ = file.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
			return nil, false, ErrLocked
		}
		return nil, false, err
	}
	return file, false, nil
}

// unlockFile releases the byte-range lock and closes the handle.
func unlockFile(file *os.File, path string, remove bool) error {
	if file == nil {
		if remove && path != "" {
			_ = os.Remove(path)
		}
		return nil
	}
	var firstErr error
	if err := windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, new(windows.Overlapped)); err != nil {
		firstErr = err
	}
	if err := file.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	if remove && path != "" {
		_ = os.Remove(path)
	}
	return firstErr
}
