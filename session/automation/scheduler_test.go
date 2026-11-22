package automation

// Tests for the scheduler core: due selection, claim/deliver/settle
// transitions, restart recovery, folding an in-flight trigger, bounded
// concurrency, delete/cancel, one-shot expiry, the run-history bound and
// Start/Stop. Written before the implementation (TDD red phase).

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// testClock is an injectable clock.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock(now time.Time) *testClock {
	return &testClock{now: now}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(delta time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(delta)
	c.mu.Unlock()
}

// schedulerRecorder is the fake Deliver/Settle pair.
type schedulerRecorder struct {
	mu sync.Mutex

	deliverCalls int
	deliveries   []Task
	deliverErr   error

	settleCalls  int
	settleIDs    []string
	settleStatus string
	settleDetail string
	settleDone   bool
}

func newSchedulerRecorder() *schedulerRecorder {
	return &schedulerRecorder{
		settleStatus: StatusSuccess,
		settleDone:   true,
	}
}

func (r *schedulerRecorder) Deliver(_ context.Context, task Task) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deliverCalls++
	if r.deliverErr != nil {
		return "", r.deliverErr
	}
	id := fmt.Sprintf("delivery-%d", r.deliverCalls)
	r.deliveries = append(r.deliveries, task)
	return id, nil
}

func (r *schedulerRecorder) Settle(_ context.Context, _ Task, deliveryID string) (string, string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.settleCalls++
	r.settleIDs = append(r.settleIDs, deliveryID)
	return r.settleStatus, r.settleDetail, r.settleDone
}

func (r *schedulerRecorder) setDeliverError(err error) {
	r.mu.Lock()
	r.deliverErr = err
	r.mu.Unlock()
}

func (r *schedulerRecorder) setSettle(done bool, status, detail string) {
	r.mu.Lock()
	r.settleDone = done
	r.settleStatus = status
	r.settleDetail = detail
	r.mu.Unlock()
}

func (r *schedulerRecorder) counts() (deliver, settle int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.deliverCalls, r.settleCalls
}

func newScheduledScheduler(store *Store, clock *testClock, recorder *schedulerRecorder) *Scheduler {
	return &Scheduler{
		Store:         store,
		Now:           clock.Now,
		Deliver:       recorder.Deliver,
		Settle:        recorder.Settle,
		Next:          func(_ Task, after time.Time) (time.Time, bool) { return after.Add(time.Hour), true },
		MaxConcurrent: 4,
		Interval:      time.Hour,
	}
}

func dueTask(id string, now time.Time) Task {
	return Task{
		ID:        id,
		Name:      id,
		Prompt:    "prompt-" + id,
		Schedule:  Schedule("every 60s"),
		Enabled:   true,
		CreatedAt: now.Add(-time.Hour),
		UpdatedAt: now.Add(-time.Hour),
		NextRun:   now.Add(-time.Minute),
	}
}

func mustCreateTask(t *testing.T, store *Store, task Task) Task {
	t.Helper()
	created, err := store.Create(task)
	if err != nil {
		t.Fatalf("Store.Create(%q): %v", task.ID, err)
	}
	return created
}

