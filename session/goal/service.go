// Package goal implements the host-agnostic durable per-session goal service:
// snapshot event replay, compare-and-set revisions, round budgets, blocking and
// process-local activation. Hosts provide the event log and session-existence
// check.

package goal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

// goalEventType is the durable session event type of a goal snapshot.
const goalEventType = "goal/change"

// goalSnapshot is the persisted payload of one goal/change event: a complete
// snapshot, so replay never needs to fold deltas.
type goalSnapshot struct {
	SessionID string `json:"sessionId"`
	// Cleared marks a ClearGoal tombstone: the goal was removed, the event
	// history stays intact, and replay reads "no goal". ID/Revision describe the
	// cleared instance (revision is the cleared revision + 1).
	Cleared       bool           `json:"cleared,omitempty"`
	ID            string         `json:"id"`
	Revision      int            `json:"revision"`
	Objective     string         `json:"objective"`
	Phase         Phase          `json:"phase"`
	RoundsStarted int            `json:"roundsStarted"`
	MaxGoalRounds int            `json:"maxGoalRounds"`
	BlockedReason *BlockedReason `json:"blockedReason,omitempty"`
	UpdatedAt     time.Time      `json:"updatedAt"`
}

// goalState is the in-memory cached state of one session.
type goalState struct {
	// loaded records that the durable log was replayed for this session (the
	// goal fields are then authoritative).
	loaded bool
	goal   Goal
	// armed is the process-lifetime activation flag (never persisted).
	armed bool
}

// subscription is one Subscribe callback.
type subscription struct {
	fn func(sessionID string)
}

// service is the GoalService implementation.
type Service struct {
	mu sync.Mutex
	// store is the durable session event log (nil keeps goals in memory only;
	// that mode is for tests/embedders, never the product).
	store EventLog
	// sessions is the optional durable session-existence lookup (contracts.Store
	// in the product). nil skips the deleted-session guard (tests/embedders).
	sessions SessionExistsFunc
	logger   rig.Logger
	// defaultMaxRounds is applied when a create request passes 0.
	defaultMaxRounds int

	goals map[string]*goalState
	// forgotten tombstones sessions whose in-memory goal cache was dropped after
	// the durable session was deleted: reads report "no goal" and replay is
	// blocked for the rest of the process, so a leftover event or a late caller
	// cannot resurrect a deleted conversation's goal.
	forgotten map[string]struct{}
	// nextGoalSeq names goal instances created by this process; the timestamp
	// prefix keeps ids unique across restarts (an old completed goal's id must
	// never be reused by a new instance while the old event history remains).
	nextGoalSeq atomic.Uint64
	subscribers []*subscription
}

// newService builds the goal service; called by the module factory and tests.
// sessions is the optional durable session-existence lookup (nil = no check).
// New builds the goal service; sessions may be nil (no existence check).
func New(store EventLog, sessions SessionExistsFunc, logger rig.Logger, defaultMaxRounds int) *Service {
	if logger == nil {
		logger = discardLogger{}
	}
	if defaultMaxRounds <= 0 {
		defaultMaxRounds = DefaultMaxGoalRounds
	}
	return &Service{
		store:            store,
		sessions:         sessions,
		logger:           logger,
		defaultMaxRounds: defaultMaxRounds,
		goals:            make(map[string]*goalState),
		forgotten:        make(map[string]struct{}),
	}
}

// discardLogger drops goal log lines.
type discardLogger struct{}

func (discardLogger) Printf(string, ...any) {}

// Goal implements GoalService.
func (s *Service) Goal(sessionID string) (Goal, bool) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return Goal{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.ensureLoadedLocked(context.Background(), sessionID)
	if state == nil || state.goal.ID == "" {
		return Goal{}, false
	}
	return s.snapshotLocked(state), true
}

