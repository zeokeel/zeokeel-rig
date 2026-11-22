// Package team provides a host-agnostic state machine for teammate
// orchestration: spawning and registering members, truthful run status
// transitions, follow-up sends, waits and wake notifications, a bounded activity
// fold, and a durable snapshot/restore seam.
//
// The package never starts an agent itself. A Host adapts a concrete backend
// (for example a rig.SubagentRunner) and is the only thing that knows how to
// spawn and drive a child run:
//
//	host := myHost{}            // Spawn/Send/Wait/Cancel/Snapshot
//	store := team.NewMemoryStore() // optional durable snapshot store
//	coordinator := team.New(host, team.Options{Store: store})
//	if err := coordinator.Restore(ctx); err != nil { ... }
//	member, err := coordinator.Spawn(ctx, team.SpawnSpec{Name: "alice", Prompt: "audit the diff"})
//	...
//	result, err := coordinator.Wait(ctx, member.ID)
//
// # Status machine
//
//   - Spawn registers a member and starts its initial run: StatusRunning.
//   - The run watcher (or Wait/Send) records the outcome: StatusDone or
//     StatusFailed. Token usage and iterations accumulate over every run.
//   - Cancel interrupts the current run but keeps the member available
//     (StatusInterrupted); a later Send starts a new run.
//   - Close stops the member and marks it StatusClosed; it stays in the read
//     model (historically) but rejects every further operation.
//   - A run that did not survive a restart, or whose backend child vanished, is
//     reported as StatusInterrupted with an explanatory Error, never as running.
//
// # Bounds
//
// The activity fold is bounded per member and overall (Options.MaxActivity,
// Options.MaxActivityPerMember) and every text is whitespace-collapsed and
// bounded (Options.MaxTextRunes, Options.MaxErrorRunes). Snapshots are deep
// copies: a caller can never mutate the coordinator's state through them.
//
// The coordinator is safe for concurrent use. It serializes one run per member:
// Send on a member that is already Running returns ErrMemberRunning instead of
// racing the watcher (the host may serialize runs internally, but the
// coordinator refuses to guess which result belongs to which run).
package team

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Status is the truthful lifecycle of one teammate run.
type Status string

const (
	// StatusIdle is a registered teammate with no run observed yet (only
	// observable after a restore of an idle snapshot).
	StatusIdle Status = "idle"
	// StatusRunning is a teammate whose current run is in flight.
	StatusRunning Status = "running"
	// StatusDone is a teammate whose latest run returned an answer.
	StatusDone Status = "done"
	// StatusFailed is a teammate whose latest run returned an error.
	StatusFailed Status = "failed"
	// StatusInterrupted is a teammate whose run was canceled, went missing or
	// did not survive a restart. The member stays available for a new Send.
	StatusInterrupted Status = "interrupted"
	// StatusClosed is a teammate that was closed; it stays in the read model as
	// history but rejects further operations.
	StatusClosed Status = "closed"
)

// ActivityKind names one row of the bounded team activity fold.
type ActivityKind string

const (
	// ActivitySpawn is a teammate registration.
	ActivitySpawn ActivityKind = "spawn"
	// ActivitySend is a follow-up message delivered to a teammate.
	ActivitySend ActivityKind = "send"
	// ActivityWait is a wait for a teammate's current run.
	ActivityWait ActivityKind = "wait"
	// ActivityFinish is a run that returned an answer.
	ActivityFinish ActivityKind = "finish"
	// ActivityError is a run or operation that failed.
	ActivityError ActivityKind = "error"
	// ActivityInterrupt is an interrupted run or a member gone missing.
	ActivityInterrupt ActivityKind = "interrupt"
	// ActivityClose is a closed member.
	ActivityClose ActivityKind = "close"
	// ActivityTool is any other team-level event a host records through
	// Coordinator.RecordActivity.
	ActivityTool ActivityKind = "tool"
)

// Default bounds of the activity fold and its text fields.
const (
	// DefaultMaxActivity bounds the whole activity fold.
	DefaultMaxActivity = 200
	// DefaultMaxActivityPerMember bounds the rows of one member.
	DefaultMaxActivityPerMember = 30
	// DefaultMaxTextRunes bounds one activity text.
	DefaultMaxTextRunes = 200
	// DefaultMaxErrorRunes bounds a stored run error.
	DefaultMaxErrorRunes = 300
)

