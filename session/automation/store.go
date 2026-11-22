// Package automation provides a host-agnostic durable scheduled-task registry
// and a bounded scheduler loop.
//
// A Store persists a registry document (default automations.json) with atomic
// writes: a temporary file is written, synced and renamed over the registry, so
// a crash never leaves a half-written document. A missing file is an empty
// registry; an unreadable or corrupt one is quarantined (renamed to
// <path>.corrupt-<timestamp>) and the store starts empty instead of failing.
//
// A Scheduler claims due tasks from the Store, delivers them through a host
// callback, records the delivery and later settles it through a second host
// callback. The host owns schedule evaluation (the Next callback), conversation
// binding, and what a delivery/settlement actually means; this package owns the
// durable state machine:
//
//	delivered -> success | error | interrupted
//
// A delivered run whose outcome is still unknown stays "delivered" across
// restarts and is retried by Settle on every tick. A one-shot schedule (Next
// reports no next occurrence) disables the task after its run; a repeating
// schedule advances NextRun. At most MaxConcurrent deliveries run at once, and
// a task is never delivered twice concurrently.
package automation

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultFileName is the registry document name under the store directory.
	DefaultFileName = "automations.json"
	// MaxRunHistory bounds the per-task run history (newest first).
	MaxRunHistory = 10
	// DefaultInterval is the scheduler poll interval when Interval is zero.
	DefaultInterval = 500 * time.Millisecond
	// DefaultMaxConcurrent bounds simultaneous deliveries when zero.
	DefaultMaxConcurrent = 4

	// fileMode is the registry document permission.
	fileMode = 0o600
	// dirMode is the store directory permission.
	dirMode = 0o700
)

// Run statuses. Delivered is the two-state record's first state; the others are
// terminal. Error is the product's failure spelling (not "failed").
const (
	// StatusDelivered marks a run whose delivery was accepted but whose outcome
	// is not known yet.
	StatusDelivered = "delivered"
	// StatusSuccess marks a run that settled without an error.
	StatusSuccess = "success"
	// StatusFailure marks a run that failed (delivery failure or a failed turn).
	StatusFailure = "error"
	// StatusInterrupted marks a run the user stopped or a restart cut short.
	StatusInterrupted = "interrupted"
)

// Errors the package reports; callers should match with errors.Is.
var (
	// ErrTaskNotFound reports an unknown task ID.
	ErrTaskNotFound = errors.New("automation: task not found")
	// ErrTaskExists reports a duplicate task ID on Create.
	ErrTaskExists = errors.New("automation: task already exists")
	// ErrTaskIDRequired reports a missing task ID on Create/Update.
	ErrTaskIDRequired = errors.New("automation: task id is required")
	// ErrStoreRequired reports a missing store (or store directory).
	ErrStoreRequired = errors.New("automation: store is required")
	// ErrDeliverRequired reports a scheduler without a Deliver callback.
	ErrDeliverRequired = errors.New("automation: deliver callback is required")
	// ErrSchedulerClosed reports a tick/run after Stop.
	ErrSchedulerClosed = errors.New("automation: scheduler is closed")
)

// Schedule is one task's schedule expression in the host's canonical
// vocabulary. This package never parses it: NextRun is persisted, and the
// scheduler asks the host's Next callback for the next occurrence after a run.
type Schedule string

// String returns the schedule expression.
func (s Schedule) String() string { return string(s) }

// Run is one recorded automation run. The record separates delivery from
// completion: DeliveredAt is set when Deliver accepted the task (zero on a
// delivery failure), CompletedAt and Status when the run settled. A delivered
// run that never settles keeps StatusDelivered and a zero CompletedAt.
type Run struct {
	// ID identifies the record while it is in flight; settlement writes back
	// by ID.
	ID string `json:"id"`
	// StartedAt is when the run attempt started.
	StartedAt time.Time `json:"startedAt"`
	// DeliveredAt is when Deliver accepted the task (zero on failure).
	DeliveredAt time.Time `json:"deliveredAt,omitempty"`
	// CompletedAt is when the run settled (zero while delivered/unknown).
	CompletedAt time.Time `json:"completedAt,omitempty"`
	// Status is StatusDelivered or one of the terminal statuses.
	Status string `json:"status"`
	// Error is the failure/interruption cause ("" for success and unknown).
	Error string `json:"error,omitempty"`
	// DeliveryID is the host delivery identifier returned by Deliver; the
	// Settle callback receives it to resolve the outcome.
	DeliveryID string `json:"deliveryId,omitempty"`
	// DurationMS is the wall time from delivery to completion (or from the
	// attempt start when delivery failed) in milliseconds.
	DurationMS int64 `json:"durationMs,omitempty"`
	// Host carries host-defined run fields verbatim (for example a display
	// summary or a conversation reference). The core never interprets them;
	// they round-trip through the registry document as top-level run keys.
	Host map[string]json.RawMessage `json:"-"`
}