// CreateGoal implements GoalService.
func (s *Service) CreateGoal(ctx context.Context, sessionID, objective string, maxGoalRounds int) (Goal, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return Goal{}, fmt.Errorf("%w: session id is required", ErrGoalNotFound)
	}
	objective = strings.TrimSpace(objective)
	if objective == "" {
		return Goal{}, errors.New("goal: objective is required and must be non-empty")
	}
	if maxGoalRounds < 0 {
		return Goal{}, errors.New("goal: maxGoalRounds must be a positive integer")
	}
	if err := ctx.Err(); err != nil {
		return Goal{}, err
	}
	rounds := maxGoalRounds
	if rounds <= 0 {
		rounds = s.defaultMaxRounds
	}

	s.mu.Lock()
	state := s.ensureLoadedLocked(ctx, sessionID)
	if state == nil {
		s.mu.Unlock()
		return Goal{}, fmt.Errorf("%w: session %q was deleted", ErrGoalNotFound, sessionID)
	}
	if !state.loaded {
		s.mu.Unlock()
		return Goal{}, fmt.Errorf("goal: cannot read the durable goal state of session %q", sessionID)
	}
	if state.goal.ID != "" && state.goal.Phase != PhaseComplete {
		existing := state.goal
		s.mu.Unlock()
		return Goal{}, fmt.Errorf("%w: session %q has goal %q in phase %q", ErrGoalConflict, sessionID, existing.ID, existing.Phase)
	}
	next := Goal{
		SessionID:     sessionID,
		ID:            s.newGoalID(),
		Revision:      1,
		Objective:     objective,
		Phase:         PhaseActive,
		RoundsStarted: 0,
		MaxGoalRounds: rounds,
		UpdatedAt:     time.Now(),
	}
	if err := s.commitLocked(ctx, state, next, true); err != nil {
		s.mu.Unlock()
		return Goal{}, err
	}
	snapshot := s.snapshotLocked(state)
	s.mu.Unlock()
	s.notify(sessionID)
	return snapshot, nil
}

// UpdateGoal implements GoalService.
func (s *Service) UpdateGoal(ctx context.Context, sessionID string, ref Ref, update Update) (Goal, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return Goal{}, fmt.Errorf("%w: session id is required", ErrGoalNotFound)
	}
	if err := ctx.Err(); err != nil {
		return Goal{}, err
	}

	s.mu.Lock()
	state := s.ensureLoadedLocked(ctx, sessionID)
	if state == nil {
		s.mu.Unlock()
		return Goal{}, fmt.Errorf("%w: session %q was deleted", ErrGoalNotFound, sessionID)
	}
	if !state.loaded {
		s.mu.Unlock()
		return Goal{}, fmt.Errorf("goal: cannot read the durable goal state of session %q", sessionID)
	}
	if state.goal.ID == "" {
		s.mu.Unlock()
		return Goal{}, fmt.Errorf("%w: session %q", ErrGoalNotFound, sessionID)
	}
	current := state.goal
	if ref.ID != current.ID || ref.Revision != current.Revision {
		s.mu.Unlock()
		return Goal{}, fmt.Errorf("%w: session %q wants %q revision %d; current is %q revision %d",
			ErrGoalStaleRef, sessionID, ref.ID, ref.Revision, current.ID, current.Revision)
	}
	next := current
	armed := state.armed
	switch update.Action {
	case ActionEdit:
		objective := strings.TrimSpace(update.Objective)
		if update.Objective != "" && objective == "" {
			s.mu.Unlock()
			return Goal{}, errors.New("goal: objective must be non-empty after trimming")
		}
		if update.MaxGoalRounds < 0 {
			s.mu.Unlock()
			return Goal{}, errors.New("goal: maxGoalRounds must be a positive integer")
		}
		if objective == "" && update.MaxGoalRounds == 0 {
			s.mu.Unlock()
			return Goal{}, errors.New("goal: edit requires objective and/or maxGoalRounds")
		}
		if update.BlockedReason != "" {
			s.mu.Unlock()
			return Goal{}, errors.New("goal: blockedReason is valid only with action blocked")
		}
		if objective != "" {
			next.Objective = objective
		}
		if update.MaxGoalRounds != 0 {
			next.MaxGoalRounds = update.MaxGoalRounds
		}
	case ActionPause:
		if current.Phase != PhaseActive {
			s.mu.Unlock()
			return Goal{}, fmt.Errorf("%w: cannot pause goal %q from phase %q", ErrGoalInvalidTransition, current.ID, current.Phase)
		}
		next.Phase = PhasePaused
		armed = false
	case ActionResume:
		switch current.Phase {
		case PhaseActive:
			// Re-arm: a user Stop or a restart disarms the goal WITHOUT
			// changing its persisted phase, and the human "continue goal"
			// action arms it again. Activation is process state only, so this
			// is not a durable change (no event, no revision bump).
			if state.armed {
				s.mu.Unlock()
				return Goal{}, fmt.Errorf("%w: goal %q is already active and armed", ErrGoalInvalidTransition, current.ID)
			}
			state.armed = true
			snapshot := s.snapshotLocked(state)
			s.mu.Unlock()
			s.notify(sessionID)
			return snapshot, nil
		case PhasePaused, PhaseBlocked:
		default:
			s.mu.Unlock()
			return Goal{}, fmt.Errorf("%w: cannot resume goal %q from phase %q", ErrGoalInvalidTransition, current.ID, current.Phase)
		}
		if current.RoundsStarted >= current.MaxGoalRounds {
			s.mu.Unlock()
			return Goal{}, fmt.Errorf("%w: goal %q exhausted %d rounds; raise maxGoalRounds before resuming", ErrGoalInvalidTransition, current.ID, current.MaxGoalRounds)
		}
		next.Phase = PhaseActive
		next.BlockedReason = nil
		armed = true
	case ActionComplete:
		switch current.Phase {
		case PhaseActive, PhasePaused, PhaseBlocked:
		default:
			s.mu.Unlock()
			return Goal{}, fmt.Errorf("%w: cannot complete goal %q from phase %q", ErrGoalInvalidTransition, current.ID, current.Phase)
		}
		next.Phase = PhaseComplete
		next.BlockedReason = nil
		armed = false
	case ActionBlocked:
		if current.Phase != PhaseActive {
			s.mu.Unlock()
			return Goal{}, fmt.Errorf("%w: cannot block goal %q from phase %q", ErrGoalInvalidTransition, current.ID, current.Phase)
		}
		reason := strings.TrimSpace(update.BlockedReason)
		if reason == "" {
			s.mu.Unlock()
			return Goal{}, errors.New("goal: blocked requires a non-empty reason")
		}
		next.Phase = PhaseBlocked
		next.BlockedReason = &BlockedReason{Code: BlockedCodeModelReported, Message: reason}
		armed = false
	default:
		s.mu.Unlock()
		return Goal{}, fmt.Errorf("%w: unknown action %q", ErrGoalInvalidTransition, update.Action)
	}
	next.Revision = current.Revision + 1
	next.UpdatedAt = time.Now()
	next.Activation = ActivationDisarmed
	if err := s.commitLocked(ctx, state, next, armed); err != nil {
		s.mu.Unlock()
		return Goal{}, err
	}
	snapshot := s.snapshotLocked(state)
	s.mu.Unlock()
	s.notify(sessionID)
	return snapshot, nil
}