func TestTickDeliversOnlyDueEnabledTasks(t *testing.T) {
	clock := newTestClock(time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC))
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	now := clock.Now()
	mustCreateTask(t, store, dueTask("due", now))
	future := dueTask("future", now)
	future.NextRun = now.Add(time.Minute)
	mustCreateTask(t, store, future)
	disabled := dueTask("disabled", now)
	disabled.Enabled = false
	mustCreateTask(t, store, disabled)
	unscheduled := dueTask("unscheduled", now)
	unscheduled.NextRun = time.Time{}
	mustCreateTask(t, store, unscheduled)

	recorder := newSchedulerRecorder()
	scheduler := newScheduledScheduler(store, clock, recorder)
	if err := scheduler.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if deliver, _ := recorder.counts(); deliver != 1 {
		t.Fatalf("deliveries = %d, want 1", deliver)
	}
	ran, ok := store.Get("due")
	if !ok {
		t.Fatal("due task disappeared")
	}
	if ran.LastRun != now || !ran.NextRun.Equal(now.Add(time.Hour)) || !ran.Enabled {
		t.Fatalf("ran task = %+v, want LastRun=now, next=+1h, enabled", ran)
	}
	if len(ran.RunHistory) != 1 {
		t.Fatalf("run history = %+v", ran.RunHistory)
	}
	run := ran.RunHistory[0]
	if run.Status != StatusSuccess || run.DeliveryID != "delivery-1" ||
		run.StartedAt != now || run.DeliveredAt != now || run.CompletedAt != now {
		t.Fatalf("run = %+v", run)
	}
	if run.DurationMS < 0 {
		t.Fatalf("duration = %d", run.DurationMS)
	}
	if ran.LastError != "" {
		t.Fatalf("LastError = %q", ran.LastError)
	}
	for _, id := range []string{"future", "disabled", "unscheduled"} {
		task, _ := store.Get(id)
		if len(task.RunHistory) != 0 || !task.LastRun.IsZero() {
			t.Fatalf("task %s ran unexpectedly: %+v", id, task)
		}
	}
}

func TestOneShotScheduleDisablesAfterRun(t *testing.T) {
	clock := newTestClock(time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC))
	store, _ := Open(t.TempDir())
	now := clock.Now()
	task := dueTask("once", now)
	task.Schedule = Schedule("at 2026-01-02T10:00:00Z")
	mustCreateTask(t, store, task)

	recorder := newSchedulerRecorder()
	scheduler := newScheduledScheduler(store, clock, recorder)
	scheduler.Next = func(Task, time.Time) (time.Time, bool) { return time.Time{}, false }
	if err := scheduler.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	ran, _ := store.Get("once")
	if ran.Enabled || !ran.NextRun.IsZero() {
		t.Fatalf("one-shot after run = enabled %v next %s, want disabled/zero", ran.Enabled, ran.NextRun)
	}
	if len(ran.RunHistory) != 1 || ran.RunHistory[0].Status != StatusSuccess {
		t.Fatalf("history = %+v", ran.RunHistory)
	}
	clock.Advance(2 * time.Hour)
	if err := scheduler.Tick(context.Background()); err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	if deliver, _ := recorder.counts(); deliver != 1 {
		t.Fatalf("deliveries = %d, want exactly one", deliver)
	}
}

func TestDeliveredThenSettled(t *testing.T) {
	clock := newTestClock(time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC))
	dir := t.TempDir()
	store, _ := Open(dir)
	now := clock.Now()
	mustCreateTask(t, store, dueTask("t1", now))

	recorder := newSchedulerRecorder()
	recorder.setSettle(false, "", "")
	scheduler := newScheduledScheduler(store, clock, recorder)
	if err := scheduler.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	delivered, _ := store.Get("t1")
	if len(delivered.RunHistory) != 1 {
		t.Fatalf("history = %+v", delivered.RunHistory)
	}
	record := delivered.RunHistory[0]
	if record.Status != StatusDelivered || !record.CompletedAt.IsZero() ||
		record.DeliveredAt != now || record.DeliveryID != "delivery-1" {
		t.Fatalf("delivered record = %+v", record)
	}
	if delivered.LastError != "" {
		t.Fatalf("delivered LastError = %q", delivered.LastError)
	}

	recorder.setSettle(true, StatusSuccess, "")
	clock.Advance(time.Minute)
	if err := scheduler.Tick(context.Background()); err != nil {
		t.Fatalf("settle Tick: %v", err)
	}
	if deliver, _ := recorder.counts(); deliver != 1 {
		t.Fatalf("settlement re-delivered the task: %d deliveries", deliver)
	}
	settled, _ := store.Get("t1")
	if len(settled.RunHistory) != 1 {
		t.Fatalf("history after settlement = %+v", settled.RunHistory)
	}
	record = settled.RunHistory[0]
	if record.Status != StatusSuccess || record.CompletedAt != clock.Now() {
		t.Fatalf("settled record = %+v", record)
	}
	if record.DurationMS != time.Minute.Milliseconds() {
		t.Fatalf("duration = %d, want %d", record.DurationMS, time.Minute.Milliseconds())
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	persisted, _ := reopened.Get("t1")
	if len(persisted.RunHistory) != 1 || persisted.RunHistory[0].Status != StatusSuccess {
		t.Fatalf("persisted run = %+v", persisted.RunHistory)
	}
}