// Sentinel errors. Hosts and callers use errors.Is to distinguish them.
var (
	// ErrNoHost reports a coordinator without a Host.
	ErrNoHost = errors.New("team: no host configured")
	// ErrUnknownMember reports an id/name that matches no teammate.
	ErrUnknownMember = errors.New("team: unknown teammate")
	// ErrAmbiguousMember reports a display name shared by several teammates.
	ErrAmbiguousMember = errors.New("team: ambiguous teammate name")
	// ErrMemberClosed reports an operation on a closed teammate.
	ErrMemberClosed = errors.New("team: teammate is closed")
	// ErrMemberRunning reports a send/close race against an in-flight run.
	ErrMemberRunning = errors.New("team: teammate is already running")
	// ErrMemberInactive reports a wait for a run that is not in flight.
	ErrMemberInactive = errors.New("team: teammate has no run in flight")
	// ErrHistorical reports a send to a restored member whose live backend is
	// gone; the host (cold resume) must resurrect it first.
	ErrHistorical = errors.New("team: teammate history has no live backend")
	// ErrEmptyName reports a spawn without a display name.
	ErrEmptyName = errors.New("team: teammate name is required")
	// ErrEmptyPrompt reports a spawn without a prompt.
	ErrEmptyPrompt = errors.New("team: teammate prompt is required")
	// ErrEmptyMessage reports a send without a message.
	ErrEmptyMessage = errors.New("team: message is required")
	// ErrDuplicateMember reports a host handle reused by a second spawn.
	ErrDuplicateMember = errors.New("team: host returned a duplicate teammate id")
	// ErrEmptyHostID reports a host that spawned without returning a handle.
	ErrEmptyHostID = errors.New("team: host returned an empty teammate id")
)

// Activity is one row of the bounded team activity fold.
type Activity struct {
	// ID is a coordinator-unique row identity.
	ID string
	// MemberID is the teammate the row belongs to ("" for a team-level row).
	MemberID string
	// Kind is one of the Activity* values.
	Kind ActivityKind
	// Text is a whitespace-collapsed, bounded one-line summary.
	Text string
	// Time is when the row was recorded.
	Time time.Time
	// Error reports whether the row describes a failure.
	Error bool
}

// Teammate is one member's typed read model.
type Teammate struct {
	// ID is the host handle returned by Host.Spawn.
	ID string
	// Name is the display name passed on spawn.
	Name string
	// Model is the model the teammate runs with ("" = host default).
	Model string
	// Prompt is the spawn prompt (bounded).
	Prompt string
	// ParentID is the parent session/scope that spawned the member ("" when the
	// host could not attribute it).
	ParentID string
	// Fork marks a member forked from the parent conversation's history.
	Fork bool
	// ForkedFrom is the parent identity a fork inherited ("" for non-forks and
	// when the host could not attribute it).
	ForkedFrom string
	// Status is the truthful run lifecycle.
	Status Status
	// Error is the latest run error ("" when the latest run succeeded).
	Error string
	// LastOutput is the latest finished answer.
	LastOutput string
	// Created is when the member was spawned.
	Created time.Time
	// Started is when the current (or latest) run started.
	Started time.Time
	// Finished is when the latest run ended (zero while running).
	Finished time.Time
	// UsageTokens and Iterations accumulate over every completed run.
	UsageTokens int
	Iterations  int
	// Activity is the member's bounded recent activity, oldest first.
	Activity []Activity
	// Historical marks a member restored from a persisted snapshot: its live
	// backend is gone until the host resurrects it.
	Historical bool
}

// TeamState is a deep, bounded snapshot of the coordinator (the durable shape).
type TeamState struct {
	// Revision increments on every published change.
	Revision uint64
	// Members are the teammates in spawn order (closed members included).
	Members []Teammate
	// Activity is the bounded team activity fold, oldest first.
	Activity []Activity
}

// Result is the outcome of one child run as reported by the Host.
type Result struct {
	// Output is the final assistant text.
	Output string
	// UsageTokens and Iterations describe the run's token usage and rounds.
	UsageTokens int
	Iterations  int
}

// SpawnSpec describes one teammate to start.
type SpawnSpec struct {
	// Name is the display name (required, not necessarily unique).
	Name string
	// Prompt is the first user message (required).
	Prompt string
	// Model optionally overrides the model of the member.
	Model string
	// ParentID attributes the member to a parent session/scope.
	ParentID string
	// Fork marks a forked member (inherits a copy of the parent history).
	Fork bool
	// ForkedFrom optionally identifies the parent the fork inherited from.
	ForkedFrom string
}

// MemberSnapshot is the host's live view of one child.
type MemberSnapshot struct {
	// Status is a host-defined availability label (for example "active").
	Status string
	// Running reports whether the host believes a run is in flight.
	Running bool
	// Finished reports a completed latest run when Running is false. Hosts that
	// cannot answer leave it false (the member is then adopted as idle).
	Finished bool
	// Output is the latest finished answer (Finished).
	Output string
	// UsageTokens and Iterations describe the latest finished run (Finished).
	UsageTokens int
	Iterations  int
	// Error is the latest finished run's error (Finished; makes it failed).
	Error string
}

// Host starts and drives child runs. It is the only backend-specific seam; all
// methods must be safe for concurrent use.
type Host interface {
	// Spawn starts one child run and returns its stable handle.
	Spawn(ctx context.Context, spec SpawnSpec) (string, error)
	// Send starts a follow-up run on the child and blocks until it finishes.
	Send(ctx context.Context, id, message string) (Result, error)
	// Wait blocks until the child's current (or initial) run finishes.
	Wait(ctx context.Context, id string) (Result, error)
	// Cancel stops the child's current run. The child may stay registered.
	Cancel(ctx context.Context, id string) error
	// Snapshot reports the host's live view of the child; ok is false when the
	// child is unknown or gone.
	Snapshot(ctx context.Context, id string) (MemberSnapshot, bool)
}

