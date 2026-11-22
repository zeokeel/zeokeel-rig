package goal

// Tests for the durable per-session goal backend: persistence/replay, revision
// CAS, the 256-round default, round-limit blocking, activation, concurrency and
// subscription. No database is involved: a tiny in-memory event log implements
// the two-method store view the service needs.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	rig "github.com/zeokeel/zeokeel-rig"
)

// fakeEventStore is an in-memory session event log.
type fakeEventStore struct {
	mu        sync.Mutex
	events    map[string][]rig.SessionEvent
	nextSeq   int64
	appendErr error
	readErr   error
	appends   int
}

func newFakeEventStore() *fakeEventStore {
	return &fakeEventStore{events: make(map[string][]rig.SessionEvent)}
}

func (s *fakeEventStore) AppendEvent(_ context.Context, event rig.SessionEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.appendErr != nil {
		return s.appendErr
	}
	s.appends++
	s.nextSeq++
	event.Seq = s.nextSeq
	if event.Time.IsZero() {
		event.Time = time.Now()
	}
	event.Payload = append(json.RawMessage(nil), event.Payload...)
	s.events[event.SessionID] = append(s.events[event.SessionID], event)
	return nil
}

func (s *fakeEventStore) Events(_ context.Context, sessionID string, from, to int64) ([]rig.SessionEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readErr != nil {
		return nil, s.readErr
	}
	out := make([]rig.SessionEvent, 0, len(s.events[sessionID]))
	for _, event := range s.events[sessionID] {
		if event.Seq < from {
			continue
		}
		if to > 0 && event.Seq > to {
			continue
		}
		out = append(out, event)
	}
	return out, nil
}

// appendCount returns how many events were accepted.
func (s *fakeEventStore) appendCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appends
}

// deleteEvents simulates sessionstore.DeleteSession removing the session's
// events with the row.
func (s *fakeEventStore) deleteEvents(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.events, sessionID)
}

// goalEvents returns the durable goal/change events of one session.
func (s *fakeEventStore) goalEvents(sessionID string) []rig.SessionEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]rig.SessionEvent, 0, 4)
	for _, event := range s.events[sessionID] {
		if event.Type == goalEventType {
			out = append(out, event)
		}
	}
	return out
}

func newTestService(store EventLog) *Service {
	return New(store, nil, nil, 0)
}

// fakeSessionLookup is the durable session-existence seam: the product passes
// the session store, tests toggle existence to simulate a deleted conversation.
type fakeSessionLookup struct {
	mu     sync.Mutex
	exists map[string]bool
	err    error
}

func newFakeSessionLookup(sessionIDs ...string) *fakeSessionLookup {
	lookup := &fakeSessionLookup{exists: make(map[string]bool, len(sessionIDs))}
	for _, sessionID := range sessionIDs {
		lookup.exists[sessionID] = true
	}
	return lookup
}

func (l *fakeSessionLookup) Exists(_ context.Context, sessionID string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return false, l.err
	}
	return l.exists[sessionID], nil
}

func (l *fakeSessionLookup) existsFunc() SessionExistsFunc { return l.Exists }

// setExists marks a session existing/deleted.
func (l *fakeSessionLookup) setExists(sessionID string, exists bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if exists {
		l.exists[sessionID] = true
		return
	}
	delete(l.exists, sessionID)
}

// newTestServiceWithSessions builds a service with the session-existence seam.
func newTestServiceWithSessions(store EventLog, lookup *fakeSessionLookup) *Service {
	return New(store, lookup.existsFunc(), nil, 0)
}

func mustCreate(t *testing.T, service *Service, sessionID, objective string, rounds int) Goal {
	t.Helper()
	goal, err := service.CreateGoal(context.Background(), sessionID, objective, rounds)
	if err != nil {
		t.Fatalf("CreateGoal(%s): %v", objective, err)
	}
	return goal
}

func TestCreateGoalPersistsFullSnapshotAndArms(t *testing.T) {
	store := newFakeEventStore()
	service := newTestService(store)

	goal := mustCreate(t, service, "sess-1", "  ship the release  ", 0)
	if goal.SessionID != "sess-1" || goal.ID == "" {
		t.Fatalf("created goal = %+v, want session and id", goal)
	}
	if goal.Revision != 1 {
		t.Fatalf("revision = %d, want 1", goal.Revision)
	}
	if goal.Objective != "ship the release" {
		t.Fatalf("objective = %q, want trimmed text", goal.Objective)
	}
	if goal.Phase != GoalPhaseActive || goal.Activation != GoalActivationArmed {
		t.Fatalf("phase/activation = %q/%q, want active/armed", goal.Phase, goal.Activation)
	}
	if goal.RoundsStarted != 0 || goal.MaxGoalRounds != DefaultMaxGoalRounds {
		t.Fatalf("rounds = %d/%d, want 0/256", goal.RoundsStarted, goal.MaxGoalRounds)
	}

	events := store.goalEvents("sess-1")
	if len(events) != 1 {
		t.Fatalf("goal events = %d, want 1", len(events))
	}
	var payload goalSnapshot
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if payload.ID != goal.ID || payload.Revision != 1 || payload.Objective != "ship the release" ||
		payload.Phase != GoalPhaseActive || payload.MaxGoalRounds != DefaultMaxGoalRounds {
		t.Fatalf("persisted snapshot = %+v, want the full goal", payload)
	}
}

