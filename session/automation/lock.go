package automation

// Cross-process registry lock: one process owns a store directory at a time.
// The lock is a lock file under the directory (LockFileName); the OS primitive
// is an advisory flock on unix, LockFileEx on Windows, and an O_EXCL+PID
// fallback elsewhere. A second Acquire (or OpenLocked, or a scheduler Start)
// reports ErrLocked instead of letting two schedulers deliver the same task.
//
// Store.OwnLock/OpenLocked tie the lock to a store; Close releases it. A
// Scheduler started over an unlocked store acquires the directory lock itself
// for its lifetime and releases it on Stop.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// LockFileName is the cross-process lock file inside a store directory.
	LockFileName = ".automations.lock"
	// defaultLockPollInterval is the retry interval while waiting for a lock.
	defaultLockPollInterval = 50 * time.Millisecond
)

// ErrLocked reports that another holder owns the store directory's lock.
var ErrLocked = errors.New("automation: registry is locked by another process")

// LockOptions configures Acquire.
type LockOptions struct {
	// Wait bounds how long Acquire retries a taken lock (0 = fail immediately).
	Wait time.Duration
	// Forever makes Acquire retry until it wins the lock or hits an I/O error.
	Forever bool
	// PollInterval is the retry interval while waiting (0 = 50ms).
	PollInterval time.Duration
}

// LockOption configures Acquire.
type LockOption func(*LockOptions)

// WithWait bounds how long Acquire waits for a taken lock.
func WithWait(wait time.Duration) LockOption {
	return func(options *LockOptions) {
		if wait > 0 {
			options.Wait = wait
		}
	}
}

// WithWaitForever makes Acquire wait until the lock is free.
func WithWaitForever() LockOption {
	return func(options *LockOptions) {
		options.Forever = true
	}
}

// WithPollInterval sets the retry interval while waiting for the lock.
func WithPollInterval(interval time.Duration) LockOption {
	return func(options *LockOptions) {
		if interval > 0 {
			options.PollInterval = interval
		}
	}
}

// resolveLockOptions applies options with defaults.
func resolveLockOptions(opts []LockOption) LockOptions {
	options := LockOptions{PollInterval: defaultLockPollInterval}
	for _, opt := range opts {
		if opt != nil {
			opt(&options)
		}
	}
	return options
}

// Lock is one held cross-process lock. Release (or Close) frees it; releasing
// twice is safe.
type Lock struct {
	mu       sync.Mutex
	file     *os.File
	path     string
	remove   bool
	released bool
}

// Path returns the lock file path.
func (l *Lock) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// Release frees the lock (and removes an O_EXCL fallback marker). It is
// idempotent.
func (l *Lock) Release() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return nil
	}
	l.released = true
	return unlockFile(l.file, l.path, l.remove)
}

// Close is Release.
func (l *Lock) Close() error { return l.Release() }

// Acquire takes the exclusive cross-process lock for the store directory dir.
// An empty dir reports ErrStoreRequired; a taken lock reports ErrLocked unless
// WithWait/WithWaitForever is set. A lock file left behind by a dead process is
// reclaimed: flock/LockFileEx are released by the OS on process exit, and the
// O_EXCL fallback detects a dead PID (or an unreadable marker).
func Acquire(dir string, opts ...LockOption) (*Lock, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, ErrStoreRequired
	}
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return nil, fmt.Errorf("automation: create lock directory %q: %w", dir, err)
	}
	path := filepath.Join(dir, LockFileName)
	options := resolveLockOptions(opts)
	var deadline time.Time
	if options.Wait > 0 && !options.Forever {
		deadline = time.Now().Add(options.Wait)
	}
	for {
		lock, err := acquireLockFile(path)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, ErrLocked) {
			return nil, err
		}
		if !options.Forever && options.Wait <= 0 {
			return nil, fmt.Errorf("%w: %s", ErrLocked, path)
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return nil, fmt.Errorf("%w: %s", ErrLocked, path)
		}
		time.Sleep(options.PollInterval)
	}
}

// acquireLockFile takes the OS lock and records the owner PID.
func acquireLockFile(path string) (*Lock, error) {
	file, remove, err := lockFile(path)
	if err != nil {
		return nil, err
	}
	if err := writeLockPID(file); err != nil {
		_ = unlockFile(file, path, remove)
		return nil, err
	}
	return &Lock{file: file, path: path, remove: remove}, nil
}

// writeLockPID records the owner PID for diagnostics and the fallback backend.
func writeLockPID(file *os.File) error {
	if err := file.Truncate(0); err != nil {
		return err
	}
	if _, err := file.Seek(0, 0); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(file, "%d\n", os.Getpid()); err != nil {
		return err
	}
	return file.Sync()
}

// OpenLocked opens (or creates) the registry under dir with the cross-process
// lock held. The lock is released by Store.Close.
func OpenLocked(dir string, opts ...LockOption) (*Store, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, ErrStoreRequired
	}
	lock, err := Acquire(dir, opts...)
	if err != nil {
		return nil, err
	}
	store, err := Open(dir)
	if err != nil {
		_ = lock.Release()
		return nil, err
	}
	store.mu.Lock()
	store.lock = lock
	store.mu.Unlock()
	return store, nil
}

// OpenFileLocked opens the registry at an explicit document path with the
// directory's cross-process lock held. The lock is released by Store.Close.
func OpenFileLocked(path string, opts ...LockOption) (*Store, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, ErrStoreRequired
	}
	lock, err := Acquire(filepath.Dir(filepath.Clean(path)), opts...)
	if err != nil {
		return nil, err
	}
	store, err := OpenFile(path)
	if err != nil {
		_ = lock.Release()
		return nil, err
	}
	store.mu.Lock()
	store.lock = lock
	store.mu.Unlock()
	return store, nil
}

// Locked reports whether the store currently owns the directory lock.
func (s *Store) Locked() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lock != nil
}

// Close releases the store's directory lock (if any). It is safe on a store
// without a lock and idempotent.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	lock := s.lock
	s.lock = nil
	s.mu.Unlock()
	if lock == nil {
		return nil
	}
	return lock.Release()
}

// ensureLock acquires the directory lock when the store has none and reports
// whether this caller owns the new lock (and must release it). A scheduler uses
// it to protect its ticker without requiring every host to call OpenLocked.
func (s *Store) ensureLock() (bool, error) {
	s.mu.Lock()
	held := s.lock != nil
	dir := s.dir
	s.mu.Unlock()
	if held {
		return false, nil
	}
	lock, err := Acquire(dir)
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	if s.lock != nil {
		// Another goroutine won the race; this lock is redundant.
		s.mu.Unlock()
		_ = lock.Release()
		return false, nil
	}
	s.lock = lock
	s.mu.Unlock()
	return true, nil
}

// releaseSchedulerLock releases a lock the scheduler acquired lazily.
func (s *Store) releaseSchedulerLock() {
	if s == nil {
		return
	}
	s.mu.Lock()
	lock := s.lock
	s.lock = nil
	s.mu.Unlock()
	if lock != nil {
		_ = lock.Release()
	}
}