func TestDeliveryErrorRecordsFailureAndReschedules(t *testing.T) {
	clock := newTestClock(time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC))
	store, _ := Open(t.TempDir())
	now := clock.Now()
	mustCreateTask(t, store, dueTask("t1", now))

	recorder := newSchedulerRecorder()
	recorder.setDeliverError(errors.New("chat exploded"))
	scheduler := newScheduledScheduler(store, clock, recorder)
	if err := scheduler.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	failed, _ := store.Get("t1")
	if len(failed.RunHistory) != 1 {
		t.Fatalf("history = %+v", failed.RunHistory)
	}
	record := failed.RunHistory[0]
	if record.Status != StatusFailure || record.Error == "" || record.DeliveryID != "" {
		t.Fatalf("failed record = %+v", record)
	}
	if failed.LastError == "" || failed.LastRun != now || !failed.NextRun.Equal(now.Add(time.Hour)) || !failed.Enabled {
		t.Fatalf("failed task = %+v", failed)
	}

	recorder.setDeliverError(nil)
	clock.Advance(time.Hour)
	if err := scheduler.Tick(context.Background()); err != nil {
		t.Fatalf("retry Tick: %v", err)
	}
	recovered, _ := store.Get("t1")
	if len(recovered.RunHistory) != 2 {
		t.Fatalf("history after retry = %+v", recovered.RunHistory)
	}
	if recovered.RunHistory[0].Status != StatusSuccess || recovered.RunHistory[1].Status != StatusFailure {
		t.Fatalf("history order = %+v", recovered.RunHistory)
	}
	if recovered.LastError != "" {
		t.Fatalf("LastError not cleared: %q", recovered.LastError)
	}
}

func TestRestartRecoversPersistedDeliveredRun(t *testing.T) {
	clock := newTestClock(time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC))
	dir := t.TempDir()
	store, _ := Open(dir)
	now := clock.Now()
	task := dueTask("old", now)
	task.NextRun = now.Add(time.Hour)
	task.RunHistory = []Run{{
		ID:          "run-old",
		StartedAt:   now.Add(-5 * time.Minute),
		DeliveredAt: now.Add(-4 * time.Minute),
		Status:      StatusDelivered,
		DeliveryID:  "delivery-old",
	}}
	mustCreateTask(t, store, task)

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	recorder := newSchedulerRecorder()
	scheduler := newScheduledScheduler(reopened, clock, recorder)
	if err := scheduler.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if deliver, settle := recorder.counts(); deliver != 0 || settle != 1 {
		t.Fatalf("counts = %d/%d, want 0 deliveries / 1 settlement", deliver, settle)
	}
	settled, _ := reopened.Get("old")
	record := settled.RunHistory[0]
	if record.Status != StatusSuccess || record.CompletedAt != now {
		t.Fatalf("settled record = %+v", record)
	}
	if record.DurationMS != (4 * time.Minute).Milliseconds() {
		t.Fatalf("duration = %d, want 4m", record.DurationMS)
	}
}

func TestRestartKeepsUnknownOutcomePending(t *testing.T) {
	clock := newTestClock(time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC))
	store, _ := Open(t.TempDir())
	now := clock.Now()
	task := dueTask("t1", now)
	task.NextRun = now.Add(time.Hour)
	task.RunHistory = []Run{{
		ID: "run-old", StartedAt: now.Add(-time.Minute), DeliveredAt: now,
		Status: StatusDelivered, DeliveryID: "delivery-old",
	}}
	mustCreateTask(t, store, task)

	recorder := newSchedulerRecorder()
	recorder.setSettle(false, "", "")
	scheduler := newScheduledScheduler(store, clock, recorder)
	for range 2 {
		if err := scheduler.Tick(context.Background()); err != nil {
			t.Fatalf("Tick: %v", err)
		}
	}
	kept, _ := store.Get("t1")
	if kept.RunHistory[0].Status != StatusDelivered || !kept.RunHistory[0].CompletedAt.IsZero() {
		t.Fatalf("unknown outcome was settled: %+v", kept.RunHistory[0])
	}
	if _, settle := recorder.counts(); settle != 2 {
		t.Fatalf("settle calls = %d, want a retry on the second tick", settle)
	}
}

