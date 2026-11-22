package automation

// Cross-process registry lock: one process owns a store directory; a second
// Acquire/OpenLocked reports ErrLocked (or waits per option) instead of letting
// two schedulers deliver the same task twice. The lock is released on
// Release/Close and a dead process's lock is recovered.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAcquireAndRelease(t *testing.T) {
	dir := t.TempDir()
	lock, err := Acquire(dir)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if !strings.HasSuffix(lock.Path(), LockFileName) {
		t.Fatalf("lock path = %q, want the %q file", lock.Path(), LockFileName)
	}
	if lock.Path() != filepath.Join(dir, LockFileName) {
		t.Fatalf("lock path = %q, want %q", lock.Path(), filepath.Join(dir, LockFileName))
	}
	data, err := os.ReadFile(lock.Path())
	if err != nil {
		t.Fatalf("read lock file: %v", err)
	}
	if strings.TrimSpace(string(data)) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("lock file PID = %q, want %d", strings.TrimSpace(string(data)), os.Getpid())
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("second Release: %v", err)
	}
	again, err := Acquire(dir)
	if err != nil {
		t.Fatalf("Acquire after release: %v", err)
	}
	if err := again.Release(); err != nil {
		t.Fatalf("release second lock: %v", err)
	}
}

func TestAcquireReportsErrLocked(t *testing.T) {
	dir := t.TempDir()
	first, err := Acquire(dir)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer first.Release()
	if _, err := Acquire(dir); !errors.Is(err, ErrLocked) {
		t.Fatalf("second Acquire = %v, want ErrLocked", err)
	}
}

func TestAcquireWaitsForRelease(t *testing.T) {
	dir := t.TempDir()
	first, err := Acquire(dir)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(60 * time.Millisecond)
		_ = first.Release()
	}()
	second, err := Acquire(dir, WithWait(3*time.Second), WithPollInterval(5*time.Millisecond))
	if err != nil {
		t.Fatalf("waiting Acquire: %v", err)
	}
	<-released
	if err := second.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

func TestAcquireWaitTimeout(t *testing.T) {
	dir := t.TempDir()
	first, err := Acquire(dir)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer first.Release()
	start := time.Now()
	if _, err := Acquire(dir, WithWait(40*time.Millisecond), WithPollInterval(5*time.Millisecond)); !errors.Is(err, ErrLocked) {
		t.Fatalf("waiting Acquire = %v, want ErrLocked", err)
	}
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond {
		t.Fatalf("Acquire returned after %s, want it to wait for the timeout", elapsed)
	}
}

// TestStaleLockFileRecovered leaves a lock file behind without a live lock
// holder (the shape a crashed process or an O_EXCL fallback leaves). Acquiring
// must succeed and take ownership of the file.
func TestStaleLockFileRecovered(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, LockFileName)
	if err := os.WriteFile(stale, []byte("999999\n"), 0o600); err != nil {
		t.Fatalf("write stale lock: %v", err)
	}
	lock, err := Acquire(dir)
	if err != nil {
		t.Fatalf("Acquire over a stale lock: %v", err)
	}
	defer lock.Release()
	data, err := os.ReadFile(stale)
	if err != nil {
		t.Fatalf("read lock: %v", err)
	}
	if strings.TrimSpace(string(data)) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("stale lock was not reclaimed: %q", strings.TrimSpace(string(data)))
	}
}

func TestAcquireValidation(t *testing.T) {
	if _, err := Acquire("   "); !errors.Is(err, ErrStoreRequired) {
		t.Fatalf("Acquire(empty) = %v, want ErrStoreRequired", err)
	}
}

