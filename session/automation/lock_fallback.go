//go:build !unix && !windows

package automation

// Fallback lock backend for platforms without flock/LockFileEx: an exclusive
// O_EXCL marker file containing the owner PID. A marker that is unreadable, or
// whose PID is this process, is stale at once; another PID's marker is stale
// after staleLockAge (there is no portable liveness probe on these platforms,
// and a fresh marker is given a grace period so the owner can write its PID).
// Release removes the marker.

import (
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// staleLockAge bounds how long a foreign PID marker is trusted.
	staleLockAge = 10 * time.Minute
	// staleLockGrace is the grace period for a marker that has no PID yet.
	staleLockGrace = 2 * time.Second
)

// lockFile creates the exclusive marker file.
func lockFile(path string) (*os.File, bool, error) {
	for attempt := 0; attempt < 3; attempt++ {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, fileMode)
		if err == nil {
			return file, true, nil
		}
		if !os.IsExist(err) {
			return nil, false, err
		}
		if !staleExclusiveLock(path) {
			return nil, false, ErrLocked
		}
		_ = os.Remove(path)
	}
	return nil, false, ErrLocked
}

// staleExclusiveLock reports whether an existing marker can be reclaimed.
func staleExclusiveLock(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return true
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return time.Since(info.ModTime()) > staleLockGrace
	}
	if pid == os.Getpid() {
		return true
	}
	return time.Since(info.ModTime()) > staleLockAge
}

// unlockFile closes and removes the marker file.
func unlockFile(file *os.File, path string, remove bool) error {
	if file == nil {
		if remove && path != "" {
			_ = os.Remove(path)
		}
		return nil
	}
	closeErr := file.Close()
	if remove && path != "" {
		_ = os.Remove(path)
	}
	return closeErr
}