// AdmitRound implements GoalService. At the cap it persists the
// goal as blocked (code round-limit) and returns ErrGoalRoundLimit: the round
// is never admitted, so the driver cannot exceed the budget.
func (s *Service) AdmitRound(ctx context.Context, sessionID string) (Goal, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return Goal{}, fmt.Errorf("%w: session id is required", ErrGoalNotFound)
	}
	if err := ctx.Err(); err != nil {
		return Goal{}, err
	}

	s.mu.Lock()
	state := s.ensureLoadedLocked(ctx, sessionID)
	if state == nil {
		s.mu.Unlock()
		return Goal{}, fmt.Errorf("%w: session %q was deleted", ErrGoalNotFound, sessionID)
	}
	if !state.loaded {
		s.mu.Unlock()
		return Goal{}, fmt.Errorf("goal: cannot read the durable goal state of session %q", sessionID)
	}
	if state.goal.ID == "" {
		s.mu.Unlock()
		return Goal{}, fmt.Errorf("%w: session %q", ErrGoalNotFound, sessionID)
	}
	current := state.goal
	if current.Phase != PhaseActive {
		s.mu.Unlock()
		return Goal{}, fmt.Errorf("%w: goal %q is %q, not active", ErrGoalInvalidTransition, current.ID, current.Phase)
	}
	// Activation is checked IN the same critical section as the admission: a
	// user Stop/disarm that landed between the driver's Goal() read and this
	// call must not consume a round. A human resume (active+disarmed → resume
	// re-arms) is required before the driver may advance the goal again.
	if !state.armed {
		s.mu.Unlock()
		return Goal{}, fmt.Errorf("%w: goal %q is disarmed; resume it before admitting a round", ErrGoalInvalidTransition, current.ID)
	}
	if current.RoundsStarted >= current.MaxGoalRounds {
		next := current
		next.Phase = PhaseBlocked
		next.BlockedReason = &BlockedReason{
			Code:    BlockedCodeRoundLimit,
			Message: fmt.Sprintf("%s (%d/%d)", RoundLimitMessage, current.RoundsStarted, current.MaxGoalRounds),
		}
		next.Revision = current.Revision + 1
		next.UpdatedAt = time.Now()
		if err := s.commitLocked(ctx, state, next, false); err != nil {
			s.mu.Unlock()
			return Goal{}, err
		}
		s.mu.Unlock()
		s.notify(sessionID)
		return Goal{}, fmt.Errorf("%w: goal %q reached %d/%d", ErrGoalRoundLimit, current.ID, current.RoundsStarted, current.MaxGoalRounds)
	}
	next := current
	next.RoundsStarted = current.RoundsStarted + 1
	next.Revision = current.Revision + 1
	next.UpdatedAt = time.Now()
	if err := s.commitLocked(ctx, state, next, state.armed); err != nil {
		s.mu.Unlock()
		return Goal{}, err
	}
	snapshot := s.snapshotLocked(state)
	s.mu.Unlock()
	s.notify(sessionID)
	return snapshot, nil
}

