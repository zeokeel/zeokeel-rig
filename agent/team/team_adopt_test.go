package team_test

// Tests for Coordinator.Adopt: taking over a host child that was started outside
// the coordinator (for example a model-facing team tool spawn), so the
// coordinator owns its run lifecycle, activity fold and persistence.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig/agent/team"
)

// addExistingChild registers a host child the coordinator did not spawn.
// running=false finishes the child immediately (a host member available but
// idle).
func (h *fakeHost) addExistingChild(id string, running bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	child := newFakeChild()
	h.children[id] = child
	if !running {
		child.finish(team.Result{Output: "already done"}, nil)
	}
}

// recordingStore captures every published state.
type recordingStore struct {
	mu     sync.Mutex
	states []team.TeamState
}

func (s *recordingStore) Load(context.Context) (team.TeamState, error) {
	return team.TeamState{}, nil
}

func (s *recordingStore) Save(_ context.Context, state team.TeamState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states = append(s.states, state)
	return nil
}

func (s *recordingStore) last(t *testing.T) team.TeamState {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.states) == 0 {
		t.Fatal("no state was persisted")
		return team.TeamState{}
	}
	return s.states[len(s.states)-1]
}

// TestAdoptTakesOverRunningHostChild is the core case: the member already runs,
// Adopt registers it as running, the watcher records its completion and token
// usage, and the activity row is attributed to it.
func TestAdoptTakesOverRunningHostChild(t *testing.T) {
	h := newFakeHost()
	h.addExistingChild("child-9", true)
	store := &recordingStore{}
	notified := 0
	c := team.New(h, team.Options{Store: store, Notify: func() { notified++ }})

	adopted, err := c.Adopt(testContext(t), "child-9", team.SpawnSpec{
		Name: "alice", Prompt: "audit the diff", Model: "model-2", ParentID: "parent-1",
	})
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if adopted.ID != "child-9" || adopted.Name != "alice" || adopted.Model != "model-2" ||
		adopted.ParentID != "parent-1" {
		t.Fatalf("adopted = %+v", adopted)
	}
	if adopted.Status != team.StatusRunning || adopted.Created.IsZero() || adopted.Started.IsZero() {
		t.Fatalf("adopted status/clocks = %+v", adopted)
	}
	if adopted.Prompt != "audit the diff" {
		t.Fatalf("prompt = %q", adopted.Prompt)
	}
	// The member is addressable by id and name.
	if current, ok := c.Teammate("alice"); !ok || current.ID != "child-9" {
		t.Fatalf("Teammate(alice) = %+v/%v", current, ok)
	}

	// The activity fold attributes the adoption to the member.
	found := false
	for _, row := range c.Activities() {
		if row.MemberID == "child-9" && row.Kind == team.ActivitySpawn && strings.Contains(row.Text, "adopted") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no adopted activity row: %+v", c.Activities())
	}
	if notified == 0 {
		t.Fatal("Adopt did not notify")
	}

	// The watcher records the run outcome it did not start.
	h.finish("child-9", team.Result{Output: "done", UsageTokens: 7, Iterations: 2}, nil)
	waitFor(t, "adopted run completion", func() bool {
		current, _ := c.Teammate("child-9")
		return current.Status == team.StatusDone
	})
	current, _ := c.Teammate("child-9")
	if current.LastOutput != "done" || current.UsageTokens != 7 || current.Iterations != 2 {
		t.Fatalf("recorded run = %+v", current)
	}
	result, err := c.Wait(testContext(t), "child-9")
	if err != nil || result.Output != "done" {
		t.Fatalf("Wait = %+v/%v", result, err)
	}

	// The adoption is durable.
	state := store.last(t)
	if len(state.Members) != 1 || state.Members[0].ID != "child-9" || state.Members[0].Name != "alice" {
		t.Fatalf("persisted state = %+v", state)
	}
	if state.Revision == 0 {
		t.Fatal("persisted state has no revision")
	}
}

// TestAdoptIdleHostChild: a host member that is not running is registered idle
// (no watcher), and Wait reports ErrMemberInactive.
func TestAdoptIdleHostChild(t *testing.T) {
	h := newFakeHost()
	h.addExistingChild("child-idle", false)
	c := team.New(h, team.Options{})
	adopted, err := c.Adopt(testContext(t), "child-idle", team.SpawnSpec{Name: "idle", Prompt: "later"})
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if adopted.Status != team.StatusIdle {
		t.Fatalf("status = %q, want idle", adopted.Status)
	}
	if _, err := c.Wait(testContext(t), "child-idle"); !errors.Is(err, team.ErrMemberInactive) {
		t.Fatalf("Wait(idle) = %v, want ErrMemberInactive", err)
	}
}

