package team_test

// Tests for follow-up tracking, fork identity and adopting an already-finished
// host member.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/agent/team"
)

// followHost is an explicit Host whose Wait consumes a queue of runs. It lets a
// test start a run at the exact moment Track begins watching.
type followHost struct {
	mu        sync.Mutex
	specs     []team.SpawnSpec
	runs      chan runOutcome
	snapshot  team.MemberSnapshot
	exists    bool
	cancelled []string
}

type runOutcome struct {
	result team.Result
	err    error
}

func newFollowHost() *followHost {
	return &followHost{runs: make(chan runOutcome), exists: true}
}

func (h *followHost) Spawn(_ context.Context, spec team.SpawnSpec) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.specs = append(h.specs, spec)
	return "child-1", nil
}

func (h *followHost) Send(ctx context.Context, _ string, _ string) (team.Result, error) {
	return h.Wait(ctx, "")
}

func (h *followHost) Wait(ctx context.Context, _ string) (team.Result, error) {
	select {
	case outcome := <-h.runs:
		return outcome.result, outcome.err
	case <-ctx.Done():
		return team.Result{}, ctx.Err()
	}
}

func (h *followHost) Cancel(_ context.Context, id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cancelled = append(h.cancelled, id)
	return nil
}

func (h *followHost) Snapshot(_ context.Context, _ string) (team.MemberSnapshot, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.snapshot, h.exists
}

func (h *followHost) finish(outcome runOutcome) { h.runs <- outcome }

func (h *followHost) setSnapshot(snapshot team.MemberSnapshot) {
	h.mu.Lock()
	h.snapshot = snapshot
	h.mu.Unlock()
}

func (h *followHost) sentSpecs() []team.SpawnSpec {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]team.SpawnSpec(nil), h.specs...)
}

// stateStore is a simple in-memory Store for restore tests.
type stateStore struct {
	mu    sync.Mutex
	state team.TeamState
}

func (s *stateStore) Load(context.Context) (team.TeamState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state, nil
}

func (s *stateStore) Save(_ context.Context, state team.TeamState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = state
	return nil
}

// TestAdoptFinishedSnapshotRecordsResult: a host member that already finished is
// adopted as done with its answer/usage instead of idle.
func TestAdoptFinishedSnapshotRecordsResult(t *testing.T) {
	h := newFollowHost()
	h.setSnapshot(team.MemberSnapshot{
		Status: "active", Running: false, Finished: true,
		Output: "the answer", UsageTokens: 11, Iterations: 3,
	})
	c := team.New(h, team.Options{})
	adopted, err := c.Adopt(testContext(t), "child-1", team.SpawnSpec{Name: "done", Prompt: "p"})
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if adopted.Status != team.StatusDone {
		t.Fatalf("status = %q, want done", adopted.Status)
	}
	if adopted.LastOutput != "the answer" || adopted.UsageTokens != 11 || adopted.Iterations != 3 {
		t.Fatalf("adopted result = %+v", adopted)
	}
	if adopted.Finished.IsZero() {
		t.Fatal("finished clock not set")
	}
	foundFinish := false
	for _, row := range c.Activities() {
		if row.MemberID == "child-1" && row.Kind == team.ActivityFinish {
			foundFinish = true
		}
	}
	if !foundFinish {
		t.Fatalf("no finish activity: %+v", c.Activities())
	}
	if _, err := c.Wait(testContext(t), "child-1"); err != nil {
		t.Fatalf("Wait(finished) = %v", err)
	}
}

// TestAdoptFailedSnapshotRecordsError: a finished-with-error snapshot becomes
// StatusFailed with the bounded error.
func TestAdoptFailedSnapshotRecordsError(t *testing.T) {
	h := newFollowHost()
	h.setSnapshot(team.MemberSnapshot{
		Status: "active", Finished: true, Error: "provider exploded",
	})
	c := team.New(h, team.Options{})
	adopted, err := c.Adopt(testContext(t), "child-1", team.SpawnSpec{Name: "bad", Prompt: "p"})
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if adopted.Status != team.StatusFailed || adopted.Error != "provider exploded" {
		t.Fatalf("adopted = %+v", adopted)
	}
	if _, err := c.Wait(testContext(t), "child-1"); err == nil {
		t.Fatal("Wait on a failed member returned nil error")
	}
}

// TestTrackWatchesNextRun: Track starts watching a run the host starts later
// (a follow-up send) without sending anything itself.
func TestTrackWatchesNextRun(t *testing.T) {
	h := newFollowHost()
	store := &stateStore{}
	c := team.New(h, team.Options{Store: store})
	first := spawnFollow(t, c, h, "worker") // watcher waits for run 1
	h.finish(runOutcome{result: team.Result{Output: "first", UsageTokens: 2, Iterations: 1}})
	waitFor(t, "initial run", func() bool {
		current, _ := c.Teammate(first.ID)
		return current.Status == team.StatusDone
	})

	tracked, err := c.Track(testContext(t), first.ID)
	if err != nil {
		t.Fatalf("Track: %v", err)
	}
	if tracked.Status != team.StatusRunning || tracked.LastOutput != "" {
		t.Fatalf("tracked = %+v, want running without a stale answer", tracked)
	}
	// Track is a watch, not a send: the host must not have been asked to send.
	if specs := h.sentSpecs(); len(specs) != 1 {
		t.Fatalf("host spawns = %d, want 1 (Track sent nothing)", len(specs))
	}
	activityFound := false
	for _, row := range c.Activities() {
		if row.MemberID == first.ID && row.Kind == team.ActivityWait {
			activityFound = true
		}
	}
	if !activityFound {
		t.Fatalf("no tracking activity: %+v", c.Activities())
	}

	h.finish(runOutcome{result: team.Result{Output: "second", UsageTokens: 5, Iterations: 2}})
	waitFor(t, "tracked follow-up", func() bool {
		current, _ := c.Teammate(first.ID)
		return current.Status == team.StatusDone && current.LastOutput == "second"
	})
	current, _ := c.Teammate(first.ID)
	if current.UsageTokens != 7 || current.Iterations != 3 {
		t.Fatalf("usage did not accumulate over both runs: %+v", current)
	}
}