// RollbackRound refunds one admitted continuation round whose run never
// started (BE-10, 2026-11 audit): AdmitRound persists RoundsStarted+1 BEFORE
// the driver starts the send, and a failed send used to leave the round
// consumed — the goal burned a round on a turn that never ran. The refund
// applies only when the goal is still active and still sits at exactly the
// admitted round; a goal that advanced, blocked or completed meanwhile keeps
// its durable truth (no-op). The refund is persisted as a new revision so a
// restart agrees, and the activation (armed/disarmed) is preserved.
func (s *Service) RollbackRound(ctx context.Context, sessionID string, round int) error {
	if ctx == nil {
		ctx = context.Background()
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return fmt.Errorf("%w: session id is required", ErrGoalNotFound)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	state := s.ensureLoadedLocked(ctx, sessionID)
	if state == nil {
		s.mu.Unlock()
		return fmt.Errorf("%w: session %q was deleted", ErrGoalNotFound, sessionID)
	}
	if !state.loaded {
		s.mu.Unlock()
		return fmt.Errorf("goal: cannot read the durable goal state of session %q", sessionID)
	}
	current := state.goal
	if current.ID == "" {
		s.mu.Unlock()
		return fmt.Errorf("%w: session %q", ErrGoalNotFound, sessionID)
	}
	if round <= 0 || current.Phase != PhaseActive || current.RoundsStarted != round {
		// Stale or moved-on: nothing to refund.
		s.mu.Unlock()
		return nil
	}
	next := current
	next.RoundsStarted = current.RoundsStarted - 1
	next.Revision = current.Revision + 1
	next.UpdatedAt = time.Now()
	if err := s.commitLocked(ctx, state, next, state.armed); err != nil {
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()
	s.notify(sessionID)
	return nil
}

// Disarm implements GoalService: it turns automatic continuation off
// in this process without touching the persisted phase/revision.
func (s *Service) Disarm(sessionID string) (Goal, bool) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return Goal{}, false
	}
	s.mu.Lock()
	state := s.ensureLoadedLocked(context.Background(), sessionID)
	if state == nil || !state.loaded || state.goal.ID == "" {
		s.mu.Unlock()
		return Goal{}, false
	}
	wasArmed := state.armed
	state.armed = false
	snapshot := s.snapshotLocked(state)
	s.mu.Unlock()
	if wasArmed {
		s.notify(sessionID)
	}
	return snapshot, true
}