func TestSettleFailureAndInterruptionUpdateTask(t *testing.T) {
	clock := newTestClock(time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC))
	store, _ := Open(t.TempDir())
	now := clock.Now()
	failing := dueTask("failing", now)
	failing.NextRun = now.Add(time.Hour)
	failing.RunHistory = []Run{{ID: "run-fail", StartedAt: now, DeliveredAt: now, Status: StatusDelivered, DeliveryID: "d-fail"}}
	mustCreateTask(t, store, failing)

	recorder := newSchedulerRecorder()
	recorder.setSettle(true, StatusFailure, "provider exploded")
	scheduler := newScheduledScheduler(store, clock, recorder)
	if err := scheduler.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	failed, _ := store.Get("failing")
	if failed.RunHistory[0].Status != StatusFailure || failed.LastError != "provider exploded" {
		t.Fatalf("failure settle = %+v / %q", failed.RunHistory[0], failed.LastError)
	}

	interrupted := dueTask("interrupted", now)
	interrupted.NextRun = now.Add(time.Hour)
	interrupted.RunHistory = []Run{{ID: "run-stop", StartedAt: now, DeliveredAt: now, Status: StatusDelivered, DeliveryID: "d-stop"}}
	mustCreateTask(t, store, interrupted)
	recorder.setSettle(true, StatusInterrupted, "stopped by user")
	if err := scheduler.Tick(context.Background()); err != nil {
		t.Fatalf("Tick interrupted: %v", err)
	}
	stopped, _ := store.Get("interrupted")
	if stopped.RunHistory[0].Status != StatusInterrupted || stopped.LastError != "" {
		t.Fatalf("interrupted settle = %+v / %q", stopped.RunHistory[0], stopped.LastError)
	}
}

func TestTickFoldsTriggerWhenRunStillPending(t *testing.T) {
	clock := newTestClock(time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC))
	store, _ := Open(t.TempDir())
	now := clock.Now()
	mustCreateTask(t, store, dueTask("t1", now))

	recorder := newSchedulerRecorder()
	recorder.setSettle(false, "", "")
	scheduler := newScheduledScheduler(store, clock, recorder)
	// Keep the task due after each delivery so a second tick would trigger it.
	scheduler.Next = func(_ Task, after time.Time) (time.Time, bool) { return after.Add(-time.Minute), true }
	if err := scheduler.Tick(context.Background()); err != nil {
		t.Fatalf("first Tick: %v", err)
	}
	if deliver, _ := recorder.counts(); deliver != 1 {
		t.Fatalf("deliveries = %d, want 1", deliver)
	}
	if err := scheduler.Tick(context.Background()); err != nil {
		t.Fatalf("fold Tick: %v", err)
	}
	deliver, settle := recorder.counts()
	if deliver != 1 {
		t.Fatalf("pending run was duplicated: %d deliveries", deliver)
	}
	if settle != 2 {
		t.Fatalf("settle calls = %d, want the pending settle retried", settle)
	}
	task, _ := store.Get("t1")
	if len(task.RunHistory) != 1 || task.RunHistory[0].Status != StatusDelivered {
		t.Fatalf("history = %+v", task.RunHistory)
	}
}