func TestCreateGoalHonoursPositiveBudgetAndRejectsConflicts(t *testing.T) {
	store := newFakeEventStore()
	service := newTestService(store)

	goal := mustCreate(t, service, "sess-1", "goal one", 7)
	if goal.MaxGoalRounds != 7 {
		t.Fatalf("maxGoalRounds = %d, want 7", goal.MaxGoalRounds)
	}
	if _, err := service.CreateGoal(context.Background(), "sess-1", "second", 0); !errors.Is(err, ErrGoalConflict) {
		t.Fatalf("CreateGoal over a live goal = %v, want ErrGoalConflict", err)
	}
	if _, err := service.CreateGoal(context.Background(), "sess-1", "", 0); err == nil {
		t.Fatal("CreateGoal with an empty objective succeeded")
	}

	// A completed goal can be replaced by a new instance with a fresh revision.
	if _, err := service.UpdateGoal(context.Background(), "sess-1", GoalRef{ID: goal.ID, Revision: goal.Revision},
		GoalUpdate{Action: GoalActionComplete}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	replacement := mustCreate(t, service, "sess-1", "goal two", 0)
	if replacement.ID == goal.ID {
		t.Fatalf("replacement id = %q, want a new goal id", replacement.ID)
	}
	if replacement.Revision != 1 || replacement.Phase != GoalPhaseActive {
		t.Fatalf("replacement = %+v, want revision 1 active", replacement)
	}
}

func TestGoalReplaySurvivesRestartAndIsDisarmed(t *testing.T) {
	store := newFakeEventStore()
	first := newTestService(store)
	goal := mustCreate(t, first, "sess-1", "keep going", 5)
	if _, err := first.AdmitRound(context.Background(), "sess-1"); err != nil {
		t.Fatalf("AdmitRound: %v", err)
	}

	// A new process replays the same durable log: the persisted fields survive,
	// activation does not (DSH: every goal is disarmed after a restart).
	second := newTestService(store)
	restored, ok := second.Goal("sess-1")
	if !ok {
		t.Fatal("Goal after restart = not found")
	}
	if restored.ID != goal.ID || restored.Objective != "keep going" || restored.MaxGoalRounds != 5 {
		t.Fatalf("restored goal = %+v, want the persisted fields", restored)
	}
	if restored.Revision != 2 || restored.RoundsStarted != 1 {
		t.Fatalf("restored revision/rounds = %d/%d, want 2/1", restored.Revision, restored.RoundsStarted)
	}
	if restored.Activation != GoalActivationDisarmed {
		t.Fatalf("activation after restart = %q, want disarmed", restored.Activation)
	}
	// Leftover activation from the first process never leaks in.
	if _, ok := first.Disarm("sess-1"); !ok {
		t.Fatal("Disarm on the first process failed")
	}
	again, _ := second.Goal("sess-1")
	if again.Activation != GoalActivationDisarmed {
		t.Fatalf("activation = %q, want disarmed", again.Activation)
	}
}

func TestUpdateGoalStaleRefIsRejected(t *testing.T) {
	store := newFakeEventStore()
	service := newTestService(store)
	goal := mustCreate(t, service, "sess-1", "objective", 0)

	if _, err := service.UpdateGoal(context.Background(), "sess-1",
		GoalRef{ID: goal.ID, Revision: goal.Revision + 1},
		GoalUpdate{Action: GoalActionPause}); !errors.Is(err, ErrGoalStaleRef) {
		t.Fatalf("stale revision = %v, want ErrGoalStaleRef", err)
	}
	if _, err := service.UpdateGoal(context.Background(), "sess-1",
		GoalRef{ID: "goal-other", Revision: goal.Revision},
		GoalUpdate{Action: GoalActionPause}); !errors.Is(err, ErrGoalStaleRef) {
		t.Fatalf("stale id = %v, want ErrGoalStaleRef", err)
	}
	current, _ := service.Goal("sess-1")
	if current.Phase != GoalPhaseActive || current.Revision != goal.Revision {
		t.Fatalf("stale update changed the goal: %+v", current)
	}
}

func TestUpdateGoalLifecycleAndActivation(t *testing.T) {
	store := newFakeEventStore()
	service := newTestService(store)
	goal := mustCreate(t, service, "sess-1", "objective", 4)
	ref := GoalRef{ID: goal.ID, Revision: goal.Revision}

	paused, err := service.UpdateGoal(context.Background(), "sess-1", ref, GoalUpdate{Action: GoalActionPause})
	if err != nil {
		t.Fatalf("pause: %v", err)
	}
	if paused.Phase != GoalPhasePaused || paused.Activation != GoalActivationDisarmed {
		t.Fatalf("paused = %+v, want paused/disarmed", paused)
	}
	if paused.Revision != 2 {
		t.Fatalf("revision after pause = %d, want 2", paused.Revision)
	}
	// Pausing again is a no-op error, never a second transition.
	if _, err := service.UpdateGoal(context.Background(), "sess-1",
		GoalRef{ID: paused.ID, Revision: paused.Revision},
		GoalUpdate{Action: GoalActionPause}); !errors.Is(err, ErrGoalInvalidTransition) {
		t.Fatalf("pause twice = %v, want ErrGoalInvalidTransition", err)
	}

	resumed, err := service.UpdateGoal(context.Background(), "sess-1",
		GoalRef{ID: paused.ID, Revision: paused.Revision},
		GoalUpdate{Action: GoalActionResume})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed.Phase != GoalPhaseActive || resumed.Activation != GoalActivationArmed {
		t.Fatalf("resumed = %+v, want active/armed", resumed)
	}

	edited, err := service.UpdateGoal(context.Background(), "sess-1",
		GoalRef{ID: resumed.ID, Revision: resumed.Revision},
		GoalUpdate{Action: GoalActionEdit, Objective: "new objective", MaxGoalRounds: 9})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if edited.Objective != "new objective" || edited.MaxGoalRounds != 9 || edited.Revision != 4 {
		t.Fatalf("edited = %+v, want the new objective/cap and revision 4", edited)
	}

	completed, err := service.UpdateGoal(context.Background(), "sess-1",
		GoalRef{ID: edited.ID, Revision: edited.Revision},
		GoalUpdate{Action: GoalActionComplete})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if completed.Phase != GoalPhaseComplete || completed.Activation != GoalActivationDisarmed {
		t.Fatalf("completed = %+v, want complete/disarmed", completed)
	}
	if _, err := service.UpdateGoal(context.Background(), "sess-1",
		GoalRef{ID: completed.ID, Revision: completed.Revision},
		GoalUpdate{Action: GoalActionResume}); !errors.Is(err, ErrGoalInvalidTransition) {
		t.Fatalf("resume completed = %v, want ErrGoalInvalidTransition", err)
	}
}

func TestBlockedRequiresReasonAndDisarms(t *testing.T) {
	store := newFakeEventStore()
	service := newTestService(store)
	goal := mustCreate(t, service, "sess-1", "objective", 0)
	ref := GoalRef{ID: goal.ID, Revision: goal.Revision}

	if _, err := service.UpdateGoal(context.Background(), "sess-1", ref,
		GoalUpdate{Action: GoalActionBlocked}); err == nil {
		t.Fatal("blocked without a reason succeeded")
	}
	blocked, err := service.UpdateGoal(context.Background(), "sess-1", ref,
		GoalUpdate{Action: GoalActionBlocked, BlockedReason: "needs credentials"})
	if err != nil {
		t.Fatalf("blocked: %v", err)
	}
	if blocked.Phase != GoalPhaseBlocked || blocked.BlockedReason == nil ||
		blocked.BlockedReason.Code != GoalBlockedCodeModelReported ||
		blocked.BlockedReason.Message != "needs credentials" {
		t.Fatalf("blocked = %+v, want the model-reported reason", blocked)
	}
	if blocked.Activation != GoalActivationDisarmed {
		t.Fatalf("activation = %q, want disarmed", blocked.Activation)
	}
}

func TestAdmitRoundCountsAndBlocksAtCap(t *testing.T) {
	store := newFakeEventStore()
	service := newTestService(store)
	mustCreate(t, service, "sess-1", "objective", 2)

	first, err := service.AdmitRound(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("AdmitRound 1: %v", err)
	}
	if first.RoundsStarted != 1 || first.Revision != 2 {
		t.Fatalf("after round 1 = %+v, want rounds 1 revision 2", first)
	}
	second, err := service.AdmitRound(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("AdmitRound 2: %v", err)
	}
	if second.RoundsStarted != 2 || second.Revision != 3 {
		t.Fatalf("after round 2 = %+v, want rounds 2 revision 3", second)
	}
	if _, err := service.AdmitRound(context.Background(), "sess-1"); !errors.Is(err, ErrGoalRoundLimit) {
		t.Fatalf("AdmitRound over the cap = %v, want ErrGoalRoundLimit", err)
	}
	current, _ := service.Goal("sess-1")
	if current.Phase != GoalPhaseBlocked {
		t.Fatalf("phase after cap = %q, want blocked", current.Phase)
	}
	if current.BlockedReason == nil || current.BlockedReason.Code != GoalBlockedCodeRoundLimit {
		t.Fatalf("blocked reason = %+v, want round-limit", current.BlockedReason)
	}
	if !strings.Contains(current.BlockedReason.Message, "2/2") {
		t.Fatalf("blocked message = %q, want the 2/2 detail", current.BlockedReason.Message)
	}
	if current.Activation != GoalActivationDisarmed {
		t.Fatalf("activation = %q, want disarmed", current.Activation)
	}
	// The blocked transition itself is persisted (one event per revision).
	if events := store.goalEvents("sess-1"); len(events) != 4 {
		t.Fatalf("goal events = %d, want 4 (create + 2 rounds + block)", len(events))
	}
	if _, err := service.AdmitRound(context.Background(), "sess-1"); !errors.Is(err, ErrGoalInvalidTransition) {
		t.Fatalf("AdmitRound on a blocked goal = %v, want ErrGoalInvalidTransition", err)
	}
}

func TestDisarmDoesNotChangeThePersistedGoal(t *testing.T) {
	store := newFakeEventStore()
	service := newTestService(store)
	goal := mustCreate(t, service, "sess-1", "objective", 0)

	disarmed, ok := service.Disarm("sess-1")
	if !ok {
		t.Fatal("Disarm reported no goal")
	}
	if disarmed.Activation != GoalActivationDisarmed || disarmed.Revision != goal.Revision {
		t.Fatalf("disarmed = %+v, want disarmed at the same revision", disarmed)
	}
	if events := store.goalEvents("sess-1"); len(events) != 1 {
		t.Fatalf("goal events = %d, want 1 (disarm is not persisted)", len(events))
	}
	if _, ok := service.Disarm("sess-unknown"); ok {
		t.Fatal("Disarm of a session without a goal reported true")
	}
}

func TestConcurrentAdmitRoundCountsEveryRound(t *testing.T) {
	store := newFakeEventStore()
	service := newTestService(store)
	goal := mustCreate(t, service, "sess-1", "objective", 100)

	const rounds = 50
	var waitGroup sync.WaitGroup
	waitGroup.Add(rounds)
	for index := 0; index < rounds; index++ {
		go func() {
			defer waitGroup.Done()
			if _, err := service.AdmitRound(context.Background(), "sess-1"); err != nil {
				t.Errorf("AdmitRound: %v", err)
			}
		}()
	}
	waitGroup.Wait()

	current, _ := service.Goal("sess-1")
	if current.RoundsStarted != rounds {
		t.Fatalf("rounds = %d, want %d", current.RoundsStarted, rounds)
	}
	if current.Revision != goal.Revision+rounds {
		t.Fatalf("revision = %d, want %d", current.Revision, goal.Revision+rounds)
	}
	if revs := revisionSequence(t, store.goalEvents("sess-1")); len(revs) != rounds+1 {
		t.Fatalf("persisted snapshots = %d, want %d", len(revs), rounds+1)
	}
}

// revisionSequence decodes the revision of each persisted snapshot.
func revisionSequence(t *testing.T, events []rig.SessionEvent) []int {
	t.Helper()
	revisions := make([]int, 0, len(events))
	for _, event := range events {
		var payload goalSnapshot
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("decode snapshot: %v", err)
		}
		revisions = append(revisions, payload.Revision)
	}
	return revisions
}