// Terminal reports whether the run reached a terminal state.
func (r Run) Terminal() bool {
	switch r.Status {
	case StatusDelivered, "":
		return false
	default:
		return true
	}
}

// Task is one durable scheduled task. Host-only fields (model, workspace,
// conversation binding, permissions) stay outside this package.
type Task struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Prompt   string   `json:"prompt"`
	Schedule Schedule `json:"schedule,omitempty"`
	Enabled  bool     `json:"enabled"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	// LastRun is when the task last fired (zero when never).
	LastRun time.Time `json:"lastRun"`
	// NextRun is the next planned firing (zero when disabled/unscheduled).
	NextRun time.Time `json:"nextRun"`
	// LastError is the last run's failure text ("" when the last run was not a
	// failure).
	LastError string `json:"lastError"`
	// RunHistory is the recent runs, newest first, bounded by MaxRunHistory.
	RunHistory []Run `json:"runHistory,omitempty"`
	// Host carries host-defined task fields verbatim (for example a model,
	// workspace, permission or conversation binding). The core never
	// interprets them; they round-trip through the registry document as
	// top-level task keys so a host keeps its existing document readable.
	Host map[string]json.RawMessage `json:"-"`
}

// registryDocument is the on-disk registry shape.
type registryDocument struct {
	Tasks []Task `json:"tasks"`
}

// Store is the durable task registry. It is safe for concurrent use.
type Store struct {
	mu    sync.Mutex
	dir   string
	path  string
	tasks []Task
}

// Open loads (or creates in memory) the registry under dir. A missing document
// is empty; a corrupt or unreadable one is quarantined and the store starts
// empty. An empty dir reports ErrStoreRequired.
func Open(dir string) (*Store, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, ErrStoreRequired
	}
	return OpenFile(filepath.Join(dir, DefaultFileName))
}

// OpenFile loads the registry at an explicit document path (a host that lets
// users choose the registry location). The directory is created on the first
// persist. An empty path reports ErrStoreRequired.
func OpenFile(path string) (*Store, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, ErrStoreRequired
	}
	clean := filepath.Clean(path)
	store := &Store{dir: filepath.Dir(clean), path: clean}
	if err := store.Load(); err != nil {
		return nil, err
	}
	return store, nil
}

// Path returns the registry document path.
func (s *Store) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// List returns the tasks ordered by creation time, then ID. The result is a
// deep copy: mutating it cannot change the store.
func (s *Store) List() ([]Task, error) {
	if s == nil {
		return nil, ErrStoreRequired
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Task, len(s.tasks))
	for index, task := range s.tasks {
		out[index] = copyTask(task)
	}
	sort.SliceStable(out, func(left, right int) bool {
		if !out[left].CreatedAt.Equal(out[right].CreatedAt) {
			return out[left].CreatedAt.Before(out[right].CreatedAt)
		}
		return out[left].ID < out[right].ID
	})
	return out, nil
}

// Get returns one task by ID (a deep copy).
func (s *Store) Get(id string) (Task, bool) {
	if s == nil {
		return Task{}, false
	}
	id = strings.TrimSpace(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.indexLocked(id)
	if index < 0 {
		return Task{}, false
	}
	return copyTask(s.tasks[index]), true
}

// Create stores one new task and persists the registry atomically. A duplicate
// ID reports ErrTaskExists. The caller owns CreatedAt/UpdatedAt/NextRun; the
// scheduler fills scheduling state.
func (s *Store) Create(task Task) (Task, error) {
	if s == nil {
		return Task{}, ErrStoreRequired
	}
	task.ID = strings.TrimSpace(task.ID)
	if task.ID == "" {
		return Task{}, ErrTaskIDRequired
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.indexLocked(task.ID) >= 0 {
		return Task{}, fmt.Errorf("%w: %q", ErrTaskExists, task.ID)
	}
	task = copyTask(task)
	previous := append([]Task(nil), s.tasks...)
	s.tasks = append(s.tasks, task)
	if err := s.persistLocked(); err != nil {
		s.tasks = previous
		return Task{}, err
	}
	return copyTask(task), nil
}

// Update replaces an existing task (matched by ID) and persists atomically. An
// unknown ID reports ErrTaskNotFound.
func (s *Store) Update(task Task) (Task, error) {
	if s == nil {
		return Task{}, ErrStoreRequired
	}
	task.ID = strings.TrimSpace(task.ID)
	if task.ID == "" {
		return Task{}, ErrTaskIDRequired
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.indexLocked(task.ID)
	if index < 0 {
		return Task{}, fmt.Errorf("%w: %q", ErrTaskNotFound, task.ID)
	}
	task = copyTask(task)
	previous := append([]Task(nil), s.tasks...)
	s.tasks[index] = task
	if err := s.persistLocked(); err != nil {
		s.tasks = previous
		return Task{}, err
	}
	return copyTask(task), nil
}

// Delete removes a task and persists atomically. An unknown ID is a no-op (the
// scheduler's Delete reports not-found when its caller needs it).
func (s *Store) Delete(id string) error {
	if s == nil {
		return ErrStoreRequired
	}
	id = strings.TrimSpace(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.indexLocked(id)
	if index < 0 {
		return nil
	}
	previous := append([]Task(nil), s.tasks...)
	s.tasks = append(s.tasks[:index], s.tasks[index+1:]...)
	if err := s.persistLocked(); err != nil {
		s.tasks = previous
		return err
	}
	return nil
}

// Save persists the current registry (mutations already persist; this is for
// hosts that changed a returned copy and wrote it back through Update).
func (s *Store) Save() error {
	if s == nil {
		return ErrStoreRequired
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.persistLocked()
}

// Load replaces the in-memory registry from disk. A missing file is empty; a
// corrupt or unreadable one is quarantined and leaves the store empty.
func (s *Store) Load() error {
	if s == nil {
		return ErrStoreRequired
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

// loadLocked reads the registry document.
func (s *Store) loadLocked() error {
	s.tasks = nil
	data, err := os.ReadFile(s.path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			s.quarantineLocked()
		}
		return nil
	}
	var document registryDocument
	if err := json.Unmarshal(data, &document); err != nil {
		s.quarantineLocked()
		return nil
	}
	tasks := make([]Task, 0, len(document.Tasks))
	for _, task := range document.Tasks {
		task.ID = strings.TrimSpace(task.ID)
		if task.ID == "" {
			continue
		}
		task.CreatedAt = task.CreatedAt.UTC()
		task.UpdatedAt = task.UpdatedAt.UTC()
		task.LastRun = task.LastRun.UTC()
		task.NextRun = task.NextRun.UTC()
		task.RunHistory = copyRuns(task.RunHistory)
		tasks = append(tasks, task)
	}
	s.tasks = tasks
	return nil
}

// quarantineLocked renames an unusable registry out of the way (best effort) so
// the next persist cannot silently destroy the original document.
func (s *Store) quarantineLocked() {
	backup := fmt.Sprintf("%s.corrupt-%d", s.path, time.Now().UTC().UnixNano())
	_ = os.Rename(s.path, backup)
}

// persistLocked writes the registry atomically. The caller holds s.mu.
func (s *Store) persistLocked() error {
	if err := os.MkdirAll(s.dir, dirMode); err != nil {
		return fmt.Errorf("automation: create registry directory %q: %w", s.dir, err)
	}
	document := registryDocument{Tasks: make([]Task, 0, len(s.tasks))}
	for _, task := range s.tasks {
		document.Tasks = append(document.Tasks, copyTask(task))
	}
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return fmt.Errorf("automation: encode registry: %w", err)
	}
	temporary, err := os.CreateTemp(s.dir, ".automations-*.tmp")
	if err != nil {
		return fmt.Errorf("automation: create temporary registry: %w", err)
	}
	temporaryPath := temporary.Name()
	cleanup := func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}
	if err := temporary.Chmod(fileMode); err != nil {
		cleanup()
		return fmt.Errorf("automation: chmod temporary registry: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("automation: write temporary registry: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("automation: sync temporary registry: %w", err)
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("automation: close temporary registry: %w", err)
	}
	if err := os.Rename(temporaryPath, s.path); err != nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("automation: replace registry %q: %w", s.path, err)
	}
	if directory, err := os.Open(s.dir); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}

// indexLocked finds a task by ID. The caller holds s.mu.
func (s *Store) indexLocked(id string) int {
	for index, task := range s.tasks {
		if task.ID == id {
			return index
		}
	}
	return -1
}

// copyTask returns an independent copy (the run history slice is copied).
func copyTask(task Task) Task {
	task.RunHistory = copyRuns(task.RunHistory)
	task.Host = copyHost(task.Host)
	return task
}

// copyRuns copies a run-history slice (nil stays nil).
func copyRuns(runs []Run) []Run {
	if runs == nil {
		return nil
	}
	out := make([]Run, len(runs))
	for index, run := range runs {
		out[index] = run
		out[index].Host = copyHost(run.Host)
	}
	return out
}
