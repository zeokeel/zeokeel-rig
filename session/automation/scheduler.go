package automation

// The scheduler state machine: claim a due task, deliver it through the host
// callback, persist the delivered run, then settle it (immediately when the host
// already knows the outcome, otherwise on a later tick). One task is never
// delivered twice concurrently, at most MaxConcurrent deliveries run at once,
// and a delivered run with an unknown outcome survives restarts.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// DeliverFunc delivers one task through the host (chat binding, prompt send,
// child session creation, ...). It returns the host's delivery identifier, which
// the Settle callback receives later. An error is recorded as a failed run.
type DeliverFunc func(ctx context.Context, task Task) (deliveryID string, err error)

// SettleFunc reports one delivered run's outcome. settled=false means the run is
// still in flight (or its outcome is unknown); the scheduler retries it on a
// later tick. A settled run must report one of StatusSuccess, StatusFailure or
// StatusInterrupted; other values are treated as still pending.
type SettleFunc func(ctx context.Context, task Task, deliveryID string) (status string, detail string, settled bool)

// NextFunc computes the next firing of a repeating schedule strictly after the
// given instant. ok=false means the schedule has no future occurrence (a
// one-shot schedule or an exhausted rule), so the task is disabled after this
// run. The host owns schedule parsing; a nil Next makes every task one-shot.
type NextFunc func(task Task, after time.Time) (next time.Time, ok bool)

// Scheduler is the durable scheduler core. Configure the exported fields before
// Start/Tick/RunNow; the zero value is not usable without a Store and Deliver.
//
// The scheduler is safe for concurrent use.
type Scheduler struct {
	// Store is the durable registry (required).
	Store *Store
	// Now is the clock (nil = time.Now).
	Now func() time.Time
	// Deliver delivers one task (required).
	Deliver DeliverFunc
	// Settle resolves a delivered run's outcome (nil leaves runs delivered).
	Settle SettleFunc
	// Next evaluates a repeating schedule after a run (nil = one-shot tasks).
	Next NextFunc
	// MaxConcurrent bounds simultaneous deliveries (0 = DefaultMaxConcurrent).
	MaxConcurrent int
	// Interval is the Start ticker interval (0 = DefaultInterval).
	Interval time.Duration
	// Logger receives warnings (nil discards them).
	Logger func(format string, args ...any)

	mu       sync.Mutex
	inFlight map[string]bool
	pending  map[string]*pendingRun
	settling map[string]bool
	sem      chan struct{}
	stop     chan struct{}
	done     chan struct{}
	started  bool
	closed   bool
	cancel   context.CancelFunc
	runSeq   uint64
}

// pendingRun is one delivered run awaiting settlement.
type pendingRun struct {
	taskID      string
	deliveryID  string
	startedAt   time.Time
	deliveredAt time.Time
}

// NewScheduler returns a scheduler over store with defaults; set the callbacks
// (and optionally Next/MaxConcurrent/Interval) before Start or Tick.
func NewScheduler(store *Store) *Scheduler {
	return &Scheduler{Store: store}
}

// Start launches the ticker goroutine. It is idempotent; a stopped scheduler
// reports ErrSchedulerClosed.
func (s *Scheduler) Start() error {
	if err := s.validate(); err != nil {
		return err
	}
	s.ensure()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrSchedulerClosed
	}
	if s.started {
		s.mu.Unlock()
		return nil
	}
	s.started = true
	interval := s.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	stop := s.stop
	done := s.done
	s.mu.Unlock()

	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				_ = s.Tick(ctx)
			}
		}
	}()
	return nil
}

// Stop cancels the ticker, waits for the current tick and returns. It is
// idempotent; no new run is started afterwards.
func (s *Scheduler) Stop() {
	s.ensure()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	started := s.started
	cancel := s.cancel
	stop := s.stop
	done := s.done
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if started {
		close(stop)
		<-done
	}
}