func TestResumeRearmsAnActiveDisarmedGoal(t *testing.T) {
	store := newFakeEventStore()
	service := newTestService(store)
	goal := mustCreate(t, service, "sess-1", "objective", 0)
	if _, ok := service.Disarm("sess-1"); !ok {
		t.Fatal("Disarm failed")
	}
	ref := GoalRef{ID: goal.ID, Revision: goal.Revision}

	// "Continue goal" from the UI: the goal is still active but disarmed
	// (user Stop / restart). Resume re-arms it without a durable change.
	rearmed, err := service.UpdateGoal(context.Background(), "sess-1", ref,
		GoalUpdate{Action: GoalActionResume})
	if err != nil {
		t.Fatalf("resume active disarmed goal: %v", err)
	}
	if rearmed.Activation != GoalActivationArmed || rearmed.Phase != GoalPhaseActive {
		t.Fatalf("rearmed = %+v, want active/armed", rearmed)
	}
	if rearmed.Revision != goal.Revision {
		t.Fatalf("revision = %d, want the unchanged %d (activation is not durable)", rearmed.Revision, goal.Revision)
	}
	if events := store.goalEvents("sess-1"); len(events) != 1 {
		t.Fatalf("goal events = %d, want 1 (re-arm is not persisted)", len(events))
	}
	// Resuming an already armed goal is an invalid transition.
	if _, err := service.UpdateGoal(context.Background(), "sess-1", ref,
		GoalUpdate{Action: GoalActionResume}); !errors.Is(err, ErrGoalInvalidTransition) {
		t.Fatalf("resume armed goal = %v, want ErrGoalInvalidTransition", err)
	}
}