func TestOpenLockedExclusive(t *testing.T) {
	dir := t.TempDir()
	first, err := OpenLocked(dir)
	if err != nil {
		t.Fatalf("OpenLocked: %v", err)
	}
	if !first.Locked() {
		t.Fatalf("Locked() = false after OpenLocked")
	}
	if _, err := OpenLocked(dir); !errors.Is(err, ErrLocked) {
		t.Fatalf("second OpenLocked = %v, want ErrLocked", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	second, err := OpenLocked(dir)
	if err != nil {
		t.Fatalf("OpenLocked after Close: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("close second: %v", err)
	}
}

func TestSaveLoadWhileLockHeld(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenLocked(dir)
	if err != nil {
		t.Fatalf("OpenLocked: %v", err)
	}
	defer store.Close()
	now := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	task := Task{ID: "t1", Name: "locked", Prompt: "p", Enabled: true, CreatedAt: now, UpdatedAt: now}
	if _, err := store.Create(task); err != nil {
		t.Fatalf("Create while locked: %v", err)
	}
	if err := store.Save(); err != nil {
		t.Fatalf("Save while locked: %v", err)
	}
	if err := store.Load(); err != nil {
		t.Fatalf("Load while locked: %v", err)
	}
	tasks, err := store.List()
	if err != nil || len(tasks) != 1 || tasks[0].ID != "t1" {
		t.Fatalf("List while locked = %+v/%v", tasks, err)
	}
	// Another process cannot take the lock while the owner is alive.
	if _, err := OpenLocked(dir); !errors.Is(err, ErrLocked) {
		t.Fatalf("second OpenLocked = %v, want ErrLocked", err)
	}
}

// schedulerOver builds a minimal scheduler over store for lock tests.
func schedulerOver(t *testing.T, store *Store, deliveries *atomic.Int64) *Scheduler {
	t.Helper()
	return &Scheduler{
		Store: store,
		Now:   func() time.Time { return time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC) },
		Deliver: func(context.Context, Task) (string, error) {
			if deliveries != nil {
				deliveries.Add(1)
			}
			return "delivery", nil
		},
		Next:     func(_ Task, after time.Time) (time.Time, bool) { return after.Add(time.Hour), true },
		Interval: 5 * time.Millisecond,
	}
}

func TestSchedulerStartSecondInstanceLocked(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenLocked(dir)
	if err != nil {
		t.Fatalf("OpenLocked: %v", err)
	}
	now := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	task := Task{ID: "t1", Name: "due", Prompt: "p", Enabled: true, CreatedAt: now, UpdatedAt: now, NextRun: now}
	if _, err := store.Create(task); err != nil {
		t.Fatalf("Create: %v", err)
	}
	first := schedulerOver(t, store, nil)
	if err := first.Start(); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	defer func() {
		first.Stop()
		_ = store.Close()
	}()

	// A second store over the same directory cannot start its scheduler.
	other, err := Open(dir)
	if err != nil {
		t.Fatalf("Open second: %v", err)
	}
	second := schedulerOver(t, other, nil)
	if err := second.Start(); !errors.Is(err, ErrLocked) {
		t.Fatalf("second Start = %v, want ErrLocked", err)
	}
	second.Stop() // no-op; never started
}

func TestSchedulerStartStopReleasesLazyLock(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	scheduler := schedulerOver(t, store, nil)
	if err := scheduler.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// While running, a second scheduler over another store is locked out.
	other, err := Open(dir)
	if err != nil {
		t.Fatalf("Open second: %v", err)
	}
	if err := schedulerOver(t, other, nil).Start(); !errors.Is(err, ErrLocked) {
		t.Fatalf("second Start = %v, want ErrLocked", err)
	}
	scheduler.Stop()
	// Stop releases the lazily acquired lock.
	after, err := Open(dir)
	if err != nil {
		t.Fatalf("Open after stop: %v", err)
	}
	restarted := schedulerOver(t, after, nil)
	if err := restarted.Start(); err != nil {
		t.Fatalf("Start after Stop released the lock: %v", err)
	}
	restarted.Stop()
}

func TestTwoSchedulersNeverDoubleDeliver(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	owner, err := OpenLocked(dir)
	if err != nil {
		t.Fatalf("OpenLocked: %v", err)
	}
	defer owner.Close()
	task := Task{ID: "t1", Name: "due", Prompt: "p", Enabled: true, CreatedAt: now, UpdatedAt: now, NextRun: now}
	if _, err := owner.Create(task); err != nil {
		t.Fatalf("Create: %v", err)
	}
	var ownerDeliveries atomic.Int64
	ownerScheduler := schedulerOver(t, owner, &ownerDeliveries)
	if err := ownerScheduler.Start(); err != nil {
		t.Fatalf("owner Start: %v", err)
	}
	defer ownerScheduler.Stop()

	challenger, err := Open(dir)
	if err != nil {
		t.Fatalf("Open challenger: %v", err)
	}
	var challengerDeliveries atomic.Int64
	challengerScheduler := schedulerOver(t, challenger, &challengerDeliveries)
	if err := challengerScheduler.Start(); !errors.Is(err, ErrLocked) {
		t.Fatalf("challenger Start = %v, want ErrLocked", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for ownerDeliveries.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if ownerDeliveries.Load() == 0 {
		t.Fatalf("owner scheduler never delivered")
	}
	if challengerDeliveries.Load() != 0 {
		t.Fatalf("challenger delivered %d times despite being locked out", challengerDeliveries.Load())
	}
}

// TestLockErrorTextMentionsPath keeps the operator-facing error readable.
func TestLockErrorTextMentionsPath(t *testing.T) {
	dir := t.TempDir()
	first, err := Acquire(dir)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer first.Release()
	_, err = Acquire(dir)
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("Acquire = %v, want ErrLocked", err)
	}
	if !strings.Contains(err.Error(), filepath.Join(dir, LockFileName)) {
		t.Fatalf("ErrLocked text = %q, want the lock path", err.Error())
	}
}