// TestTrackErrorsAndGenerationGuard covers validation and the stale-watcher
// guard.
func TestTrackErrorsAndGenerationGuard(t *testing.T) {
	h := newFollowHost()
	c := team.New(h, team.Options{})
	if _, err := (*team.Coordinator)(nil).Track(testContext(t), "child-1"); !errors.Is(err, team.ErrNoHost) {
		t.Fatalf("Track(nil) = %v, want ErrNoHost", err)
	}
	if _, err := c.Track(testContext(t), "missing"); !errors.Is(err, team.ErrUnknownMember) {
		t.Fatalf("Track(missing) = %v, want ErrUnknownMember", err)
	}
	if _, err := c.Track(testContext(t), "  "); !errors.Is(err, team.ErrUnknownMember) {
		t.Fatalf("Track(empty) = %v, want ErrUnknownMember", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Track(cancelled, "child-1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Track(cancelled) = %v, want context.Canceled", err)
	}

	spawned := spawnFollow(t, c, h, "worker")
	h.finish(runOutcome{result: team.Result{Output: "one"}})
	waitFor(t, "initial", func() bool {
		current, _ := c.Teammate(spawned.ID)
		return current.Status == team.StatusDone
	})
	if _, err := c.Track(testContext(t), spawned.ID); err != nil {
		t.Fatalf("Track: %v", err)
	}
	if err := c.Cancel(testContext(t), spawned.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	h.finish(runOutcome{result: team.Result{Output: "late", UsageTokens: 50}})
	time.Sleep(50 * time.Millisecond)
	current, _ := c.Teammate(spawned.ID)
	if current.Status != team.StatusInterrupted || current.UsageTokens != 0 {
		t.Fatalf("stale tracked run recorded: %+v", current)
	}
}

// TestForkIdentityInReadModelAndPersistence: SpawnSpec.Fork/ForkedFrom reach the
// host and the read model, survive persistence/restore, and Adopt preserves them.
func TestForkIdentityInReadModelAndPersistence(t *testing.T) {
	h := newFollowHost()
	store := &stateStore{}
	c := team.New(h, team.Options{Store: store})
	forked, err := c.Spawn(testContext(t), team.SpawnSpec{
		Name: "forklet", Prompt: "independent", Fork: true, ForkedFrom: "parent-1",
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if !forked.Fork || forked.ForkedFrom != "parent-1" {
		t.Fatalf("spawned fork identity = %+v", forked)
	}
	specs := h.sentSpecs()
	if len(specs) != 1 || !specs[0].Fork || specs[0].ForkedFrom != "parent-1" {
		t.Fatalf("host spawn spec = %+v, fork identity was not propagated", specs)
	}

	restored := team.New(h, team.Options{Store: store})
	if err := restored.Restore(testContext(t)); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	member, ok := restored.Teammate("forklet")
	if !ok || !member.Fork || member.ForkedFrom != "parent-1" {
		t.Fatalf("restored fork identity = %+v/%v", member, ok)
	}

	h.setSnapshot(team.MemberSnapshot{Status: "active", Finished: true, Output: "fork answer"})
	adopted, err := c.Adopt(testContext(t), "child-2", team.SpawnSpec{
		Name: "adopted-fork", Prompt: "p", Fork: true, ForkedFrom: "parent-2",
	})
	if err != nil {
		t.Fatalf("Adopt(fork): %v", err)
	}
	if !adopted.Fork || adopted.ForkedFrom != "parent-2" {
		t.Fatalf("adopted fork identity = %+v", adopted)
	}
}

// TestSpawnForkDefaults: a normal spawn has no fork identity.
func TestSpawnForkDefaults(t *testing.T) {
	h := newFollowHost()
	c := team.New(h, team.Options{})
	member := spawnFollow(t, c, h, "plain")
	if member.Fork || member.ForkedFrom != "" {
		t.Fatalf("plain member = %+v, want no fork identity", member)
	}
	if got := fmt.Sprintf("%+v", member.Fork); !strings.Contains(got, "false") {
		t.Fatalf("unexpected fork rendering: %s", got)
	}
}

// spawnFollow spawns one member on the followHost test host.
func spawnFollow(t *testing.T, c *team.Coordinator, h *followHost, name string) team.Teammate {
	t.Helper()
	member, err := c.Spawn(testContext(t), team.SpawnSpec{
		Name: name, Prompt: "prompt for " + name, Model: "model-1", ParentID: "parent-1",
	})
	if err != nil {
		t.Fatalf("Spawn(%q): %v", name, err)
	}
	return member
}