// Store persists the read model between processes. Implementations should be
// cheap: the coordinator saves after every published change.
type Store interface {
	// Load returns the last saved state (zero when nothing was saved yet).
	Load(ctx context.Context) (TeamState, error)
	// Save replaces the saved state with a deep copy of state.
	Save(ctx context.Context, state TeamState) error
}

// Options configures a Coordinator. Zero values pick the documented defaults.
type Options struct {
	// Store optionally persists the read model after every change.
	Store Store
	// Notify is called after every published change (typically a UI frame
	// request). It is never called with a lock held.
	Notify func()
	// Clock overrides the time source (tests).
	Clock func() time.Time
	// OnError reports non-fatal errors (a failed Store.Save); nil discards them.
	OnError func(error)
	// MaxActivity bounds the whole activity fold (DefaultMaxActivity).
	MaxActivity int
	// MaxActivityPerMember bounds the rows of one member
	// (DefaultMaxActivityPerMember).
	MaxActivityPerMember int
	// MaxTextRunes bounds one activity text (DefaultMaxTextRunes).
	MaxTextRunes int
	// MaxErrorRunes bounds a stored run error (DefaultMaxErrorRunes).
	MaxErrorRunes int
}

// Coordinator owns the team state machine of one process.
type Coordinator struct {
	host    Host
	store   Store
	notify  func()
	clock   func() time.Time
	onError func(error)

	maxActivity          int
	maxActivityPerMember int
	maxTextRunes         int
	maxErrorRunes        int

	mu            sync.Mutex
	revision      uint64
	savedRevision uint64
	nextActivity  uint64
	members       []*member
	membersByID   map[string]*member
	activities    []Activity
	wake          chan struct{}

	// saveMu serializes Store.Save calls so a slow store cannot interleave two
	// snapshots and persist an older revision after a newer one.
	saveMu sync.Mutex
}

// member is the mutable state of one teammate.
type member struct {
	info Teammate
	// gen identifies the current run; a watcher/Send result is only recorded when
	// its generation is still current (Cancel/Close/missing bump it).
	gen uint64
	// observed is the newest generation whose outcome was recorded.
	observed uint64
	// last is the latest run result (Wait answers a finished run with it).
	last Result
	// lastErr is the latest run error (nil on success).
	lastErr error
}

// New builds a Coordinator over host. A nil host is allowed: every operation
// that needs the backend returns ErrNoHost.
func New(host Host, opts Options) *Coordinator {
	coordinator := &Coordinator{
		host:                 host,
		store:                opts.Store,
		notify:               opts.Notify,
		clock:                opts.Clock,
		onError:              opts.OnError,
		maxActivity:          opts.MaxActivity,
		maxActivityPerMember: opts.MaxActivityPerMember,
		maxTextRunes:         opts.MaxTextRunes,
		maxErrorRunes:        opts.MaxErrorRunes,
		membersByID:          make(map[string]*member),
		wake:                 make(chan struct{}),
	}
	if coordinator.clock == nil {
		coordinator.clock = time.Now
	}
	if coordinator.maxActivity <= 0 {
		coordinator.maxActivity = DefaultMaxActivity
	}
	if coordinator.maxActivityPerMember <= 0 {
		coordinator.maxActivityPerMember = DefaultMaxActivityPerMember
	}
	if coordinator.maxTextRunes <= 0 {
		coordinator.maxTextRunes = DefaultMaxTextRunes
	}
	if coordinator.maxErrorRunes <= 0 {
		coordinator.maxErrorRunes = DefaultMaxErrorRunes
	}
	return coordinator
}

// Spawn registers a teammate and starts its initial run. The returned snapshot
// is StatusRunning; the run is observed by one watcher goroutine (Wait/Send see
// its result too, and only one of them records it).
func (c *Coordinator) Spawn(ctx context.Context, spec SpawnSpec) (Teammate, error) {
	if c == nil || c.host == nil {
		return Teammate{}, ErrNoHost
	}
	if err := contextErr(ctx); err != nil {
		return Teammate{}, err
	}
	name := strings.TrimSpace(spec.Name)
	prompt := strings.TrimSpace(spec.Prompt)
	if name == "" {
		return Teammate{}, ErrEmptyName
	}
	if prompt == "" {
		return Teammate{}, ErrEmptyPrompt
	}
	clean := SpawnSpec{
		Name:       name,
		Prompt:     prompt,
		Model:      strings.TrimSpace(spec.Model),
		ParentID:   strings.TrimSpace(spec.ParentID),
		Fork:       spec.Fork,
		ForkedFrom: strings.TrimSpace(spec.ForkedFrom),
	}
	id, err := c.host.Spawn(ctx, clean)
	if err != nil {
		return Teammate{}, err
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return Teammate{}, ErrEmptyHostID
	}

	c.mu.Lock()
	if _, exists := c.membersByID[id]; exists {
		c.mu.Unlock()
		// The host returned a handle the coordinator already owns: release the
		// stray child instead of silently aliasing two members.
		_ = c.host.Cancel(context.Background(), id)
		return Teammate{}, ErrDuplicateMember
	}
	now := c.now()
	registered := &member{
		info: Teammate{
			ID:         id,
			Name:       name,
			Model:      clean.Model,
			Prompt:     collapseText(prompt, c.maxTextRunes),
			ParentID:   clean.ParentID,
			Fork:       clean.Fork,
			ForkedFrom: clean.ForkedFrom,
			Status:     StatusRunning,
			Created:    now,
			Started:    now,
		},
		gen: 1,
	}
	c.members = append(c.members, registered)
	c.membersByID[id] = registered
	c.addActivityLocked(registered, ActivitySpawn,
		fmt.Sprintf("spawned %s: %s", name, registered.info.Prompt), false)
	snapshot := c.snapshotMemberLocked(registered)
	gen := registered.gen
	c.mu.Unlock()

	c.publish()
	go c.watch(id, gen)
	return snapshot, nil
}