// ClearGoal implements GoalService: a revision-checked tombstone that
// removes the current goal while preserving the durable event history. A stale
// ref changes nothing; a session without a goal reports (false, nil).
func (s *Service) ClearGoal(ctx context.Context, sessionID string, ref Ref) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return false, fmt.Errorf("%w: session id is required", ErrGoalNotFound)
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}

	s.mu.Lock()
	state := s.ensureLoadedLocked(ctx, sessionID)
	if state == nil {
		s.mu.Unlock()
		return false, fmt.Errorf("%w: session %q was deleted", ErrGoalNotFound, sessionID)
	}
	if !state.loaded {
		s.mu.Unlock()
		return false, fmt.Errorf("goal: cannot read the durable goal state of session %q", sessionID)
	}
	current := state.goal
	if current.ID == "" {
		s.mu.Unlock()
		return false, nil
	}
	if ref.ID != current.ID || ref.Revision != current.Revision {
		s.mu.Unlock()
		return false, fmt.Errorf("%w: session %q wants %q revision %d; current is %q revision %d",
			ErrGoalStaleRef, sessionID, ref.ID, ref.Revision, current.ID, current.Revision)
	}
	tombstone := goalSnapshot{
		SessionID: sessionID,
		ID:        current.ID,
		Revision:  current.Revision + 1,
		Cleared:   true,
		UpdatedAt: time.Now(),
	}
	if err := s.appendSnapshotLocked(ctx, tombstone); err != nil {
		s.mu.Unlock()
		return false, err
	}
	state.goal = Goal{}
	state.armed = false
	state.loaded = true
	s.mu.Unlock()
	s.notify(sessionID)
	return true, nil
}

// Forget drops the process cache and activation of a session whose durable
// session row was deleted (agent.DeleteSession calls it through an optional
// interface). It also tombstones the session for this process so a leftover goal
// event or a late driver read cannot resurrect the deleted conversation's goal.
// It is intentionally not part of GoalService: only the agent's
// delete path needs it.
func (s *Service) Forget(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	s.mu.Lock()
	delete(s.goals, sessionID)
	s.forgotten[sessionID] = struct{}{}
	s.mu.Unlock()
}

// CopyGoal copies one session's goal into another session (used by the agent's
// session fork). It is intentionally NOT part of GoalService: only the
// fork path needs it and the agent calls it through a type assertion.
//
// DSH semantics: the copy keeps the objective/phase/rounds/max/blocked reason,
// gets a fresh goal ID at revision 1 and is DISARMED (a forked session must not
// continue a goal without a human resume). The source goal (and its activation)
// is untouched. A session without a goal returns (zero, false, nil); a target
// with a non-complete goal is ErrGoalConflict; a missing target session is
// ErrGoalNotFound and nothing is written.
func (s *Service) CopyGoal(ctx context.Context, fromSessionID, toSessionID string) (Goal, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	fromSessionID = strings.TrimSpace(fromSessionID)
	toSessionID = strings.TrimSpace(toSessionID)
	if fromSessionID == "" || toSessionID == "" {
		return Goal{}, false, fmt.Errorf("%w: source and target session ids are required", ErrGoalNotFound)
	}
	if fromSessionID == toSessionID {
		return Goal{}, false, errors.New("goal: cannot copy a goal onto its own session")
	}
	if err := ctx.Err(); err != nil {
		return Goal{}, false, err
	}

	s.mu.Lock()
	sourceState := s.ensureLoadedLocked(ctx, fromSessionID)
	if sourceState == nil || !sourceState.loaded {
		s.mu.Unlock()
		return Goal{}, false, fmt.Errorf("goal: cannot read the durable goal state of session %q", fromSessionID)
	}
	if sourceState.goal.ID == "" {
		s.mu.Unlock()
		return Goal{}, false, nil
	}
	targetState := s.ensureLoadedLocked(ctx, toSessionID)
	if targetState == nil {
		s.mu.Unlock()
		return Goal{}, false, fmt.Errorf("%w: target session %q was deleted", ErrGoalNotFound, toSessionID)
	}
	if !targetState.loaded {
		s.mu.Unlock()
		return Goal{}, false, fmt.Errorf("goal: cannot read the durable goal state of session %q", toSessionID)
	}
	if targetState.goal.ID != "" && targetState.goal.Phase != PhaseComplete {
		existing := targetState.goal
		s.mu.Unlock()
		return Goal{}, false, fmt.Errorf("%w: target session %q already has goal %q in phase %q",
			ErrGoalConflict, toSessionID, existing.ID, existing.Phase)
	}
	source := sourceState.goal
	next := Goal{
		SessionID:     toSessionID,
		ID:            s.newGoalID(),
		Revision:      1,
		Objective:     source.Objective,
		Phase:         source.Phase,
		RoundsStarted: source.RoundsStarted,
		MaxGoalRounds: source.MaxGoalRounds,
		UpdatedAt:     time.Now(),
	}
	if source.BlockedReason != nil {
		reason := *source.BlockedReason
		next.BlockedReason = &reason
	}
	if err := s.commitLocked(ctx, targetState, next, false); err != nil {
		s.mu.Unlock()
		return Goal{}, false, err
	}
	snapshot := s.snapshotLocked(targetState)
	s.mu.Unlock()
	s.notify(toSessionID)
	return snapshot, true, nil
}