func TestSubscribeReportsChanges(t *testing.T) {
	store := newFakeEventStore()
	service := newTestService(store)
	notifications := make(chan string, 8)
	unsub, err := service.Subscribe(func(sessionID string) { notifications <- sessionID })
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	goal := mustCreate(t, service, "sess-1", "objective", 0)
	if got := <-notifications; got != "sess-1" {
		t.Fatalf("notification = %q, want sess-1", got)
	}
	if _, err := service.AdmitRound(context.Background(), "sess-1"); err != nil {
		t.Fatalf("AdmitRound: %v", err)
	}
	if got := <-notifications; got != "sess-1" {
		t.Fatalf("second notification = %q, want sess-1", got)
	}
	unsub()
	if _, err := service.UpdateGoal(context.Background(), "sess-1", GoalRef{ID: goal.ID, Revision: 2},
		GoalUpdate{Action: GoalActionPause}); err != nil {
		t.Fatalf("pause after unsub: %v", err)
	}
	select {
	case unexpected := <-notifications:
		t.Fatalf("unsubscribed callback fired for %q", unexpected)
	default:
	}
}

func TestReplaySkipsMalformedEventsAndKeepsTheLastValidOne(t *testing.T) {
	store := newFakeEventStore()
	first := newTestService(store)
	goal := mustCreate(t, first, "sess-1", "objective", 3)
	// Append a corrupt goal event after the valid one: replay must skip it and
	// keep the last valid snapshot instead of dropping the goal.
	if err := store.AppendEvent(context.Background(), rig.SessionEvent{
		SessionID: "sess-1", Type: goalEventType, Payload: json.RawMessage("{not json"),
	}); err != nil {
		t.Fatalf("append corrupt event: %v", err)
	}

	restarted := newTestService(store)
	restored, ok := restarted.Goal("sess-1")
	if !ok || restored.ID != goal.ID || restored.Objective != "objective" {
		t.Fatalf("restored = %+v (ok=%v), want the last valid snapshot", restored, ok)
	}
}

func TestPersistFailureLeavesTheCachedGoalUnchanged(t *testing.T) {
	store := newFakeEventStore()
	service := newTestService(store)
	goal := mustCreate(t, service, "sess-1", "objective", 0)

	store.mu.Lock()
	store.appendErr = errors.New("disk full")
	store.mu.Unlock()
	if _, err := service.AdmitRound(context.Background(), "sess-1"); err == nil {
		t.Fatal("AdmitRound with a failing store succeeded")
	}
	current, _ := service.Goal("sess-1")
	if current.Revision != goal.Revision || current.RoundsStarted != 0 {
		t.Fatalf("goal changed despite a persist failure: %+v", current)
	}
}