// Adopt registers a host child that was started outside the coordinator (for
// example one spawned by a model-facing team tool) so the coordinator owns its
// run state, activity and persistence. The host child must still exist; a
// running child gets one watcher (a later Cancel/Close invalidates it through
// the generation guard), an idle child is registered as StatusIdle without a
// watcher.
//
// Adopt rejects an unknown/vanished host child (ErrUnknownMember), a member the
// coordinator already owns (ErrDuplicateMember, or ErrMemberClosed for a closed
// one), and the same validation errors as Spawn (ErrEmptyName/ErrEmptyPrompt).
func (c *Coordinator) Adopt(ctx context.Context, id string, spec SpawnSpec) (Teammate, error) {
	if c == nil || c.host == nil {
		return Teammate{}, ErrNoHost
	}
	if err := contextErr(ctx); err != nil {
		return Teammate{}, err
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return Teammate{}, ErrUnknownMember
	}
	name := strings.TrimSpace(spec.Name)
	prompt := strings.TrimSpace(spec.Prompt)
	if name == "" {
		return Teammate{}, ErrEmptyName
	}
	if prompt == "" {
		return Teammate{}, ErrEmptyPrompt
	}

	c.mu.Lock()
	if existing := c.membersByID[id]; existing != nil {
		closed := existing.info.Status == StatusClosed
		c.mu.Unlock()
		if closed {
			return Teammate{}, ErrMemberClosed
		}
		return Teammate{}, ErrDuplicateMember
	}
	c.mu.Unlock()

	// The host is authoritative about whether the child still exists and what
	// its latest run looks like.
	live, ok := c.host.Snapshot(ctx, id)
	if !ok {
		return Teammate{}, ErrUnknownMember
	}

	clean := SpawnSpec{
		Name:       name,
		Prompt:     prompt,
		Model:      strings.TrimSpace(spec.Model),
		ParentID:   strings.TrimSpace(spec.ParentID),
		Fork:       spec.Fork,
		ForkedFrom: strings.TrimSpace(spec.ForkedFrom),
	}
	status := StatusIdle
	if live.Running {
		status = StatusRunning
	}
	now := c.now()
	registered := &member{
		info: Teammate{
			ID:         id,
			Name:       name,
			Model:      clean.Model,
			Prompt:     collapseText(prompt, c.maxTextRunes),
			ParentID:   clean.ParentID,
			Fork:       clean.Fork,
			ForkedFrom: clean.ForkedFrom,
			Status:     status,
			Created:    now,
		},
		gen: 1,
	}
	switch {
	case live.Running:
		registered.info.Started = now
	case live.Finished:
		// A member that already completed (a fast tool spawn) keeps its answer
		// instead of being adopted as idle with no result.
		registered.info.Finished = now
		registered.info.UsageTokens = maxInt(live.UsageTokens, 0)
		registered.info.Iterations = maxInt(live.Iterations, 0)
		if errText := strings.TrimSpace(live.Error); errText != "" {
			registered.info.Status = StatusFailed
			registered.info.Error = truncateText(errText, c.maxErrorRunes)
			registered.lastErr = errors.New(errText)
		} else {
			registered.info.Status = StatusDone
			registered.info.LastOutput = strings.TrimSpace(live.Output)
			registered.last = Result{
				Output:      strings.TrimSpace(live.Output),
				UsageTokens: maxInt(live.UsageTokens, 0),
				Iterations:  maxInt(live.Iterations, 0),
			}
		}
	}

	c.mu.Lock()
	// A concurrent Adopt/Spawn may have registered the id while the host was
	// queried; keep the first owner.
	if existing := c.membersByID[id]; existing != nil {
		closed := existing.info.Status == StatusClosed
		c.mu.Unlock()
		if closed {
			return Teammate{}, ErrMemberClosed
		}
		return Teammate{}, ErrDuplicateMember
	}
	c.members = append(c.members, registered)
	c.membersByID[id] = registered
	c.addActivityLocked(registered, ActivitySpawn,
		fmt.Sprintf("adopted %s: %s", name, registered.info.Prompt), false)
	switch registered.info.Status {
	case StatusDone:
		c.addActivityLocked(registered, ActivityFinish,
			fmt.Sprintf("%s finished: %s", name, registered.info.LastOutput), false)
	case StatusFailed:
		c.addActivityLocked(registered, ActivityError,
			fmt.Sprintf("%s failed: %s", name, registered.info.Error), true)
	}
	snapshot := c.snapshotMemberLocked(registered)
	gen := registered.gen
	c.mu.Unlock()

	c.publish()
	if status == StatusRunning {
		go c.watch(id, gen)
	}
	return snapshot, nil
}