// Tick runs one scheduling pass: settle delivered runs whose outcome is now
// known, then deliver every due enabled task (up to MaxConcurrent at once) and
// wait for those deliveries. It returns a structural error only (missing
// store/deliver, canceled context, closed scheduler); delivery and settlement
// failures are recorded in the registry.
func (s *Scheduler) Tick(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.validate(); err != nil {
		return err
	}
	s.ensure()
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return ErrSchedulerClosed
	}
	s.recoverPending()
	if err := s.settlePending(ctx); err != nil {
		return err
	}
	due, err := s.dueTaskIDs(s.now())
	if err != nil {
		return err
	}
	var wait sync.WaitGroup
	for _, id := range due {
		wait.Add(1)
		go func(id string) {
			defer wait.Done()
			_ = s.executeBounded(ctx, id, false)
		}(id)
	}
	wait.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

// SettlePending retries every pending delivered run without dispatching due
// tasks. It is the restart/chat-change reconciliation path; Tick is the
// scheduling pass.
func (s *Scheduler) SettlePending(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.validate(); err != nil {
		return err
	}
	s.ensure()
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return ErrSchedulerClosed
	}
	s.recoverPending()
	return s.settlePending(ctx)
}

// RunNow delivers one task immediately, even when it is disabled or not due. It
// is synchronous and folds the trigger when the task's previous delivered run
// is still pending. Unknown IDs report ErrTaskNotFound; a stopped scheduler
// reports ErrSchedulerClosed.
func (s *Scheduler) RunNow(ctx context.Context, id string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.validate(); err != nil {
		return err
	}
	s.ensure()
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return ErrSchedulerClosed
	}
	id = strings.TrimSpace(id)
	if _, ok := s.Store.Get(id); !ok {
		return fmt.Errorf("%w: %q", ErrTaskNotFound, id)
	}
	s.recoverPending()
	if err := s.settlePending(ctx); err != nil {
		return err
	}
	return s.executeBounded(ctx, id, true)
}

// Delete removes a task and forgets its pending settlement. The task is not
// delivered again. Unknown IDs report ErrTaskNotFound.
func (s *Scheduler) Delete(ctx context.Context, id string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	id = strings.TrimSpace(id)
	if _, ok := s.Store.Get(id); !ok {
		return fmt.Errorf("%w: %q", ErrTaskNotFound, id)
	}
	if err := s.Store.Delete(id); err != nil {
		return err
	}
	s.mu.Lock()
	for runID, run := range s.pending {
		if run.taskID == id {
			delete(s.pending, runID)
		}
	}
	s.mu.Unlock()
	return nil
}

// Cancel disables a task and clears its next run, keeping its history. An
// already-running delivery is not interrupted.
func (s *Scheduler) Cancel(ctx context.Context, id string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	id = strings.TrimSpace(id)
	task, ok := s.Store.Get(id)
	if !ok {
		return fmt.Errorf("%w: %q", ErrTaskNotFound, id)
	}
	now := s.now()
	task.Enabled = false
	task.NextRun = time.Time{}
	task.UpdatedAt = now
	_, err := s.Store.Update(task)
	return err
}

// validate reports missing required configuration.
func (s *Scheduler) validate() error {
	if s == nil || s.Store == nil {
		return ErrStoreRequired
	}
	if s.Deliver == nil {
		return ErrDeliverRequired
	}
	return nil
}

// ensure lazily initializes the scheduler's runtime state.
func (s *Scheduler) ensure() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlight == nil {
		s.inFlight = make(map[string]bool)
	}
	if s.pending == nil {
		s.pending = make(map[string]*pendingRun)
	}
	if s.settling == nil {
		s.settling = make(map[string]bool)
	}
	if s.sem == nil {
		capacity := s.MaxConcurrent
		if capacity <= 0 {
			capacity = DefaultMaxConcurrent
		}
		s.sem = make(chan struct{}, capacity)
	}
	if s.stop == nil {
		s.stop = make(chan struct{})
	}
	if s.done == nil {
		s.done = make(chan struct{})
	}
}

// now returns the scheduler clock.
func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// logf logs a warning when a logger is configured.
func (s *Scheduler) logf(format string, args ...any) {
	if s.Logger != nil {
		s.Logger(format, args...)
	}
}

// executeBounded acquires a delivery slot, runs one task and releases it.
func (s *Scheduler) executeBounded(ctx context.Context, id string, force bool) error {
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-s.sem }()
	s.execute(ctx, id, force)
	return nil
}