func TestGoalsAreIsolatedPerSession(t *testing.T) {
	store := newFakeEventStore()
	service := newTestService(store)
	first := mustCreate(t, service, "sess-1", "goal one", 0)
	second := mustCreate(t, service, "sess-2", "goal two", 0)
	if first.ID == second.ID {
		t.Fatal("two sessions share a goal id")
	}
	if _, err := service.AdmitRound(context.Background(), "sess-1"); err != nil {
		t.Fatalf("AdmitRound sess-1: %v", err)
	}
	if _, ok := service.Disarm("sess-2"); !ok {
		t.Fatal("Disarm sess-2 failed")
	}
	one, _ := service.Goal("sess-1")
	two, _ := service.Goal("sess-2")
	if one.RoundsStarted != 1 || one.Activation != GoalActivationArmed {
		t.Fatalf("sess-1 goal = %+v, want one round and armed", one)
	}
	if two.RoundsStarted != 0 || two.Activation != GoalActivationDisarmed || two.Objective != "goal two" {
		t.Fatalf("sess-2 goal = %+v, want untouched and disarmed", two)
	}
	// Events are partitioned by session in the durable log as well.
	if len(store.goalEvents("sess-1")) != 2 || len(store.goalEvents("sess-2")) != 1 {
		t.Fatalf("events = %d/%d, want 2/1", len(store.goalEvents("sess-1")), len(store.goalEvents("sess-2")))
	}
}

func TestReadFailureRefusesMutationsAndRecovers(t *testing.T) {
	store := newFakeEventStore()
	service := newTestService(store)
	mustCreate(t, service, "sess-1", "objective", 0)

	// A fresh process whose durable read fails must not silently overwrite the
	// persisted goal: mutations fail closed and reads report no goal.
	store.mu.Lock()
	store.readErr = errors.New("db busy")
	store.mu.Unlock()
	restarted := newTestService(store)
	if _, ok := restarted.Goal("sess-1"); ok {
		t.Fatal("Goal with a failing read reported a goal")
	}
	if _, err := restarted.CreateGoal(context.Background(), "sess-1", "overwrite", 0); err == nil {
		t.Fatal("CreateGoal with a failing read succeeded")
	}
	if _, err := restarted.AdmitRound(context.Background(), "sess-1"); err == nil {
		t.Fatal("AdmitRound with a failing read succeeded")
	}
	// Once the read works again the persisted goal is replayed intact.
	store.mu.Lock()
	store.readErr = nil
	store.mu.Unlock()
	recovered, ok := restarted.Goal("sess-1")
	if !ok || recovered.Objective != "objective" {
		t.Fatalf("recovered goal = %+v (ok=%v), want the persisted goal", recovered, ok)
	}
}

func TestUnknownAndEmptySessionsHaveNoGoal(t *testing.T) {
	service := newTestService(nil)
	if _, ok := service.Goal(""); ok {
		t.Fatal("Goal of an empty session reported a goal")
	}
	if _, ok := service.Goal("sess-unknown"); ok {
		t.Fatal("Goal of an unknown session reported a goal")
	}
	if _, err := service.UpdateGoal(context.Background(), "sess-unknown", GoalRef{ID: "goal-1", Revision: 1},
		GoalUpdate{Action: GoalActionPause}); !errors.Is(err, ErrGoalNotFound) {
		t.Fatalf("UpdateGoal unknown session = %v, want ErrGoalNotFound", err)
	}
	if _, err := service.AdmitRound(context.Background(), "sess-unknown"); !errors.Is(err, ErrGoalNotFound) {
		t.Fatalf("AdmitRound unknown session = %v, want ErrGoalNotFound", err)
	}
}

func TestResumeRefusesExhaustedBudget(t *testing.T) {
	store := newFakeEventStore()
	service := newTestService(store)
	mustCreate(t, service, "sess-1", "objective", 1)
	if _, err := service.AdmitRound(context.Background(), "sess-1"); err != nil {
		t.Fatalf("AdmitRound: %v", err)
	}
	active, _ := service.Goal("sess-1")
	// Force the exhausted state: pause and then try to resume at the cap.
	ref := GoalRef{ID: active.ID, Revision: active.Revision}
	paused, err := service.UpdateGoal(context.Background(), "sess-1", ref,
		GoalUpdate{Action: GoalActionPause})
	if err != nil {
		t.Fatalf("pause: %v", err)
	}
	if _, err := service.UpdateGoal(context.Background(), "sess-1",
		GoalRef{ID: paused.ID, Revision: paused.Revision},
		GoalUpdate{Action: GoalActionResume}); !errors.Is(err, ErrGoalInvalidTransition) {
		t.Fatalf("resume at the cap = %v, want ErrGoalInvalidTransition", err)
	}
	// Raising the cap through edit makes the resume possible again.
	raised, err := service.UpdateGoal(context.Background(), "sess-1",
		GoalRef{ID: paused.ID, Revision: paused.Revision},
		GoalUpdate{Action: GoalActionEdit, MaxGoalRounds: 5})
	if err != nil {
		t.Fatalf("edit cap: %v", err)
	}
	if _, err := service.UpdateGoal(context.Background(), "sess-1",
		GoalRef{ID: raised.ID, Revision: raised.Revision},
		GoalUpdate{Action: GoalActionResume}); err != nil {
		t.Fatalf("resume after raising the cap: %v", err)
	}
}