func TestBoundedConcurrency(t *testing.T) {
	clock := newTestClock(time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC))
	store, _ := Open(t.TempDir())
	now := clock.Now()
	for index := 0; index < 5; index++ {
		mustCreateTask(t, store, dueTask(fmt.Sprintf("t%d", index), now))
	}

	var mu sync.Mutex
	active, maxActive, calls := 0, 0, 0
	started := make(chan string, 8)
	release := make(chan struct{})
	deliver := func(ctx context.Context, task Task) (string, error) {
		mu.Lock()
		active++
		calls++
		if active > maxActive {
			maxActive = active
		}
		mu.Unlock()
		started <- task.ID
		select {
		case <-release:
		case <-ctx.Done():
			mu.Lock()
			active--
			mu.Unlock()
			return "", ctx.Err()
		}
		mu.Lock()
		active--
		mu.Unlock()
		return "delivery-" + task.ID, nil
	}
	scheduler := &Scheduler{
		Store:         store,
		Now:           clock.Now,
		Deliver:       deliver,
		Settle:        newSchedulerRecorder().Settle,
		Next:          func(_ Task, after time.Time) (time.Time, bool) { return after.Add(time.Hour), true },
		MaxConcurrent: 2,
		Interval:      time.Hour,
	}
	done := make(chan error, 1)
	go func() { done <- scheduler.Tick(context.Background()) }()
	for index := 0; index < 2; index++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d deliveries started", index)
		}
	}
	select {
	case id := <-started:
		t.Fatalf("a third delivery started while two were in flight: %s", id)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Tick: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Tick did not finish")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 5 {
		t.Fatalf("deliveries = %d, want all 5", calls)
	}
	if maxActive != 2 {
		t.Fatalf("max concurrent deliveries = %d, want 2", maxActive)
	}
}

func TestDeleteAndCancelPreventRuns(t *testing.T) {
	clock := newTestClock(time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC))
	store, _ := Open(t.TempDir())
	now := clock.Now()
	mustCreateTask(t, store, dueTask("deleted", now))
	mustCreateTask(t, store, dueTask("cancelled", now))
	recorder := newSchedulerRecorder()
	scheduler := newScheduledScheduler(store, clock, recorder)
	ctx := context.Background()

	if err := scheduler.Delete(ctx, "deleted"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := store.Get("deleted"); ok {
		t.Fatal("Delete kept the task")
	}
	if err := scheduler.Delete(ctx, "missing"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("Delete(missing) = %v, want ErrTaskNotFound", err)
	}
	if err := scheduler.Cancel(ctx, "cancelled"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	cancelled, _ := store.Get("cancelled")
	if cancelled.Enabled || !cancelled.NextRun.IsZero() {
		t.Fatalf("cancelled task = %+v, want disabled/zero next", cancelled)
	}
	if err := scheduler.Cancel(ctx, "missing"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("Cancel(missing) = %v, want ErrTaskNotFound", err)
	}
	if err := scheduler.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if deliver, _ := recorder.counts(); deliver != 0 {
		t.Fatalf("deleted/cancelled tasks delivered %d times", deliver)
	}
}

func TestDeleteDropsPendingSettlement(t *testing.T) {
	clock := newTestClock(time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC))
	store, _ := Open(t.TempDir())
	now := clock.Now()
	mustCreateTask(t, store, dueTask("t1", now))
	recorder := newSchedulerRecorder()
	recorder.setSettle(false, "", "")
	scheduler := newScheduledScheduler(store, clock, recorder)
	if err := scheduler.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if _, settle := recorder.counts(); settle != 1 {
		t.Fatalf("settle calls = %d, want 1", settle)
	}
	if err := scheduler.Delete(context.Background(), "t1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := scheduler.Tick(context.Background()); err != nil {
		t.Fatalf("Tick after delete: %v", err)
	}
	if _, settle := recorder.counts(); settle != 1 {
		t.Fatalf("settled a deleted task's run: %d settle calls", settle)
	}
}