// TestAdoptRejectsUnknownAndInvalid covers the error contract.
func TestAdoptRejectsUnknownAndInvalid(t *testing.T) {
	h := newFakeHost()
	c := team.New(h, team.Options{})
	if _, err := c.Adopt(testContext(t), "missing", team.SpawnSpec{Name: "x", Prompt: "y"}); !errors.Is(err, team.ErrUnknownMember) {
		t.Fatalf("Adopt(unknown) = %v, want ErrUnknownMember", err)
	}
	if _, err := c.Adopt(testContext(t), "  ", team.SpawnSpec{Name: "x", Prompt: "y"}); !errors.Is(err, team.ErrUnknownMember) {
		t.Fatalf("Adopt(empty id) = %v, want ErrUnknownMember", err)
	}
	h.addExistingChild("child-1", true)
	if _, err := c.Adopt(testContext(t), "child-1", team.SpawnSpec{Name: " ", Prompt: "y"}); !errors.Is(err, team.ErrEmptyName) {
		t.Fatalf("Adopt(empty name) = %v, want ErrEmptyName", err)
	}
	if _, err := c.Adopt(testContext(t), "child-1", team.SpawnSpec{Name: "x"}); !errors.Is(err, team.ErrEmptyPrompt) {
		t.Fatalf("Adopt(empty prompt) = %v, want ErrEmptyPrompt", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Adopt(cancelled, "child-1", team.SpawnSpec{Name: "x", Prompt: "y"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Adopt(cancelled) = %v, want context.Canceled", err)
	}
	if _, err := (*team.Coordinator)(nil).Adopt(testContext(t), "child-1", team.SpawnSpec{Name: "x", Prompt: "y"}); !errors.Is(err, team.ErrNoHost) {
		t.Fatalf("Adopt(nil) = %v, want ErrNoHost", err)
	}
	if len(c.Snapshot().Members) != 0 {
		t.Fatalf("rejected Adopt created members: %+v", c.Snapshot().Members)
	}
}

// TestAdoptAfterSpawnConflicts: a member the coordinator already owns cannot be
// adopted twice; a closed member reports ErrMemberClosed.
func TestAdoptAfterSpawnConflicts(t *testing.T) {
	h := newFakeHost()
	c := team.New(h, team.Options{})
	spawned := spawnTeammate(t, c, h, "owned")
	if _, err := c.Adopt(testContext(t), spawned.ID, team.SpawnSpec{Name: "again", Prompt: "again"}); !errors.Is(err, team.ErrDuplicateMember) {
		t.Fatalf("Adopt(spawned) = %v, want ErrDuplicateMember", err)
	}
	if members := c.Snapshot().Members; len(members) != 1 || members[0].ID != spawned.ID {
		t.Fatalf("members after conflict = %+v", members)
	}

	h.addExistingChild("child-2", true)
	adopted := adoptMember(t, c, "child-2", "twice")
	if _, err := c.Adopt(testContext(t), adopted.ID, team.SpawnSpec{Name: "third", Prompt: "third"}); !errors.Is(err, team.ErrDuplicateMember) {
		t.Fatalf("second Adopt = %v, want ErrDuplicateMember", err)
	}

	if err := c.Close(testContext(t), spawned.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := c.Adopt(testContext(t), spawned.ID, team.SpawnSpec{Name: "closed", Prompt: "closed"}); !errors.Is(err, team.ErrMemberClosed) {
		t.Fatalf("Adopt(closed) = %v, want ErrMemberClosed", err)
	}
}

// TestAdoptGenerationGuard: Cancel after Adopt invalidates the watcher, so the
// late host completion must not overwrite the interrupted status.
func TestAdoptGenerationGuard(t *testing.T) {
	h := newFakeHost()
	h.addExistingChild("child-3", true)
	c := team.New(h, team.Options{})
	adoptMember(t, c, "child-3", "guarded")
	if err := c.Cancel(testContext(t), "child-3"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	h.finish("child-3", team.Result{Output: "late", UsageTokens: 99}, nil)
	time.Sleep(50 * time.Millisecond)
	current, _ := c.Teammate("child-3")
	if current.Status != team.StatusInterrupted {
		t.Fatalf("status = %q, want interrupted (stale watcher won)", current.Status)
	}
	if current.UsageTokens != 0 || current.LastOutput != "" {
		t.Fatalf("stale result recorded: %+v", current)
	}
}

// adoptMember adopts one existing host child.
func adoptMember(t *testing.T, c *team.Coordinator, id, name string) team.Teammate {
	t.Helper()
	adopted, err := c.Adopt(testContext(t), id, team.SpawnSpec{Name: name, Prompt: "prompt for " + name})
	if err != nil {
		t.Fatalf("Adopt(%q): %v", id, err)
	}
	return adopted
}
