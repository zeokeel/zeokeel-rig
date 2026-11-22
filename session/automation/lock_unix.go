//go:build unix

package automation

// Unix lock backend: an advisory flock on the lock file. The kernel releases it
// when the process exits or the descriptor closes, so a crashed process can
// never leave the lock held; a leftover lock file is harmless.

import (
	"errors"
	"os"
	"syscall"
)

// lockFile takes the exclusive flock and reports whether the marker file is
// removed on release (false: the flock, not the file, is the lock).
func lockFile(path string) (*os.File, bool, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, fileMode)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, false, ErrLocked
		}
		return nil, false, err
	}
	return file, false, nil
}

// unlockFile releases the flock and closes the descriptor. The lock file stays
// behind (harmless: the next owner reuses it).
func unlockFile(file *os.File, path string, remove bool) error {
	if file == nil {
		if remove && path != "" {
			_ = os.Remove(path)
		}
		return nil
	}
	var firstErr error
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN); err != nil {
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