// Track starts watching the member's NEXT run without sending anything. The
// host must already have a run in flight or queued (for example after an
// asynchronous send); the watcher records that run's outcome exactly once.
// Tracking an idle/done member simply arms the next result. A later
// Cancel/Close bumps the generation, so a stale tracked result is ignored.
func (c *Coordinator) Track(ctx context.Context, id string) (Teammate, error) {
	if c == nil || c.host == nil {
		return Teammate{}, ErrNoHost
	}
	if err := contextErr(ctx); err != nil {
		return Teammate{}, err
	}
	c.mu.Lock()
	registered, err := c.resolveLocked(id)
	if err != nil {
		c.mu.Unlock()
		return Teammate{}, err
	}
	if registered.info.Status == StatusClosed {
		c.mu.Unlock()
		return Teammate{}, ErrMemberClosed
	}
	if registered.info.Historical {
		// A restored member needs a live backend before its next run can be
		// observed.
		if _, ok := c.host.Snapshot(ctx, registered.info.ID); !ok {
			c.mu.Unlock()
			return Teammate{}, ErrHistorical
		}
	}
	teammateID := registered.info.ID
	name := registered.info.Name
	registered.gen++ // any previous watcher's result belongs to an older run
	gen := registered.gen
	registered.info.Status = StatusRunning
	registered.info.Started = c.now()
	registered.info.Finished = time.Time{}
	registered.info.Error = ""
	registered.info.LastOutput = ""
	registered.info.Historical = false
	c.addActivityLocked(registered, ActivityWait, fmt.Sprintf("tracking %s", name), false)
	snapshot := c.snapshotMemberLocked(registered)
	c.mu.Unlock()

	c.publish()
	if _, ok := c.host.Snapshot(ctx, teammateID); !ok {
		c.markMissing(teammateID, gen)
		return Teammate{}, ErrUnknownMember
	}
	go c.watch(teammateID, gen)
	return snapshot, nil
}

// watch records the initial run's outcome. It is one bounded goroutine per
// spawn/adoption and exits when the run finishes (Cancel/Close unblock it
// through the host and bump the run generation so its record is ignored).
func (c *Coordinator) watch(id string, gen uint64) {
	if c.host == nil {
		return
	}
	result, err := c.host.Wait(context.Background(), id)
	c.recordRun(id, gen, result, err)
}

// Send delivers a follow-up message and returns the run's answer. It starts a
// new run (StatusRunning), so it is rejected while a run is in flight.
func (c *Coordinator) Send(ctx context.Context, id, message string) (Result, error) {
	if c == nil || c.host == nil {
		return Result{}, ErrNoHost
	}
	if err := contextErr(ctx); err != nil {
		return Result{}, err
	}
	message = strings.TrimSpace(message)
	if message == "" {
		return Result{}, ErrEmptyMessage
	}

	c.mu.Lock()
	registered, err := c.resolveLocked(id)
	if err != nil {
		c.mu.Unlock()
		return Result{}, err
	}
	teammateID := registered.info.ID
	name := registered.info.Name
	historical := registered.info.Historical
	status := registered.info.Status
	c.mu.Unlock()

	if status == StatusClosed {
		return Result{}, ErrMemberClosed
	}
	if status == StatusRunning {
		return Result{}, ErrMemberRunning
	}
	if historical {
		// A restored member has no live backend until the host resurrects it.
		if _, ok := c.host.Snapshot(ctx, teammateID); !ok {
			return Result{}, ErrHistorical
		}
	}

	c.mu.Lock()
	registered = c.membersByID[teammateID]
	if registered == nil {
		c.mu.Unlock()
		return Result{}, ErrUnknownMember
	}
	if registered.info.Status == StatusClosed {
		c.mu.Unlock()
		return Result{}, ErrMemberClosed
	}
	if registered.info.Status == StatusRunning {
		c.mu.Unlock()
		return Result{}, ErrMemberRunning
	}
	registered.gen++
	gen := registered.gen
	registered.info.Status = StatusRunning
	registered.info.Started = c.now()
	registered.info.Finished = time.Time{}
	registered.info.Error = ""
	// While a run is in flight the previous answer is stale: the read model shows
	// the run, not the last answer.
	registered.info.LastOutput = ""
	registered.info.Historical = false
	c.addActivityLocked(registered, ActivitySend,
		fmt.Sprintf("sent to %s: %s", name, collapseText(message, c.maxTextRunes)), false)
	c.mu.Unlock()

	c.publish()
	if _, ok := c.host.Snapshot(ctx, teammateID); !ok {
		c.markMissing(teammateID, gen)
		return Result{}, ErrUnknownMember
	}
	result, err := c.host.Send(ctx, teammateID, message)
	if ctxErr := ctx.Err(); ctxErr != nil {
		c.markInterrupted(teammateID, gen, ctxErr)
		return Result{}, ctxErr
	}
	c.recordRun(teammateID, gen, result, err)
	return result, err
}