// execute performs one delivery attempt for one task.
func (s *Scheduler) execute(ctx context.Context, id string, force bool) {
	task, ok := s.claim(id, force)
	if !ok {
		return
	}
	defer s.release(id)

	// A trigger while the previous delivered run is still unresolved is folded:
	// the ticker's settlement pass already had its chance this tick.
	if s.hasPendingTask(id) {
		s.logf("automation: task %s still has a run in flight; folding this trigger", id)
		return
	}

	started := s.now()
	deliveryID, deliveryErr := s.Deliver(ctx, task)
	now := s.now()
	current, ok := s.Store.Get(id)
	if !ok {
		// The task was deleted while the delivery was in flight.
		return
	}

	run := Run{ID: s.newRunID(), StartedAt: started}
	if deliveryErr != nil {
		run.Status = StatusFailure
		run.CompletedAt = now
		run.Error = truncateRunSummary(deliveryErr.Error())
		run.DurationMS = runDuration(now, time.Time{}, started)
		current.LastError = run.Error
	} else {
		run.DeliveredAt = now
		run.Status = StatusDelivered
		run.DeliveryID = deliveryID
		status, detail, settled := "", "", false
		if s.Settle != nil {
			status, detail, settled = s.Settle(ctx, task, deliveryID)
		}
		if settled && terminalStatus(status) {
			run.Status = status
			run.CompletedAt = s.now()
			run.Error = truncateRunSummary(detail)
			run.DurationMS = runDuration(run.CompletedAt, run.DeliveredAt, started)
			if status == StatusFailure {
				current.LastError = run.Error
			} else {
				current.LastError = ""
			}
		} else {
			current.LastError = ""
			s.mu.Lock()
			s.pending[run.ID] = &pendingRun{
				taskID:      id,
				deliveryID:  deliveryID,
				startedAt:   started,
				deliveredAt: now,
			}
			s.mu.Unlock()
		}
	}

	current.LastRun = now
	current.UpdatedAt = now
	current.RunHistory = append([]Run{run}, current.RunHistory...)
	if len(current.RunHistory) > MaxRunHistory {
		current.RunHistory = current.RunHistory[:MaxRunHistory]
	}
	current.NextRun, current.Enabled = s.nextRun(current, now)
	if _, err := s.Store.Update(current); err != nil {
		s.logf("automation: persist run of task %s: %v", id, err)
	}
}

// claim takes the in-flight guard for one execution and snapshots the task.
func (s *Scheduler) claim(id string, force bool) (Task, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlight[id] {
		return Task{}, false
	}
	task, ok := s.Store.Get(id)
	if !ok {
		return Task{}, false
	}
	if !force && !task.Enabled {
		return Task{}, false
	}
	s.inFlight[id] = true
	return task, true
}

// release clears the in-flight guard.
func (s *Scheduler) release(id string) {
	s.mu.Lock()
	delete(s.inFlight, id)
	s.mu.Unlock()
}

// dueTaskIDs returns the enabled, due, not-in-flight tasks ordered by NextRun
// then ID.
func (s *Scheduler) dueTaskIDs(now time.Time) ([]string, error) {
	tasks, err := s.Store.List()
	if err != nil {
		return nil, err
	}
	type candidate struct {
		id   string
		next time.Time
	}
	s.mu.Lock()
	due := make([]candidate, 0, len(tasks))
	for _, task := range tasks {
		if !task.Enabled || task.NextRun.IsZero() || task.NextRun.After(now) || s.inFlight[task.ID] {
			continue
		}
		due = append(due, candidate{id: task.ID, next: task.NextRun})
	}
	s.mu.Unlock()
	sort.SliceStable(due, func(left, right int) bool {
		if !due[left].next.Equal(due[right].next) {
			return due[left].next.Before(due[right].next)
		}
		return due[left].id < due[right].id
	})
	ids := make([]string, len(due))
	for index, entry := range due {
		ids[index] = entry.id
	}
	return ids, nil
}

// nextRun computes the task's next occurrence after one run. A disabled task or
// a schedule without a future occurrence ends with a zero NextRun and a false
// enabled state.
func (s *Scheduler) nextRun(task Task, after time.Time) (time.Time, bool) {
	if !task.Enabled || s.Next == nil {
		return time.Time{}, false
	}
	next, ok := s.Next(task, after)
	if !ok || next.IsZero() {
		return time.Time{}, false
	}
	return next, true
}