func TestRunNowForcesDisabledTaskAndFoldsPending(t *testing.T) {
	clock := newTestClock(time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC))
	store, _ := Open(t.TempDir())
	now := clock.Now()
	task := dueTask("t1", now)
	task.Enabled = false
	task.NextRun = now.Add(time.Hour)
	mustCreateTask(t, store, task)

	recorder := newSchedulerRecorder()
	scheduler := newScheduledScheduler(store, clock, recorder)
	if err := scheduler.RunNow(context.Background(), "t1"); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	if deliver, _ := recorder.counts(); deliver != 1 {
		t.Fatalf("deliveries = %d, want 1", deliver)
	}
	ran, _ := store.Get("t1")
	if ran.Enabled || !ran.NextRun.IsZero() || ran.LastRun != now {
		t.Fatalf("forced run task = %+v, want still disabled/zero next", ran)
	}
	if len(ran.RunHistory) != 1 || ran.RunHistory[0].Status != StatusSuccess {
		t.Fatalf("history = %+v", ran.RunHistory)
	}
	if err := scheduler.RunNow(context.Background(), "missing"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("RunNow(missing) = %v, want ErrTaskNotFound", err)
	}

	// A second RunNow while the delivered run is pending is folded.
	recorder.setSettle(false, "", "")
	pendingTask := dueTask("t2", now)
	pendingTask.NextRun = now.Add(time.Hour)
	mustCreateTask(t, store, pendingTask)
	if err := scheduler.RunNow(context.Background(), "t2"); err != nil {
		t.Fatalf("RunNow pending #1: %v", err)
	}
	if err := scheduler.RunNow(context.Background(), "t2"); err != nil {
		t.Fatalf("RunNow pending #2: %v", err)
	}
	if deliver, settle := recorder.counts(); deliver != 2 || settle < 3 {
		t.Fatalf("counts = %d/%d, want the pending trigger folded", deliver, settle)
	}
}

func TestRunHistoryIsCappedNewestFirst(t *testing.T) {
	clock := newTestClock(time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC))
	store, _ := Open(t.TempDir())
	now := clock.Now()
	task := dueTask("t1", now)
	task.Enabled = false
	task.NextRun = now.Add(time.Hour)
	mustCreateTask(t, store, task)
	recorder := newSchedulerRecorder()
	scheduler := newScheduledScheduler(store, clock, recorder)
	for index := 0; index < MaxRunHistory+2; index++ {
		if err := scheduler.RunNow(context.Background(), "t1"); err != nil {
			t.Fatalf("RunNow #%d: %v", index, err)
		}
		clock.Advance(time.Second)
	}
	ran, _ := store.Get("t1")
	if len(ran.RunHistory) != MaxRunHistory {
		t.Fatalf("history length = %d, want %d", len(ran.RunHistory), MaxRunHistory)
	}
	if ran.RunHistory[0].StartedAt != clock.Now().Add(-time.Second) {
		t.Fatalf("newest run StartedAt = %s, want the last run", ran.RunHistory[0].StartedAt)
	}
	seen := map[string]bool{}
	for _, run := range ran.RunHistory {
		if run.ID == "" || seen[run.ID] {
			t.Fatalf("run id missing or duplicated: %+v", ran.RunHistory)
		}
		seen[run.ID] = true
	}
}

func TestStartStopStopsTicks(t *testing.T) {
	clock := newTestClock(time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC))
	store, _ := Open(t.TempDir())
	mustCreateTask(t, store, dueTask("t1", clock.Now()))

	var mu sync.Mutex
	delivered := 0
	deliveredCh := make(chan struct{}, 4)
	deliver := func(context.Context, Task) (string, error) {
		mu.Lock()
		delivered++
		mu.Unlock()
		deliveredCh <- struct{}{}
		return "delivery", nil
	}
	scheduler := &Scheduler{
		Store:         store,
		Now:           clock.Now,
		Deliver:       deliver,
		Settle:        newSchedulerRecorder().Settle,
		Next:          func(_ Task, after time.Time) (time.Time, bool) { return after.Add(time.Hour), true },
		MaxConcurrent: 1,
		Interval:      5 * time.Millisecond,
	}
	if err := scheduler.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-deliveredCh:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not deliver the due task")
	}
	scheduler.Stop()
	mu.Lock()
	after := delivered
	mu.Unlock()
	time.Sleep(40 * time.Millisecond)
	mu.Lock()
	late := delivered
	mu.Unlock()
	if late != after {
		t.Fatalf("ticker kept delivering after Stop: %d -> %d", after, late)
	}
	if err := scheduler.Tick(context.Background()); !errors.Is(err, ErrSchedulerClosed) {
		t.Fatalf("Tick after Stop = %v, want ErrSchedulerClosed", err)
	}
	if err := scheduler.RunNow(context.Background(), "t1"); !errors.Is(err, ErrSchedulerClosed) {
		t.Fatalf("RunNow after Stop = %v, want ErrSchedulerClosed", err)
	}
	if err := scheduler.Start(); !errors.Is(err, ErrSchedulerClosed) {
		t.Fatalf("Start after Stop = %v, want ErrSchedulerClosed", err)
	}
}