// Wait blocks until the teammate's current run finishes and returns its result.
// A finished run keeps answering with its last result; a member without a run
// in flight reports ErrMemberInactive.
func (c *Coordinator) Wait(ctx context.Context, id string) (Result, error) {
	if c == nil || c.host == nil {
		return Result{}, ErrNoHost
	}
	if err := contextErr(ctx); err != nil {
		return Result{}, err
	}

	c.mu.Lock()
	registered, err := c.resolveLocked(id)
	if err != nil {
		c.mu.Unlock()
		return Result{}, err
	}
	teammateID := registered.info.ID
	name := registered.info.Name
	status := registered.info.Status
	gen := registered.gen
	last := registered.last
	lastErr := registered.lastErr
	memberError := registered.info.Error
	c.mu.Unlock()

	switch status {
	case StatusClosed:
		return Result{}, ErrMemberClosed
	case StatusIdle, StatusInterrupted:
		return Result{}, ErrMemberInactive
	case StatusDone:
		return last, nil
	case StatusFailed:
		if lastErr != nil {
			return Result{}, lastErr
		}
		if memberError != "" {
			return Result{}, errors.New(memberError)
		}
		return Result{}, nil
	}

	c.mu.Lock()
	if current := c.membersByID[teammateID]; current != nil {
		c.addActivityLocked(current, ActivityWait, fmt.Sprintf("waiting for %s", name), false)
	}
	c.mu.Unlock()
	c.publish()

	if _, ok := c.host.Snapshot(ctx, teammateID); !ok {
		c.markMissing(teammateID, gen)
		return Result{}, ErrUnknownMember
	}
	result, err := c.host.Wait(ctx, teammateID)
	if ctxErr := ctx.Err(); ctxErr != nil {
		c.markInterrupted(teammateID, gen, ctxErr)
		return Result{}, ctxErr
	}
	c.recordRun(teammateID, gen, result, err)
	return result, err
}

// Cancel interrupts the teammate's current run and keeps the member available
// for a later Send. Canceling an idle member is a no-op.
func (c *Coordinator) Cancel(ctx context.Context, id string) error {
	if c == nil || c.host == nil {
		return ErrNoHost
	}
	if err := contextErr(ctx); err != nil {
		return err
	}
	c.mu.Lock()
	registered, err := c.resolveLocked(id)
	if err != nil {
		c.mu.Unlock()
		return err
	}
	teammateID := registered.info.ID
	if registered.info.Status == StatusClosed {
		c.mu.Unlock()
		return ErrMemberClosed
	}
	running := registered.info.Status == StatusRunning
	if running {
		registered.gen++ // the watcher's outcome no longer belongs to this run
		registered.info.Status = StatusInterrupted
		registered.info.Finished = c.now()
		registered.info.Error = "run interrupted"
		c.addActivityLocked(registered, ActivityInterrupt,
			fmt.Sprintf("interrupted %s", registered.info.Name), false)
	}
	c.mu.Unlock()
	if !running {
		return nil
	}
	c.publish()
	return c.host.Cancel(ctx, teammateID)
}

// Close stops the teammate and marks it closed. The member stays in the read
// model so a reopened app can show history; every later operation is rejected.
func (c *Coordinator) Close(ctx context.Context, id string) error {
	if c == nil || c.host == nil {
		return ErrNoHost
	}
	if err := contextErr(ctx); err != nil {
		return err
	}
	c.mu.Lock()
	registered, err := c.resolveLocked(id)
	if err != nil {
		c.mu.Unlock()
		return err
	}
	teammateID := registered.info.ID
	if registered.info.Status == StatusClosed {
		c.mu.Unlock()
		return ErrMemberClosed
	}
	running := registered.info.Status == StatusRunning
	if running {
		registered.gen++
		registered.info.Status = StatusInterrupted
		registered.info.Finished = c.now()
		registered.info.Error = "closed while running"
		c.addActivityLocked(registered, ActivityInterrupt,
			fmt.Sprintf("interrupted %s", registered.info.Name), false)
	}
	registered.info.Status = StatusClosed
	registered.info.Historical = false
	c.addActivityLocked(registered, ActivityClose,
		fmt.Sprintf("closed %s", registered.info.Name), false)
	c.mu.Unlock()

	c.publish()
	// A restored member has no backend to cancel; a live one is stopped.
	if _, ok := c.host.Snapshot(ctx, teammateID); ok {
		if err := c.host.Cancel(ctx, teammateID); err != nil {
			return err
		}
	}
	return nil
}