// Subscribe implements GoalService.
func (s *Service) Subscribe(fn func(sessionID string)) (func(), error) {
	if fn == nil {
		return func() {}, nil
	}
	listener := &subscription{fn: fn}
	s.mu.Lock()
	s.subscribers = append(s.subscribers, listener)
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			for index, candidate := range s.subscribers {
				if candidate == listener {
					s.subscribers = append(s.subscribers[:index], s.subscribers[index+1:]...)
					return
				}
			}
		})
	}, nil
}

// --- durable log ------------------------------------------------------------

// ensureLoadedLocked replays the durable goal log for sessionID once. The caller
// holds s.mu; a read failure is logged and leaves the cached state empty (the
// next access retries the replay). It returns nil when the session id is empty.
func (s *Service) ensureLoadedLocked(ctx context.Context, sessionID string) *goalState {
	if sessionID == "" {
		return nil
	}
	if _, forgotten := s.forgotten[sessionID]; forgotten {
		// A deleted session's goal must never come back (not even from a
		// leftover event): every read reports "no goal" for the rest of the
		// process.
		return nil
	}
	state, ok := s.goals[sessionID]
	if ok && state.loaded {
		return state
	}
	if state == nil {
		state = &goalState{}
		s.goals[sessionID] = state
	}
	if s.store == nil {
		state.loaded = true
		return state
	}
	if ctx == nil {
		ctx = context.Background()
	}
	events, err := s.store.Events(ctx, sessionID, 0, 0)
	if err != nil {
		s.logger.Printf("[goal] replay %s: %v", sessionID, err)
		return state
	}
	for _, event := range events {
		if event.Type != goalEventType {
			continue
		}
		snapshot, ok := decodeGoalSnapshot(event.Payload)
		if !ok {
			s.logger.Printf("[goal] replay %s: skipping malformed %s event", sessionID, goalEventType)
			continue
		}
		if snapshot.Cleared {
			// ClearGoal tombstone: the history is preserved but the newest
			// event says there is no goal. A later CreateGoal appends a fresh
			// instance that wins the replay again.
			state.goal = Goal{}
			continue
		}
		state.goal = replayGoal(sessionID, snapshot)
	}
	state.loaded = true
	return state
}

// decodeGoalSnapshot decodes one persisted snapshot; malformed payloads are
// skipped rather than failing the replay (a corrupt event must not hide an
// older valid one).
func decodeGoalSnapshot(payload []byte) (goalSnapshot, bool) {
	if len(payload) == 0 {
		return goalSnapshot{}, false
	}
	var snapshot goalSnapshot
	if err := json.Unmarshal(payload, &snapshot); err != nil {
		return goalSnapshot{}, false
	}
	if snapshot.Cleared {
		// A ClearGoal tombstone is valid even though it removes the goal.
		return snapshot, true
	}
	if strings.TrimSpace(snapshot.ID) == "" {
		return goalSnapshot{}, false
	}
	return snapshot, true
}

// replayGoal converts a persisted snapshot back into a cached goal. Activation
// is never replayed: every goal loaded after a restart is disarmed.
func replayGoal(sessionID string, snapshot goalSnapshot) Goal {
	revision := snapshot.Revision
	if revision < 1 {
		revision = 1
	}
	maxRounds := snapshot.MaxGoalRounds
	if maxRounds < 1 {
		maxRounds = DefaultMaxGoalRounds
	}
	phase := snapshot.Phase
	if phase == "" {
		phase = PhaseActive
	}
	goal := Goal{
		SessionID:     sessionID,
		ID:            strings.TrimSpace(snapshot.ID),
		Revision:      revision,
		Objective:     strings.TrimSpace(snapshot.Objective),
		Phase:         phase,
		RoundsStarted: snapshot.RoundsStarted,
		MaxGoalRounds: maxRounds,
		BlockedReason: snapshot.BlockedReason,
		UpdatedAt:     snapshot.UpdatedAt,
	}
	if goal.RoundsStarted < 0 {
		goal.RoundsStarted = 0
	}
	if goal.Phase != PhaseBlocked {
		goal.BlockedReason = nil
	}
	return goal
}