func TestAdmitRoundRefusesDisarmedGoal(t *testing.T) {
	store := newFakeEventStore()
	service := newTestService(store)
	goal := mustCreate(t, service, "sess-1", "objective", 5)
	if _, ok := service.Disarm("sess-1"); !ok {
		t.Fatal("Disarm failed")
	}
	// The driver decided to continue from an armed read, then the user stopped:
	// admitting must fail without consuming a round or touching the revision.
	if _, err := service.AdmitRound(context.Background(), "sess-1"); !errors.Is(err, ErrGoalInvalidTransition) {
		t.Fatalf("AdmitRound after Disarm = %v, want ErrGoalInvalidTransition", err)
	}
	current, _ := service.Goal("sess-1")
	if current.RoundsStarted != 0 || current.Revision != goal.Revision {
		t.Fatalf("goal after refused admit = %+v, want no round and no revision", current)
	}
	if events := store.goalEvents("sess-1"); len(events) != 1 {
		t.Fatalf("goal events = %d, want 1 (a refused admit writes nothing)", len(events))
	}
	// A human resume re-arms the goal; the next admit works again.
	rearmed, err := service.UpdateGoal(context.Background(), "sess-1",
		GoalRef{ID: current.ID, Revision: current.Revision},
		GoalUpdate{Action: GoalActionResume})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	admitted, err := service.AdmitRound(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("AdmitRound after resume: %v", err)
	}
	if admitted.RoundsStarted != 1 || admitted.Revision != rearmed.Revision+1 {
		t.Fatalf("admitted = %+v, want round 1 at the next revision", admitted)
	}
}

func TestAdmitRoundDisarmRaceKeepsRoundsConsistent(t *testing.T) {
	store := newFakeEventStore()
	service := newTestService(store)
	mustCreate(t, service, "sess-1", "objective", 100)

	const attempts = 50
	var accepted atomic.Int64
	var waitGroup sync.WaitGroup
	waitGroup.Add(attempts + 1)
	for index := 0; index < attempts; index++ {
		go func() {
			defer waitGroup.Done()
			if _, err := service.AdmitRound(context.Background(), "sess-1"); err == nil {
				accepted.Add(1)
			}
		}()
	}
	go func() {
		defer waitGroup.Done()
		_, _ = service.Disarm("sess-1")
	}()
	waitGroup.Wait()

	// Every accepted round is durable and counted exactly once, and no round is
	// admitted after the state is disarmed.
	_, _ = service.Disarm("sess-1")
	current, _ := service.Goal("sess-1")
	if got := int64(current.RoundsStarted); got != accepted.Load() {
		t.Fatalf("rounds = %d, want the %d accepted admits", got, accepted.Load())
	}
	if current.Revision != 1+int(accepted.Load()) {
		t.Fatalf("revision = %d, want %d", current.Revision, 1+int(accepted.Load()))
	}
	if !errors.Is(func() error {
		_, err := service.AdmitRound(context.Background(), "sess-1")
		return err
	}(), ErrGoalInvalidTransition) {
		t.Fatal("AdmitRound on a disarmed goal succeeded")
	}
}

func TestDeletedSessionCannotBeRevivedByGoalEvents(t *testing.T) {
	store := newFakeEventStore()
	lookup := newFakeSessionLookup("sess-1")
	service := newTestServiceWithSessions(store, lookup)
	goal := mustCreate(t, service, "sess-1", "objective", 5)
	before := store.appendCount()

	// Delete the durable session row: no goal event may bring it back.
	lookup.setExists("sess-1", false)
	if _, err := service.UpdateGoal(context.Background(), "sess-1",
		GoalRef{ID: goal.ID, Revision: goal.Revision},
		GoalUpdate{Action: GoalActionPause}); !errors.Is(err, ErrGoalNotFound) {
		t.Fatalf("UpdateGoal on a deleted session = %v, want ErrGoalNotFound", err)
	}
	if _, err := service.AdmitRound(context.Background(), "sess-1"); !errors.Is(err, ErrGoalNotFound) {
		t.Fatalf("AdmitRound on a deleted session = %v, want ErrGoalNotFound", err)
	}
	if cleared, err := service.ClearGoal(context.Background(), "sess-1",
		GoalRef{ID: goal.ID, Revision: goal.Revision}); err == nil || cleared {
		t.Fatalf("ClearGoal on a deleted session = %v/%v, want an ErrGoalNotFound", cleared, err)
	}
	if after := store.appendCount(); after != before {
		t.Fatalf("goal events written for a deleted session: %d -> %d", before, after)
	}
	// The cache is only dropped by Forget (the agent calls it after delete).
	if cached, ok := service.Goal("sess-1"); !ok || cached.ID != goal.ID {
		t.Fatalf("cached goal = %+v (ok=%v), want the untouched cache", cached, ok)
	}

	// A fresh process (no cache) must refuse to create a goal for a session that
	// no longer exists instead of resurrecting its row through AppendEvent.
	fresh := newTestServiceWithSessions(store, lookup)
	if _, err := fresh.CreateGoal(context.Background(), "sess-gone", "objective", 0); !errors.Is(err, ErrGoalNotFound) {
		t.Fatalf("CreateGoal for a deleted session = %v, want ErrGoalNotFound", err)
	}
	if after := store.appendCount(); after != before {
		t.Fatalf("CreateGoal for a deleted session wrote an event (%d -> %d)", before, after)
	}
}