// hasPendingTask reports whether the task has a delivered run awaiting
// settlement.
func (s *Scheduler) hasPendingTask(taskID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, run := range s.pending {
		if run.taskID == taskID {
			return true
		}
	}
	return false
}

// recoverPending rebuilds the in-memory pending set from the durable run
// history: a delivered run with a delivery id survived a restart and still
// needs settlement.
func (s *Scheduler) recoverPending() {
	tasks, err := s.Store.List()
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, task := range tasks {
		for _, run := range task.RunHistory {
			if run.ID == "" || run.Status != StatusDelivered || run.DeliveryID == "" {
				continue
			}
			if _, ok := s.pending[run.ID]; ok {
				continue
			}
			s.pending[run.ID] = &pendingRun{
				taskID:      task.ID,
				deliveryID:  run.DeliveryID,
				startedAt:   run.StartedAt,
				deliveredAt: run.DeliveredAt,
			}
		}
	}
}

// settlePending attempts to settle every pending delivered run.
func (s *Scheduler) settlePending(ctx context.Context) error {
	s.mu.Lock()
	ids := make([]string, 0, len(s.pending))
	for id := range s.pending {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	sort.Strings(ids)
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.settleRun(ctx, id)
	}
	return nil
}

// settleRun settles one delivered run (idempotent; a run already being settled
// by another goroutine is skipped).
func (s *Scheduler) settleRun(ctx context.Context, runID string) {
	s.mu.Lock()
	run := s.pending[runID]
	if run == nil || s.settling[runID] {
		s.mu.Unlock()
		return
	}
	s.settling[runID] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.settling, runID)
		s.mu.Unlock()
	}()

	task, ok := s.Store.Get(run.taskID)
	if !ok {
		// The task was deleted: its pending settlement is moot.
		s.mu.Lock()
		delete(s.pending, runID)
		s.mu.Unlock()
		return
	}
	if s.Settle == nil {
		return
	}
	status, detail, settled := s.Settle(ctx, task, run.deliveryID)
	if !settled || !terminalStatus(status) {
		return
	}
	now := s.now()
	updated := task
	found := false
	for index := range updated.RunHistory {
		record := &updated.RunHistory[index]
		if record.ID != runID || record.Status != StatusDelivered {
			continue
		}
		record.Status = status
		record.Error = truncateRunSummary(detail)
		record.CompletedAt = now
		record.DurationMS = runDuration(now, record.DeliveredAt, record.StartedAt)
		found = true
		if index == 0 {
			if status == StatusFailure {
				updated.LastError = record.Error
			} else {
				updated.LastError = ""
			}
		}
		break
	}
	if !found {
		s.mu.Lock()
		delete(s.pending, runID)
		s.mu.Unlock()
		return
	}
	updated.UpdatedAt = now
	if _, err := s.Store.Update(updated); err != nil {
		s.logf("automation: persist settled run %s: %v", runID, err)
		return
	}
	s.mu.Lock()
	delete(s.pending, runID)
	s.mu.Unlock()
}

// newRunID allocates a stable run-record ID.
func (s *Scheduler) newRunID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runSeq++
	return fmt.Sprintf("run-%d-%d", time.Now().UTC().UnixNano(), s.runSeq)
}

// terminalStatus reports whether status is a terminal run state.
func terminalStatus(status string) bool {
	switch status {
	case StatusSuccess, StatusFailure, StatusInterrupted:
		return true
	default:
		return false
	}
}

// runDuration returns the delivery-to-completion duration in milliseconds, or
// the attempt duration when no delivery instant was recorded.
func runDuration(completedAt, deliveredAt, startedAt time.Time) int64 {
	if !deliveredAt.IsZero() {
		return completedAt.Sub(deliveredAt).Milliseconds()
	}
	if !startedAt.IsZero() {
		return completedAt.Sub(startedAt).Milliseconds()
	}
	return 0
}

// truncateRunSummary bounds one run error/detail to 200 runes.
func truncateRunSummary(summary string) string {
	const limit = 200
	runes := []rune(summary)
	if len(runes) <= limit {
		return summary
	}
	return string(runes[:limit-1]) + "…"
}