// commitLocked persists next and, only after the durable append succeeds,
// publishes it into the cached state. The caller holds s.mu; a persist failure
// leaves both the cache and the revision untouched (the caller reports it).
func (s *Service) commitLocked(ctx context.Context, state *goalState, next Goal, armed bool) error {
	if err := s.appendSnapshotLocked(ctx, goalSnapshot{
		SessionID:     next.SessionID,
		ID:            next.ID,
		Revision:      next.Revision,
		Objective:     next.Objective,
		Phase:         next.Phase,
		RoundsStarted: next.RoundsStarted,
		MaxGoalRounds: next.MaxGoalRounds,
		BlockedReason: next.BlockedReason,
		UpdatedAt:     next.UpdatedAt,
	}); err != nil {
		return err
	}
	state.goal = next
	state.loaded = true
	state.armed = armed
	return nil
}

// appendSnapshotLocked appends one goal/change event after verifying the durable
// session still exists. The existence check is essential: AppendEvent recreates
// a deleted session's row (ensureSessionTx), so a goal event for a deleted
// conversation would resurrect it. A missing session returns ErrGoalNotFound
// and nothing is written. The caller holds s.mu.
func (s *Service) appendSnapshotLocked(ctx context.Context, snapshot goalSnapshot) error {
	if err := s.requireSessionLocked(ctx, snapshot.SessionID); err != nil {
		return err
	}
	if s.store == nil {
		return nil
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("goal: encode snapshot of session %q: %w", snapshot.SessionID, err)
	}
	eventTime := snapshot.UpdatedAt
	if eventTime.IsZero() {
		eventTime = time.Now()
	}
	if err := s.store.AppendEvent(ctx, rig.SessionEvent{
		SessionID: snapshot.SessionID,
		Type:      goalEventType,
		Time:      eventTime,
		Payload:   payload,
	}); err != nil {
		return fmt.Errorf("goal: persist goal of session %q: %w", snapshot.SessionID, err)
	}
	return nil
}

// requireSessionLocked fails closed when the durable session no longer exists
// (deleted conversation). A nil lookup keeps the historical in-memory behavior
// for tests/embedders; a lookup error is returned so the caller does not write
// a goal event it cannot verify. The caller holds s.mu.
func (s *Service) requireSessionLocked(ctx context.Context, sessionID string) error {
	if s.sessions == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	found, err := s.sessions(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("goal: check session %q: %w", sessionID, err)
	}
	if !found {
		return fmt.Errorf("%w: session %q was deleted", ErrGoalNotFound, sessionID)
	}
	return nil
}

// snapshotLocked builds the caller-facing view of one cached goal: the
// persisted fields plus the process-lifetime activation observation.
func (s *Service) snapshotLocked(state *goalState) Goal {
	goal := state.goal
	goal.Activation = ActivationDisarmed
	if state.armed && goal.Phase == PhaseActive {
		goal.Activation = ActivationArmed
	}
	if goal.Phase == PhaseBlocked && goal.BlockedReason != nil {
		reason := *goal.BlockedReason
		goal.BlockedReason = &reason
	} else if goal.Phase != PhaseBlocked {
		goal.BlockedReason = nil
	}
	return goal
}

// newGoalID returns a goal instance id unique across restarts.
func (s *Service) newGoalID() string {
	return fmt.Sprintf("goal-%d-%d", time.Now().UnixNano(), s.nextGoalSeq.Add(1))
}

// notify calls every subscriber outside the service lock; a panicking callback
// is contained so a UI bug cannot break goal mutations.
func (s *Service) notify(sessionID string) {
	s.mu.Lock()
	subscribers := append([]*subscription(nil), s.subscribers...)
	s.mu.Unlock()
	for _, listener := range subscribers {
		if listener == nil || listener.fn == nil {
			continue
		}
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					s.logger.Printf("[goal] subscriber panicked: %v", recovered)
				}
			}()
			listener.fn(sessionID)
		}()
	}
}

// --- module -----------------------------------------------------------------