func TestClearGoalWritesARevisionedTombstone(t *testing.T) {
	store := newFakeEventStore()
	lookup := newFakeSessionLookup("sess-1")
	service := newTestServiceWithSessions(store, lookup)
	goal := mustCreate(t, service, "sess-1", "objective", 5)

	// A stale ref is rejected and writes nothing.
	if cleared, err := service.ClearGoal(context.Background(), "sess-1",
		GoalRef{ID: goal.ID, Revision: goal.Revision + 1}); !errors.Is(err, ErrGoalStaleRef) || cleared {
		t.Fatalf("stale clear = %v/%v, want ErrGoalStaleRef", cleared, err)
	}
	if events := store.goalEvents("sess-1"); len(events) != 1 {
		t.Fatalf("events after stale clear = %d, want 1", len(events))
	}

	cleared, err := service.ClearGoal(context.Background(), "sess-1",
		GoalRef{ID: goal.ID, Revision: goal.Revision})
	if err != nil || !cleared {
		t.Fatalf("ClearGoal = %v/%v, want true, nil", cleared, err)
	}
	if _, ok := service.Goal("sess-1"); ok {
		t.Fatal("Goal after ClearGoal reported a goal")
	}
	events := store.goalEvents("sess-1")
	if len(events) != 2 {
		t.Fatalf("events after clear = %d, want 2 (create + tombstone)", len(events))
	}
	var payload goalSnapshot
	if err := json.Unmarshal(events[1].Payload, &payload); err != nil {
		t.Fatalf("decode tombstone: %v", err)
	}
	if !payload.Cleared || payload.ID != goal.ID || payload.Revision != goal.Revision+1 {
		t.Fatalf("tombstone payload = %+v, want cleared revision %d", payload, goal.Revision+1)
	}
	// Clearing again (no goal) is a no-op, not an error.
	if cleared, err := service.ClearGoal(context.Background(), "sess-1",
		GoalRef{ID: goal.ID, Revision: goal.Revision + 1}); err != nil || cleared {
		t.Fatalf("second ClearGoal = %v/%v, want false, nil", cleared, err)
	}

	// A restart replays the tombstone as "no goal"; a later create starts a
	// fresh instance and wins the replay again.
	restarted := newTestServiceWithSessions(store, lookup)
	if _, ok := restarted.Goal("sess-1"); ok {
		t.Fatal("replayed tombstone still reports a goal")
	}
	fresh := mustCreate(t, restarted, "sess-1", "second objective", 0)
	if fresh.ID == goal.ID || fresh.Revision != 1 || fresh.Activation != GoalActivationArmed {
		t.Fatalf("recreated goal = %+v, want a new armed instance at revision 1", fresh)
	}
	replayed := newTestServiceWithSessions(store, lookup)
	after, ok := replayed.Goal("sess-1")
	if !ok || after.ID != fresh.ID || after.Objective != "second objective" {
		t.Fatalf("replayed goal after recreate = %+v (ok=%v), want the new instance", after, ok)
	}
}

func TestForgetClearsCacheAndBlocksReplay(t *testing.T) {
	store := newFakeEventStore()
	service := newTestService(store)
	mustCreate(t, service, "sess-1", "objective", 0)
	service.Forget("sess-1")
	if _, ok := service.Goal("sess-1"); ok {
		t.Fatal("Goal after Forget reported a goal")
	}
	// Forget is a process tombstone: even a leftover durable event cannot bring
	// the deleted conversation's goal back.
	if _, err := service.CreateGoal(context.Background(), "sess-1", "zombie", 0); !errors.Is(err, ErrGoalNotFound) {
		t.Fatalf("CreateGoal after Forget = %v, want ErrGoalNotFound", err)
	}
	// A restart replays the durable log: with the session (and its events) gone
	// there is no goal either.
	store.deleteEvents("sess-1")
	restarted := newTestService(store)
	if _, ok := restarted.Goal("sess-1"); ok {
		t.Fatal("restarted service replayed a goal for the deleted session")
	}
}

func TestSessionLookupErrorFailsClosed(t *testing.T) {
	store := newFakeEventStore()
	lookup := newFakeSessionLookup("sess-1")
	lookup.mu.Lock()
	lookup.err = errors.New("db busy")
	lookup.mu.Unlock()
	service := newTestServiceWithSessions(store, lookup)

	if _, err := service.CreateGoal(context.Background(), "sess-1", "objective", 0); err == nil {
		t.Fatal("CreateGoal with a failing session lookup succeeded")
	}
	if got := store.appendCount(); got != 0 {
		t.Fatalf("events written without verifying the session: %d", got)
	}
	// Once the lookup works again the goal is created normally.
	lookup.mu.Lock()
	lookup.err = nil
	lookup.mu.Unlock()
	goal := mustCreate(t, service, "sess-1", "objective", 0)
	if goal.Revision != 1 || goal.Activation != GoalActivationArmed {
		t.Fatalf("goal after recovery = %+v, want a fresh armed goal", goal)
	}
}