// Reconcile compares the in-flight members with the host's live view and marks
// the ones the backend no longer knows as interrupted. It never starts runs.
func (c *Coordinator) Reconcile(ctx context.Context) error {
	if c == nil || c.host == nil {
		return ErrNoHost
	}
	if err := contextErr(ctx); err != nil {
		return err
	}
	type probe struct {
		id  string
		gen uint64
	}
	c.mu.Lock()
	probes := make([]probe, 0, len(c.members))
	for _, registered := range c.members {
		switch registered.info.Status {
		case StatusRunning, StatusIdle:
			probes = append(probes, probe{id: registered.info.ID, gen: registered.gen})
		}
	}
	c.mu.Unlock()
	for _, candidate := range probes {
		if _, ok := c.host.Snapshot(ctx, candidate.id); !ok {
			c.markMissing(candidate.id, candidate.gen)
		}
	}
	return nil
}

// Restore loads the persisted state. A run cannot survive a process restart, so
// every running/idle member becomes StatusInterrupted with an explanatory error
// and the corrected state is published (and saved) again. Restored members are
// Historical until the host resurrects them.
func (c *Coordinator) Restore(ctx context.Context) error {
	if c == nil || c.store == nil {
		return nil
	}
	if err := contextErr(ctx); err != nil {
		return err
	}
	state, err := c.store.Load(ctx)
	if err != nil {
		return err
	}

	c.mu.Lock()
	c.members = nil
	c.membersByID = make(map[string]*member, len(state.Members))
	c.activities = nil
	c.revision = state.Revision
	c.savedRevision = state.Revision
	c.activities = append(c.activities, cloneActivities(state.Activity)...)
	// A Store may persist only the members (each carrying its own bounded
	// activity); rebuild the fold from them so the team timeline survives too.
	if len(c.activities) == 0 {
		c.activities = foldMemberActivity(state.Members)
	}
	var stale []*member
	for _, saved := range state.Members {
		info := saved
		info.Activity = nil
		info.Historical = true
		registered := &member{info: info, gen: 1}
		if info.Status == StatusRunning || info.Status == StatusIdle {
			registered.info.Status = StatusInterrupted
			if registered.info.Finished.IsZero() {
				registered.info.Finished = c.now()
			}
			registered.info.Error = "run did not survive the restart"
			stale = append(stale, registered)
		}
		if registered.info.Status == StatusFailed && registered.info.Error != "" {
			registered.lastErr = errors.New(registered.info.Error)
		}
		registered.last = Result{Output: strings.TrimSpace(registered.info.LastOutput)}
		c.members = append(c.members, registered)
		c.membersByID[info.ID] = registered
	}
	for _, registered := range stale {
		c.addActivityLocked(registered, ActivityInterrupt,
			fmt.Sprintf("interrupted %s: run did not survive the restart", registered.info.Name), true)
	}
	c.mu.Unlock()

	if len(stale) > 0 {
		c.publish()
	}
	return nil
}

// Changes returns a channel closed on the next published change. Subscribers
// that need to observe a specific change should take the channel before
// triggering it; a later call observes only changes after it.
func (c *Coordinator) Changes() <-chan struct{} {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wake
}