func TestSchedulerValidationAndCancellation(t *testing.T) {
	if err := (&Scheduler{}).Tick(context.Background()); !errors.Is(err, ErrStoreRequired) {
		t.Fatalf("nil-store Tick = %v, want ErrStoreRequired", err)
	}
	store, _ := Open(t.TempDir())
	withoutDeliver := &Scheduler{Store: store}
	if err := withoutDeliver.Tick(context.Background()); !errors.Is(err, ErrDeliverRequired) {
		t.Fatalf("nil-deliver Tick = %v, want ErrDeliverRequired", err)
	}
	recorder := newSchedulerRecorder()
	clock := newTestClock(time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC))
	scheduler := newScheduledScheduler(store, clock, recorder)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := scheduler.Tick(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Tick = %v, want context.Canceled", err)
	}
	if err := scheduler.Tick(context.TODO()); err != nil { //nolint:staticcheck
		t.Fatalf("Tick(nil ctx): %v", err)
	}
}

func TestConcurrentRunNowDoesNotDuplicateHistory(t *testing.T) {
	clock := newTestClock(time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC))
	store, _ := Open(t.TempDir())
	now := clock.Now()
	mustCreateTask(t, store, dueTask("t1", now))
	recorder := newSchedulerRecorder()
	scheduler := newScheduledScheduler(store, clock, recorder)
	var wg sync.WaitGroup
	for index := 0; index < 8; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := scheduler.RunNow(context.Background(), "t1"); err != nil {
				t.Errorf("RunNow: %v", err)
			}
		}()
	}
	wg.Wait()
	task, _ := store.Get("t1")
	if len(task.RunHistory) == 0 || len(task.RunHistory) > MaxRunHistory {
		t.Fatalf("history = %+v", task.RunHistory)
	}
	deliver, _ := recorder.counts()
	if deliver > 8 {
		t.Fatalf("deliveries = %d", deliver)
	}
}

func TestSettleDoesNotDispatchDueTasks(t *testing.T) {
	clock := newTestClock(time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC))
	store, _ := Open(t.TempDir())
	now := clock.Now()
	task := dueTask("t1", now) // enabled and due
	task.RunHistory = []Run{{
		ID: "run-old", StartedAt: now.Add(-time.Minute), DeliveredAt: now,
		Status: StatusDelivered, DeliveryID: "delivery-old",
	}}
	mustCreateTask(t, store, task)

	recorder := newSchedulerRecorder()
	scheduler := newScheduledScheduler(store, clock, recorder)
	if err := scheduler.SettlePending(context.Background()); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	deliver, settle := recorder.counts()
	if deliver != 0 || settle != 1 {
		t.Fatalf("counts = %d/%d, want 0 deliveries / 1 settlement", deliver, settle)
	}
	settled, _ := store.Get("t1")
	if settled.RunHistory[0].Status != StatusSuccess || !settled.RunHistory[0].CompletedAt.Equal(now) {
		t.Fatalf("settled record = %+v", settled.RunHistory[0])
	}
	// The due task is still due: the next scheduling pass delivers it.
	if err := scheduler.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if deliver, _ := recorder.counts(); deliver != 1 {
		t.Fatalf("Tick deliveries = %d, want 1", deliver)
	}
}