func TestCopyGoalCreatesADisarmedCopy(t *testing.T) {
	store := newFakeEventStore()
	lookup := newFakeSessionLookup("sess-src", "sess-dst")
	service := newTestServiceWithSessions(store, lookup)
	source := mustCreate(t, service, "sess-src", "ship it", 5)
	for round := 0; round < 2; round++ {
		if _, err := service.AdmitRound(context.Background(), "sess-src"); err != nil {
			t.Fatalf("AdmitRound: %v", err)
		}
	}
	sourceEvents := len(store.goalEvents("sess-src"))

	copied, ok, err := service.CopyGoal(context.Background(), "sess-src", "sess-dst")
	if err != nil || !ok {
		t.Fatalf("CopyGoal = %v/%v, want a copy", ok, err)
	}
	if copied.ID == source.ID || copied.ID == "" {
		t.Fatalf("copied goal id = %q, want a fresh id (source %q)", copied.ID, source.ID)
	}
	if copied.SessionID != "sess-dst" || copied.Revision != 1 {
		t.Fatalf("copied goal = %+v, want revision 1 on the target session", copied)
	}
	if copied.Objective != "ship it" || copied.RoundsStarted != 2 || copied.MaxGoalRounds != 5 ||
		copied.Phase != GoalPhaseActive {
		t.Fatalf("copied goal = %+v, want the source state", copied)
	}
	if copied.Activation != GoalActivationDisarmed {
		t.Fatalf("copied activation = %q, want disarmed (fork needs a human resume)", copied.Activation)
	}

	// The source goal is untouched (id/revision/rounds/activation).
	unchanged, ok := service.Goal("sess-src")
	if !ok || unchanged.ID != source.ID || unchanged.RoundsStarted != 2 ||
		unchanged.Revision != source.Revision+2 || unchanged.Activation != GoalActivationArmed {
		t.Fatalf("source goal after copy = %+v (ok=%v), want it untouched and armed", unchanged, ok)
	}
	if got := len(store.goalEvents("sess-src")); got != sourceEvents {
		t.Fatalf("source goal events = %d, want %d (the copy must not touch the source)", got, sourceEvents)
	}

	// A restart replays the copy as a disarmed goal.
	restarted := newTestServiceWithSessions(store, lookup)
	replayed, ok := restarted.Goal("sess-dst")
	if !ok || replayed.ID != copied.ID || replayed.RoundsStarted != 2 ||
		replayed.Activation != GoalActivationDisarmed {
		t.Fatalf("replayed copy = %+v (ok=%v), want the disarmed copy", replayed, ok)
	}
}

func TestCopyGoalPreservesPhaseAndBlockedReason(t *testing.T) {
	store := newFakeEventStore()
	lookup := newFakeSessionLookup("src", "dst", "dst-complete")
	service := newTestServiceWithSessions(store, lookup)
	mustCreate(t, service, "src", "blocked goal", 3)
	current, _ := service.Goal("src")
	blocked, err := service.UpdateGoal(context.Background(), "src",
		GoalRef{ID: current.ID, Revision: current.Revision},
		GoalUpdate{Action: GoalActionBlocked, BlockedReason: "needs credentials"})
	if err != nil {
		t.Fatalf("blocked: %v", err)
	}
	copied, ok, err := service.CopyGoal(context.Background(), "src", "dst")
	if err != nil || !ok {
		t.Fatalf("CopyGoal(blocked) = %v/%v", ok, err)
	}
	if copied.Phase != GoalPhaseBlocked || copied.BlockedReason == nil ||
		copied.BlockedReason.Message != "needs credentials" ||
		copied.BlockedReason.Code != GoalBlockedCodeModelReported {
		t.Fatalf("copied blocked goal = %+v, want the phase and reason preserved", copied)
	}
	if copied.Activation != GoalActivationDisarmed {
		t.Fatalf("copied activation = %q, want disarmed", copied.Activation)
	}

	// A complete goal is copied as complete too (DSH: phase is copied verbatim).
	completed, err := service.UpdateGoal(context.Background(), "src",
		GoalRef{ID: blocked.ID, Revision: blocked.Revision},
		GoalUpdate{Action: GoalActionResume})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	completed, err = service.UpdateGoal(context.Background(), "src",
		GoalRef{ID: completed.ID, Revision: completed.Revision},
		GoalUpdate{Action: GoalActionComplete})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	completedCopy, ok, err := service.CopyGoal(context.Background(), "src", "dst-complete")
	if err != nil || !ok {
		t.Fatalf("CopyGoal(complete) = %v/%v", ok, err)
	}
	if completedCopy.Phase != GoalPhaseComplete || completedCopy.Activation != GoalActivationDisarmed {
		t.Fatalf("copied complete goal = %+v, want complete/disarmed", completedCopy)
	}
}

func TestCopyGoalRejectsMissingSourceTargetAndConflicts(t *testing.T) {
	store := newFakeEventStore()
	lookup := newFakeSessionLookup("src", "dst", "gone")
	service := newTestServiceWithSessions(store, lookup)

	// Source without a goal: no copy, no side effect.
	if _, ok, err := service.CopyGoal(context.Background(), "src", "dst"); err != nil || ok {
		t.Fatalf("CopyGoal(no goal) = %v/%v, want false, nil", ok, err)
	}
	if events := store.goalEvents("dst"); len(events) != 0 {
		t.Fatalf("target goal events = %d, want 0 for a goal-less source", len(events))
	}

	mustCreate(t, service, "src", "objective", 0)
	// Target already has a live goal: conflict, nothing written.
	mustCreate(t, service, "dst", "target goal", 0)
	targetEvents := len(store.goalEvents("dst"))
	if _, ok, err := service.CopyGoal(context.Background(), "src", "dst"); !errors.Is(err, ErrGoalConflict) || ok {
		t.Fatalf("CopyGoal over a live target goal = %v/%v, want ErrGoalConflict", ok, err)
	}
	if got := len(store.goalEvents("dst")); got != targetEvents {
		t.Fatalf("target goal events after conflict = %d, want %d", got, targetEvents)
	}

	// A deleted target session is refused (no event that could resurrect it).
	lookup.setExists("gone", false)
	if _, ok, err := service.CopyGoal(context.Background(), "src", "gone"); !errors.Is(err, ErrGoalNotFound) || ok {
		t.Fatalf("CopyGoal to a deleted session = %v/%v, want ErrGoalNotFound", ok, err)
	}
	if events := store.goalEvents("gone"); len(events) != 0 {
		t.Fatalf("deleted target received %d goal events, want 0", len(events))
	}
}