// WaitChange blocks until the next published change or the context ends.
func (c *Coordinator) WaitChange(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	changes := c.Changes()
	select {
	case <-changes:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Teammate returns one member by id or unique display name.
func (c *Coordinator) Teammate(ref string) (Teammate, bool) {
	if c == nil {
		return Teammate{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	registered, err := c.resolveLocked(ref)
	if err != nil {
		return Teammate{}, false
	}
	return c.snapshotMemberLocked(registered), true
}

// Activities returns a deep copy of the bounded activity fold, oldest first.
func (c *Coordinator) Activities() []Activity {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return cloneActivities(c.activities)
}

// Snapshot returns a deep copy of the whole read model.
func (c *Coordinator) Snapshot() TeamState {
	if c == nil {
		return TeamState{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stateLocked()
}

// RecordActivity lets a host record one team-level event (a task-board action,
// a model-facing tool invocation) in the bounded fold. It publishes a change.
func (c *Coordinator) RecordActivity(kind ActivityKind, text string, isError bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.addActivityLocked(nil, kind, text, isError)
	c.mu.Unlock()
	c.publish()
}

// resolveLocked resolves an id or unique display name to its member. Closed
// members stay addressable by id (their operations report ErrMemberClosed) but
// do not participate in name resolution. The caller holds c.mu.
func (c *Coordinator) resolveLocked(ref string) (*member, error) {
	trimmed := strings.TrimSpace(ref)
	if trimmed == "" {
		return nil, ErrUnknownMember
	}
	if registered := c.membersByID[trimmed]; registered != nil {
		return registered, nil
	}
	var found *member
	for _, registered := range c.members {
		if registered.info.Status == StatusClosed || registered.info.Name != trimmed {
			continue
		}
		if found != nil {
			return nil, ErrAmbiguousMember
		}
		found = registered
	}
	if found == nil {
		return nil, ErrUnknownMember
	}
	return found, nil
}

// snapshotMemberLocked converts one member to its read-model copy, with the
// member's bounded activity rows. The caller holds c.mu.
func (c *Coordinator) snapshotMemberLocked(registered *member) Teammate {
	info := registered.info
	info.Activity = c.activityForMemberLocked(info.ID)
	return info
}

// stateLocked builds the deep read-model snapshot. The caller holds c.mu.
func (c *Coordinator) stateLocked() TeamState {
	state := TeamState{
		Revision: c.revision,
		Members:  make([]Teammate, len(c.members)),
		Activity: cloneActivities(c.activities),
	}
	for index, registered := range c.members {
		state.Members[index] = c.snapshotMemberLocked(registered)
	}
	return state
}

// publish increments the revision, wakes subscribers, persists the new state and
// notifies the host. It must be called without c.mu held.
func (c *Coordinator) publish() {
	c.mu.Lock()
	c.revision++
	state := c.stateLocked()
	if c.wake != nil {
		close(c.wake)
	}
	c.wake = make(chan struct{})
	c.mu.Unlock()

	if c.store != nil {
		c.saveMu.Lock()
		c.mu.Lock()
		pending := state.Revision > c.savedRevision
		if pending {
			c.savedRevision = state.Revision
		}
		c.mu.Unlock()
		if pending {
			if err := c.store.Save(context.Background(), state); err != nil {
				c.reportError(err)
			}
		}
		c.saveMu.Unlock()
	}
	if c.notify != nil {
		c.notify()
	}
}

// recordRun applies one run outcome to the member if gen is still its current
// run. It is the single place the status machine moves to Done/Failed from a
// completed run.
func (c *Coordinator) recordRun(id string, gen uint64, result Result, err error) {
	c.mu.Lock()
	registered := c.membersByID[id]
	if registered == nil || registered.gen != gen || registered.observed >= gen ||
		registered.info.Status == StatusClosed {
		c.mu.Unlock()
		return
	}
	registered.observed = gen
	registered.info.Finished = c.now()
	registered.last = result
	registered.info.UsageTokens += maxInt(result.UsageTokens, 0)
	registered.info.Iterations += maxInt(result.Iterations, 0)
	if err != nil {
		if isContextError(err) {
			registered.info.Status = StatusInterrupted
			registered.info.Error = truncateText(err.Error(), c.maxErrorRunes)
			registered.lastErr = err
			c.addActivityLocked(registered, ActivityInterrupt,
				fmt.Sprintf("interrupted %s: %s", registered.info.Name, err.Error()), false)
		} else {
			registered.info.Status = StatusFailed
			registered.info.Error = truncateText(err.Error(), c.maxErrorRunes)
			registered.lastErr = err
			c.addActivityLocked(registered, ActivityError,
				fmt.Sprintf("%s failed: %s", registered.info.Name, err.Error()), true)
		}
	} else {
		registered.info.Status = StatusDone
		registered.info.Error = ""
		registered.info.LastOutput = strings.TrimSpace(result.Output)
		registered.lastErr = nil
		c.addActivityLocked(registered, ActivityFinish,
			fmt.Sprintf("%s finished: %s", registered.info.Name, result.Output), false)
	}
	c.mu.Unlock()
	c.publish()
}

// markMissing marks a member interrupted because the host no longer knows its
// child, invalidating whatever the old watcher may still report.
func (c *Coordinator) markMissing(id string, gen uint64) {
	c.mu.Lock()
	registered := c.membersByID[id]
	if registered == nil || registered.gen != gen || registered.info.Status == StatusClosed {
		c.mu.Unlock()
		return
	}
	registered.gen++
	registered.info.Status = StatusInterrupted
	registered.info.Finished = c.now()
	registered.info.Error = "child run is gone"
	c.addActivityLocked(registered, ActivityInterrupt,
		fmt.Sprintf("%s interrupted: child run is gone", registered.info.Name), true)
	c.mu.Unlock()
	c.publish()
}

// markInterrupted marks an in-flight run interrupted for an external reason
// (the caller's context ended), invalidating the stale watcher record.
func (c *Coordinator) markInterrupted(id string, gen uint64, reason error) {
	if reason == nil {
		reason = errors.New("run interrupted")
	}
	c.mu.Lock()
	registered := c.membersByID[id]
	if registered == nil || registered.gen != gen || registered.info.Status == StatusClosed {
		c.mu.Unlock()
		return
	}
	registered.gen++
	registered.info.Status = StatusInterrupted
	registered.info.Finished = c.now()
	registered.info.Error = truncateText(reason.Error(), c.maxErrorRunes)
	c.addActivityLocked(registered, ActivityInterrupt,
		fmt.Sprintf("interrupted %s: %s", registered.info.Name, reason.Error()), false)
	c.mu.Unlock()
	c.publish()
}

// now returns the coordinator clock.
func (c *Coordinator) now() time.Time {
	if c.clock == nil {
		return time.Now()
	}
	return c.clock()
}

// reportError forwards a non-fatal error to Options.OnError.
func (c *Coordinator) reportError(err error) {
	if err != nil && c.onError != nil {
		c.onError(err)
	}
}

// contextErr returns ctx.Err() for a non-nil context.
func contextErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

// isContextError reports whether err is a cancellation/deadline error.
func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// maxInt returns the larger of two ints.
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
