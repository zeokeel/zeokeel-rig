package team_test

// Tests for the host-agnostic team orchestration state machine. They define the
// contract of the package: spawn/register, run status transitions, send/wait/
// wake, the bounded activity fold, persistence (snapshot/restore), stale/missing
// children, close/cancel and concurrency.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/zeokeel/zeokeel-rig/agent/team"
)

// fakeHost is a controllable Host: every spawn gets a pending initial run the
// test finishes explicitly, so the state machine is observed without timing.
type fakeHost struct {
	mu        sync.Mutex
	next      int
	children  map[string]*fakeChild
	spawnErr  error
	emptyID   bool
	fixedID   string
	sendFn    func(ctx context.Context, id, message string) (team.Result, error)
	missing   map[string]bool
	canceled  []string
	waitCalls int
}

func newFakeHost() *fakeHost {
	return &fakeHost{children: make(map[string]*fakeChild), missing: make(map[string]bool)}
}

func (h *fakeHost) Spawn(_ context.Context, _ team.SpawnSpec) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.spawnErr != nil {
		return "", h.spawnErr
	}
	if h.emptyID {
		return "", nil
	}
	h.next++
	id := h.fixedID
	if id == "" {
		id = fmt.Sprintf("child-%d", h.next)
	}
	if _, exists := h.children[id]; exists {
		return id, nil // the coordinator must reject the duplicate
	}
	h.children[id] = newFakeChild()
	return id, nil
}

func (h *fakeHost) Send(ctx context.Context, id, message string) (team.Result, error) {
	h.mu.Lock()
	_, ok := h.children[id]
	sendFn := h.sendFn
	h.mu.Unlock()
	if !ok {
		return team.Result{}, team.ErrUnknownMember
	}
	if sendFn != nil {
		return sendFn(ctx, id, message)
	}
	return team.Result{Output: "reply: " + message, UsageTokens: 3, Iterations: 1}, nil
}

func (h *fakeHost) Wait(ctx context.Context, id string) (team.Result, error) {
	h.mu.Lock()
	h.waitCalls++
	child, ok := h.children[id]
	h.mu.Unlock()
	if !ok {
		return team.Result{}, team.ErrUnknownMember
	}
	return child.wait(ctx)
}

func (h *fakeHost) Cancel(_ context.Context, id string) error {
	h.mu.Lock()
	h.canceled = append(h.canceled, id)
	child := h.children[id]
	h.mu.Unlock()
	if child != nil {
		child.finish(team.Result{}, context.Canceled)
	}
	return nil
}

func (h *fakeHost) Snapshot(_ context.Context, id string) (team.MemberSnapshot, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.missing[id] {
		return team.MemberSnapshot{}, false
	}
	child, ok := h.children[id]
	if !ok {
		return team.MemberSnapshot{}, false
	}
	return team.MemberSnapshot{Status: "active", Running: child.running()}, true
}

// finish completes the pending initial run of one child.
func (h *fakeHost) finish(id string, result team.Result, err error) {
	h.mu.Lock()
	child := h.children[id]
	h.mu.Unlock()
	if child != nil {
		child.finish(result, err)
	}
}

// markMissing makes the backend report one child as gone.
func (h *fakeHost) markMissing(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.missing[id] = true
}

func (h *fakeHost) cancelCalls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.canceled...)
}

func (h *fakeHost) waitCallsCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.waitCalls
}

// fakeChild is one pending/running child run.
type fakeChild struct {
	mu     sync.Mutex
	done   chan struct{}
	closed bool
	result team.Result
	err    error
}

func newFakeChild() *fakeChild { return &fakeChild{done: make(chan struct{})} }

func (c *fakeChild) finish(result team.Result, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.result, c.err = result, err
	c.closed = true
	close(c.done)
}

func (c *fakeChild) wait(ctx context.Context) (team.Result, error) {
	select {
	case <-c.done:
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.result, c.err
	case <-ctx.Done():
		return team.Result{}, ctx.Err()
	}
}

func (c *fakeChild) running() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.closed
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// waitFor polls cond until it holds (the asynchronous run watcher is a
// goroutine, so a successful spawn/finish is observed through this helper).
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func spawnTeammate(t *testing.T, c *team.Coordinator, h *fakeHost, name string) team.Teammate {
	t.Helper()
	teammate, err := c.Spawn(testContext(t), team.SpawnSpec{
		Name: name, Prompt: "prompt for " + name, Model: "model-1", ParentID: "parent-1",
	})
	if err != nil {
		t.Fatalf("Spawn(%q): %v", name, err)
	}
	return teammate
}

func activityKinds(rows []team.Activity) []team.ActivityKind {
	kinds := make([]team.ActivityKind, len(rows))
	for index, row := range rows {
		kinds[index] = row.Kind
	}
	return kinds
}

func TestSpawnRegistersRunningTeammate(t *testing.T) {
	h := newFakeHost()
	notified := 0
	c := team.New(h, team.Options{Notify: func() { notified++ }})

	teammate := spawnTeammate(t, c, h, "alpha")
	if teammate.ID != "child-1" {
		t.Fatalf("id = %q, want child-1", teammate.ID)
	}
	if teammate.Status != team.StatusRunning {
		t.Fatalf("status = %q, want running", teammate.Status)
	}
	if teammate.Name != "alpha" || teammate.Model != "model-1" ||
		teammate.Prompt != "prompt for alpha" || teammate.ParentID != "parent-1" {
		t.Fatalf("spawn metadata = %+v", teammate)
	}
	if teammate.Created.IsZero() || teammate.Started.IsZero() || !teammate.Finished.IsZero() {
		t.Fatalf("spawn timing = %+v", teammate)
	}
	if len(teammate.Activity) != 1 || teammate.Activity[0].Kind != team.ActivitySpawn {
		t.Fatalf("spawn activity = %+v", teammate.Activity)
	}
	if teammate.Activity[0].MemberID != "child-1" || teammate.Activity[0].Error {
		t.Fatalf("spawn activity attribution = %+v", teammate.Activity[0])
	}
	if notified == 0 {
		t.Fatal("Notify was not called for the spawn")
	}

	stored, ok := c.Teammate("child-1")
	if !ok || stored.Status != team.StatusRunning || stored.Name != "alpha" {
		t.Fatalf("Teammate(child-1) = %+v, %v", stored, ok)
	}
	if _, ok := c.Teammate("missing"); ok {
		t.Fatal("Teammate(missing) reported a member")
	}
}

func TestSpawnValidationAndHostError(t *testing.T) {
	h := newFakeHost()
	c := team.New(h, team.Options{})

	if _, err := c.Spawn(testContext(t), team.SpawnSpec{Prompt: "p"}); !errors.Is(err, team.ErrEmptyName) {
		t.Fatalf("empty name error = %v", err)
	}
	if _, err := c.Spawn(testContext(t), team.SpawnSpec{Name: "n"}); !errors.Is(err, team.ErrEmptyPrompt) {
		t.Fatalf("empty prompt error = %v", err)
	}

	boom := errors.New("spawn failed")
	h.spawnErr = boom
	if _, err := c.Spawn(testContext(t), team.SpawnSpec{Name: "n", Prompt: "p"}); !errors.Is(err, boom) {
		t.Fatalf("host spawn error = %v", err)
	}
	if members := c.Snapshot().Members; len(members) != 0 {
		t.Fatalf("failed spawn registered members: %+v", members)
	}
	if activities := c.Activities(); len(activities) != 0 {
		t.Fatalf("failed spawn recorded activity: %+v", activities)
	}
}

func TestSpawnRejectsDuplicateHostID(t *testing.T) {
	h := newFakeHost()
	h.fixedID = "child-x"
	c := team.New(h, team.Options{})

	if _, err := c.Spawn(testContext(t), team.SpawnSpec{Name: "a", Prompt: "p"}); err != nil {
		t.Fatalf("first spawn: %v", err)
	}
	if _, err := c.Spawn(testContext(t), team.SpawnSpec{Name: "b", Prompt: "p"}); !errors.Is(err, team.ErrDuplicateMember) {
		t.Fatalf("duplicate spawn error = %v", err)
	}
	if calls := h.cancelCalls(); len(calls) != 1 || calls[0] != "child-x" {
		t.Fatalf("duplicate spawn did not release the host child: %v", calls)
	}
	if members := c.Snapshot().Members; len(members) != 1 {
		t.Fatalf("duplicate spawn registered %d members", len(members))
	}
}

func TestInitialRunFinishTransitionsToDone(t *testing.T) {
	h := newFakeHost()
	c := team.New(h, team.Options{})
	teammate := spawnTeammate(t, c, h, "alpha")

	changes := c.Changes()
	h.finish(teammate.ID, team.Result{Output: "the answer", UsageTokens: 12, Iterations: 2}, nil)
	select {
	case <-changes:
	case <-time.After(2 * time.Second):
		t.Fatal("no change published for the finished run")
	}

	done, ok := c.Teammate(teammate.ID)
	if !ok {
		t.Fatal("member disappeared")
	}
	if done.Status != team.StatusDone {
		t.Fatalf("status = %q, want done", done.Status)
	}
	if done.LastOutput != "the answer" || done.UsageTokens != 12 || done.Iterations != 2 {
		t.Fatalf("done snapshot = %+v", done)
	}
	if done.Finished.IsZero() || done.Error != "" {
		t.Fatalf("done timing/error = %+v", done)
	}
	kinds := activityKinds(done.Activity)
	if len(kinds) != 2 || kinds[0] != team.ActivitySpawn || kinds[1] != team.ActivityFinish {
		t.Fatalf("activity kinds = %v", kinds)
	}
	if done.Activity[1].Text == "" || done.Activity[1].Error {
		t.Fatalf("finish activity = %+v", done.Activity[1])
	}
}

func TestFailedRunTransitionsToFailed(t *testing.T) {
	h := newFakeHost()
	c := team.New(h, team.Options{MaxErrorRunes: 40})
	teammate := spawnTeammate(t, c, h, "alpha")

	boom := errors.New(strings.Repeat("x", 200))
	h.finish(teammate.ID, team.Result{}, boom)
	waitFor(t, "failed state", func() bool {
		got, _ := c.Teammate(teammate.ID)
		return got.Status == team.StatusFailed
	})

	failed, _ := c.Teammate(teammate.ID)
	if !strings.Contains(failed.Error, "x") {
		t.Fatalf("error = %q", failed.Error)
	}
	if utf8.RuneCountInString(failed.Error) > 41 || !strings.HasSuffix(failed.Error, "…") {
		t.Fatalf("error was not bounded: %q", failed.Error)
	}
	kinds := activityKinds(failed.Activity)
	if len(kinds) != 2 || kinds[1] != team.ActivityError {
		t.Fatalf("activity kinds = %v", kinds)
	}
	if !failed.Activity[1].Error {
		t.Fatalf("error activity = %+v", failed.Activity[1])
	}
}

func TestSendRunsAndAccumulatesUsage(t *testing.T) {
	h := newFakeHost()
	c := team.New(h, team.Options{})
	teammate := spawnTeammate(t, c, h, "alpha")
	h.finish(teammate.ID, team.Result{Output: "first", UsageTokens: 10, Iterations: 1}, nil)
	waitFor(t, "first run", func() bool {
		got, _ := c.Teammate(teammate.ID)
		return got.Status == team.StatusDone
	})

	result, err := c.Send(testContext(t), teammate.ID, "follow up")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if result.Output != "reply: follow up" || result.UsageTokens != 3 || result.Iterations != 1 {
		t.Fatalf("send result = %+v", result)
	}
	after, _ := c.Teammate(teammate.ID)
	if after.Status != team.StatusDone || after.LastOutput != "reply: follow up" {
		t.Fatalf("after send = %+v", after)
	}
	if after.UsageTokens != 13 || after.Iterations != 2 {
		t.Fatalf("usage accumulation = %d/%d, want 13/2", after.UsageTokens, after.Iterations)
	}
	if after.Started.Before(after.Created) {
		t.Fatalf("run start %v before spawn %v", after.Started, after.Created)
	}
	kinds := activityKinds(after.Activity)
	want := []team.ActivityKind{team.ActivitySpawn, team.ActivityFinish, team.ActivitySend, team.ActivityFinish}
	if len(kinds) != len(want) {
		t.Fatalf("activity kinds = %v, want %v", kinds, want)
	}
	for index := range want {
		if kinds[index] != want[index] {
			t.Fatalf("activity kinds = %v, want %v", kinds, want)
		}
	}
}

func TestSendFailureMarksFailed(t *testing.T) {
	h := newFakeHost()
	boom := errors.New("provider exploded")
	h.sendFn = func(context.Context, string, string) (team.Result, error) {
		return team.Result{UsageTokens: 7}, boom
	}
	c := team.New(h, team.Options{})
	teammate := spawnTeammate(t, c, h, "alpha")
	h.finish(teammate.ID, team.Result{Output: "first"}, nil)
	waitFor(t, "first run", func() bool {
		got, _ := c.Teammate(teammate.ID)
		return got.Status == team.StatusDone
	})

	if _, err := c.Send(testContext(t), teammate.ID, "follow up"); !errors.Is(err, boom) {
		t.Fatalf("Send error = %v", err)
	}
	after, _ := c.Teammate(teammate.ID)
	if after.Status != team.StatusFailed || !strings.Contains(after.Error, "provider exploded") {
		t.Fatalf("after failed send = %+v", after)
	}
	if after.UsageTokens != 7 {
		t.Fatalf("failed run usage = %d, want 7 (provider usage is still billed)", after.UsageTokens)
	}
	if last := after.Activity[len(after.Activity)-1]; last.Kind != team.ActivityError || !last.Error {
		t.Fatalf("last activity = %+v", last)
	}
}

func TestSendWhileRunningRejected(t *testing.T) {
	h := newFakeHost()
	c := team.New(h, team.Options{})
	teammate := spawnTeammate(t, c, h, "alpha")

	if _, err := c.Send(testContext(t), teammate.ID, "too soon"); !errors.Is(err, team.ErrMemberRunning) {
		t.Fatalf("Send while running = %v", err)
	}
	h.finish(teammate.ID, team.Result{Output: "ok"}, nil)
	waitFor(t, "done", func() bool {
		got, _ := c.Teammate(teammate.ID)
		return got.Status == team.StatusDone
	})
	if _, err := c.Send(testContext(t), teammate.ID, "now"); err != nil {
		t.Fatalf("Send after done: %v", err)
	}
}

func TestWaitReturnsRunResultAndCachesIt(t *testing.T) {
	h := newFakeHost()
	c := team.New(h, team.Options{})
	teammate := spawnTeammate(t, c, h, "alpha")

	go h.finish(teammate.ID, team.Result{Output: "waited", UsageTokens: 4, Iterations: 2}, nil)
	result, err := c.Wait(testContext(t), teammate.ID)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if result.Output != "waited" || result.UsageTokens != 4 || result.Iterations != 2 {
		t.Fatalf("wait result = %+v", result)
	}
	// A completed run keeps answering with the same result (subagent parity).
	cached, err := c.Wait(testContext(t), teammate.ID)
	if err != nil || cached.Output != "waited" {
		t.Fatalf("second Wait = %+v, %v", cached, err)
	}
	if calls := h.waitCallsCount(); calls < 1 {
		t.Fatalf("host Wait was never called")
	}
}

func TestWaitOnIdleOrInterruptedRejected(t *testing.T) {
	h := newFakeHost()
	c := team.New(h, team.Options{})
	teammate := spawnTeammate(t, c, h, "alpha")

	if err := c.Cancel(testContext(t), teammate.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if _, err := c.Wait(testContext(t), teammate.ID); !errors.Is(err, team.ErrMemberInactive) {
		t.Fatalf("Wait on interrupted = %v", err)
	}
}

func TestCancelInterruptsAndKeepsMember(t *testing.T) {
	h := newFakeHost()
	c := team.New(h, team.Options{})
	teammate := spawnTeammate(t, c, h, "alpha")

	if err := c.Cancel(testContext(t), teammate.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	canceled, ok := c.Teammate(teammate.ID)
	if !ok {
		t.Fatal("canceled member disappeared")
	}
	if canceled.Status != team.StatusInterrupted || canceled.Finished.IsZero() {
		t.Fatalf("canceled state = %+v", canceled)
	}
	if calls := h.cancelCalls(); len(calls) != 1 || calls[0] != teammate.ID {
		t.Fatalf("host cancel calls = %v", calls)
	}
	if last := canceled.Activity[len(canceled.Activity)-1]; last.Kind != team.ActivityInterrupt {
		t.Fatalf("last activity = %+v", last)
	}
	// The interrupted member stays available: a follow-up run succeeds.
	if _, err := c.Send(testContext(t), teammate.ID, "resume"); err != nil {
		t.Fatalf("Send after cancel: %v", err)
	}
	resumed, _ := c.Teammate(teammate.ID)
	if resumed.Status != team.StatusDone {
		t.Fatalf("resumed state = %+v", resumed)
	}
	// A stale watcher record (the canceled initial run) must not overwrite it.
	time.Sleep(20 * time.Millisecond)
	settled, _ := c.Teammate(teammate.ID)
	if settled.Status != team.StatusDone {
		t.Fatalf("stale watcher overwrote the state: %+v", settled)
	}
	// Canceling an idle member is a no-op, not an error.
	if err := c.Cancel(testContext(t), teammate.ID); err != nil {
		t.Fatalf("Cancel on idle: %v", err)
	}
}

func TestCloseStopsAndCloses(t *testing.T) {
	h := newFakeHost()
	c := team.New(h, team.Options{})
	teammate := spawnTeammate(t, c, h, "alpha")

	if err := c.Close(testContext(t), teammate.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	closed, ok := c.Teammate(teammate.ID)
	if !ok || closed.Status != team.StatusClosed {
		t.Fatalf("closed state = %+v, %v", closed, ok)
	}
	kinds := activityKinds(closed.Activity)
	if kinds[len(kinds)-1] != team.ActivityClose {
		t.Fatalf("activity kinds = %v, want a close row", kinds)
	}
	if calls := h.cancelCalls(); len(calls) != 1 || calls[0] != teammate.ID {
		t.Fatalf("host cancel calls = %v", calls)
	}
	if err := c.Close(testContext(t), teammate.ID); !errors.Is(err, team.ErrMemberClosed) {
		t.Fatalf("second Close = %v", err)
	}
	if _, err := c.Send(testContext(t), teammate.ID, "hello"); !errors.Is(err, team.ErrMemberClosed) {
		t.Fatalf("Send to closed = %v", err)
	}
	if _, err := c.Wait(testContext(t), teammate.ID); !errors.Is(err, team.ErrMemberClosed) {
		t.Fatalf("Wait on closed = %v", err)
	}
}

func TestUnknownReferences(t *testing.T) {
	c := team.New(newFakeHost(), team.Options{})
	ctx := testContext(t)
	if _, err := c.Send(ctx, "nope", "hi"); !errors.Is(err, team.ErrUnknownMember) {
		t.Fatalf("Send unknown = %v", err)
	}
	if _, err := c.Wait(ctx, "nope"); !errors.Is(err, team.ErrUnknownMember) {
		t.Fatalf("Wait unknown = %v", err)
	}
	if err := c.Cancel(ctx, "nope"); !errors.Is(err, team.ErrUnknownMember) {
		t.Fatalf("Cancel unknown = %v", err)
	}
	if err := c.Close(ctx, "nope"); !errors.Is(err, team.ErrUnknownMember) {
		t.Fatalf("Close unknown = %v", err)
	}
}

func TestMissingChildMarksInterrupted(t *testing.T) {
	h := newFakeHost()
	c := team.New(h, team.Options{})
	teammate := spawnTeammate(t, c, h, "alpha")
	h.markMissing(teammate.ID)

	if _, err := c.Wait(testContext(t), teammate.ID); !errors.Is(err, team.ErrUnknownMember) {
		t.Fatalf("Wait on a missing child = %v", err)
	}
	missing, _ := c.Teammate(teammate.ID)
	if missing.Status != team.StatusInterrupted || missing.Error == "" {
		t.Fatalf("missing child state = %+v", missing)
	}
	if last := missing.Activity[len(missing.Activity)-1]; last.Kind != team.ActivityInterrupt || !last.Error {
		t.Fatalf("missing activity = %+v", last)
	}
}

func TestReconcileMarksMissingChildrenOnly(t *testing.T) {
	h := newFakeHost()
	c := team.New(h, team.Options{})
	alive := spawnTeammate(t, c, h, "alive")
	gone := spawnTeammate(t, c, h, "gone")
	h.markMissing(gone.ID)

	if err := c.Reconcile(testContext(t)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	aliveNow, _ := c.Teammate(alive.ID)
	goneNow, _ := c.Teammate(gone.ID)
	if aliveNow.Status != team.StatusRunning {
		t.Fatalf("alive member changed: %+v", aliveNow)
	}
	if goneNow.Status != team.StatusInterrupted {
		t.Fatalf("gone member state = %+v", goneNow)
	}
	h.finish(alive.ID, team.Result{Output: "ok"}, nil)
	waitFor(t, "alive finishes", func() bool {
		got, _ := c.Teammate(alive.ID)
		return got.Status == team.StatusDone
	})
}

func TestActivityBoundsAndPerMemberLimit(t *testing.T) {
	h := newFakeHost()
	c := team.New(h, team.Options{MaxActivity: 6, MaxActivityPerMember: 2})
	teammate := spawnTeammate(t, c, h, "alpha")
	h.finish(teammate.ID, team.Result{Output: "first"}, nil)
	waitFor(t, "first run", func() bool {
		got, _ := c.Teammate(teammate.ID)
		return got.Status == team.StatusDone
	})

	for index := 0; index < 5; index++ {
		if _, err := c.Send(testContext(t), teammate.ID, fmt.Sprintf("message %d", index)); err != nil {
			t.Fatalf("Send(%d): %v", index, err)
		}
	}
	activities := c.Activities()
	if len(activities) > 6 {
		t.Fatalf("global activity = %d rows, want <= 6", len(activities))
	}
	if len(activities) == 0 {
		t.Fatal("activity was pruned to nothing")
	}
	perMember := 0
	for _, row := range activities {
		if row.MemberID == teammate.ID {
			perMember++
		}
	}
	if perMember > 2 {
		t.Fatalf("per-member activity = %d rows, want <= 2", perMember)
	}
	state := c.Snapshot()
	if len(state.Activity) != len(activities) {
		t.Fatalf("snapshot activity = %d, Activities() = %d", len(state.Activity), len(activities))
	}
	member, _ := c.Teammate(teammate.ID)
	if len(member.Activity) > 2 {
		t.Fatalf("member activity = %d rows, want <= 2", len(member.Activity))
	}
	// The newest rows survive (the last send/finish pair).
	if last := activities[len(activities)-1]; last.Kind != team.ActivityFinish {
		t.Fatalf("newest activity = %+v, want the last finish", last)
	}
}

func TestActivityTextIsBoundedAndCollapsed(t *testing.T) {
	h := newFakeHost()
	c := team.New(h, team.Options{MaxTextRunes: 60})
	longPrompt := strings.Repeat("# heading\n```text\nfenced code\n```\n", 20) + "tail"
	teammate, err := c.Spawn(testContext(t), team.SpawnSpec{Name: "alpha", Prompt: longPrompt})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	member, _ := c.Teammate(teammate.ID)
	text := member.Activity[0].Text
	if strings.Contains(text, "```") || strings.Contains(text, "#") {
		t.Fatalf("activity text kept markdown structure: %q", text)
	}
	if runes := utf8.RuneCountInString(text); runes > 61 || !strings.HasSuffix(text, "…") {
		t.Fatalf("activity text was not bounded (%d runes): %q", runes, text)
	}
	if strings.Contains(text, "fenced code") {
		t.Fatalf("activity text kept fenced code: %q", text)
	}
}

func TestPersistenceSnapshotAndRestore(t *testing.T) {
	store := team.NewMemoryStore()
	h := newFakeHost()
	c := team.New(h, team.Options{Store: store})
	teammate := spawnTeammate(t, c, h, "alpha")

	saved, err := store.Load(testContext(t))
	if err != nil {
		t.Fatalf("Load after spawn: %v", err)
	}
	if len(saved.Members) != 1 || saved.Members[0].Status != team.StatusRunning || saved.Revision == 0 {
		t.Fatalf("saved spawn state = %+v", saved)
	}
	if len(saved.Members[0].Activity) != 1 {
		t.Fatalf("saved spawn activity = %+v", saved.Members[0].Activity)
	}

	h.finish(teammate.ID, team.Result{Output: "persisted", UsageTokens: 9, Iterations: 3}, nil)
	waitFor(t, "persisted finish", func() bool {
		state, _ := store.Load(testContext(t))
		return len(state.Members) == 1 && state.Members[0].Status == team.StatusDone
	})
	saved, _ = store.Load(testContext(t))
	if saved.Members[0].LastOutput != "persisted" || saved.Members[0].UsageTokens != 9 || saved.Members[0].Iterations != 3 {
		t.Fatalf("saved finish state = %+v", saved.Members[0])
	}
}

func TestRestoreMarksStaleRunsInterrupted(t *testing.T) {
	store := team.NewMemoryStore()
	start := time.Now().Add(-time.Minute)
	seed := team.TeamState{
		Revision: 7,
		Members: []team.Teammate{
			{ID: "m-1", Name: "stale", Status: team.StatusRunning, Started: start},
			{ID: "m-2", Name: "done", Status: team.StatusDone, LastOutput: "old answer", Started: start, Finished: start.Add(time.Second)},
			{ID: "m-3", Name: "closed", Status: team.StatusClosed},
		},
		Activity: []team.Activity{{ID: "a-1", MemberID: "m-2", Kind: team.ActivityFinish, Text: "old", Time: start}},
	}
	if err := store.Save(context.Background(), seed); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	c := team.New(newFakeHost(), team.Options{Store: store})
	if err := c.Restore(testContext(t)); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	state := c.Snapshot()
	if state.Revision <= 7 {
		t.Fatalf("restore did not publish the stale fix: revision %d", state.Revision)
	}
	if len(state.Members) != 3 {
		t.Fatalf("restored members = %+v", state.Members)
	}
	byID := make(map[string]team.Teammate, len(state.Members))
	for _, member := range state.Members {
		byID[member.ID] = member
	}
	if stale := byID["m-1"]; stale.Status != team.StatusInterrupted || stale.Error == "" || !stale.Historical || stale.Finished.IsZero() {
		t.Fatalf("stale member = %+v", stale)
	}
	if done := byID["m-2"]; done.Status != team.StatusDone || done.LastOutput != "old answer" {
		t.Fatalf("done member = %+v", done)
	}
	if closed := byID["m-3"]; closed.Status != team.StatusClosed {
		t.Fatalf("closed member = %+v", closed)
	}
	if len(state.Activity) < 2 {
		t.Fatalf("restored activity = %+v, want the interrupt row appended", state.Activity)
	}
	last := state.Activity[len(state.Activity)-1]
	if last.Kind != team.ActivityInterrupt || last.MemberID != "m-1" || !last.Error {
		t.Fatalf("restored interrupt row = %+v", last)
	}
	// The corrected state is persisted, so the next restore is stable.
	persisted, err := store.Load(testContext(t))
	if err != nil {
		t.Fatalf("Load after restore: %v", err)
	}
	if persisted.Revision != state.Revision {
		t.Fatalf("persisted revision = %d, snapshot = %d", persisted.Revision, state.Revision)
	}
}

func TestRestoredMemberWithoutBackendIsHistorical(t *testing.T) {
	store := team.NewMemoryStore()
	if err := store.Save(context.Background(), team.TeamState{
		Revision: 3,
		Members:  []team.Teammate{{ID: "m-1", Name: "past", Status: team.StatusDone, LastOutput: "answer"}},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	c := team.New(newFakeHost(), team.Options{Store: store})
	if err := c.Restore(testContext(t)); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	member, _ := c.Teammate("m-1")
	if !member.Historical {
		t.Fatalf("restored member = %+v, want Historical", member)
	}
	// Reading the recorded answer still works; a follow-up needs a live backend.
	if result, err := c.Wait(testContext(t), "m-1"); err != nil || result.Output != "answer" {
		t.Fatalf("Wait on historical done = %+v, %v", result, err)
	}
	if _, err := c.Send(testContext(t), "m-1", "hello"); !errors.Is(err, team.ErrHistorical) {
		t.Fatalf("Send to historical = %v", err)
	}
}

func TestRestoreFoldsPerMemberActivity(t *testing.T) {
	store := team.NewMemoryStore()
	start := time.Now().Add(-time.Minute)
	if err := store.Save(context.Background(), team.TeamState{
		Revision: 2,
		Members: []team.Teammate{{
			ID: "m-1", Name: "a", Status: team.StatusDone, LastOutput: "x",
			Activity: []team.Activity{
				{ID: "a-1", MemberID: "m-1", Kind: team.ActivitySpawn, Text: "spawned a", Time: start},
				{ID: "a-2", MemberID: "m-1", Kind: team.ActivityFinish, Text: "a finished", Time: start.Add(time.Second)},
			},
		}},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	c := team.New(newFakeHost(), team.Options{Store: store})
	if err := c.Restore(testContext(t)); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	rows := c.Activities()
	if len(rows) != 2 || rows[0].Kind != team.ActivitySpawn || rows[1].Kind != team.ActivityFinish {
		t.Fatalf("folded member activity = %+v", rows)
	}
	member, _ := c.Teammate("m-1")
	if len(member.Activity) != 2 {
		t.Fatalf("member activity = %+v", member.Activity)
	}
}

func TestRestoreWithoutStoreIsNoop(t *testing.T) {
	c := team.New(newFakeHost(), team.Options{})
	if err := c.Restore(testContext(t)); err != nil {
		t.Fatalf("Restore without store: %v", err)
	}
	if members := c.Snapshot().Members; len(members) != 0 {
		t.Fatalf("restore without store produced members: %+v", members)
	}
}

func TestSnapshotIsADeepCopy(t *testing.T) {
	store := team.NewMemoryStore()
	c := team.New(newFakeHost(), team.Options{Store: store})
	teammate := spawnTeammate(t, c, newFakeHost(), "alpha")

	state := c.Snapshot()
	state.Members[0].Name = "mutated"
	state.Members[0].Activity[0].Text = "mutated"
	state.Activity[0].Text = "mutated"
	state.Revision = 999
	if saved, err := store.Load(testContext(t)); err != nil || saved.Revision == 999 {
		t.Fatalf("store state was mutated through Snapshot: %+v, %v", saved, err)
	}

	again, _ := c.Teammate(teammate.ID)
	if again.Name != "alpha" || again.Activity[0].Text == "mutated" {
		t.Fatalf("coordinator state was mutated through Snapshot: %+v", again)
	}
	if rows := c.Activities(); len(rows) == 0 || rows[0].Text == "mutated" {
		t.Fatalf("Activity() was mutated through Snapshot: %+v", rows)
	}
}

func TestNameResolutionAndAmbiguity(t *testing.T) {
	h := newFakeHost()
	c := team.New(h, team.Options{})
	first := spawnTeammate(t, c, h, "alice")
	h.finish(first.ID, team.Result{Output: "ok"}, nil)
	waitFor(t, "first done", func() bool {
		got, _ := c.Teammate(first.ID)
		return got.Status == team.StatusDone
	})

	if _, err := c.Send(testContext(t), "alice", "by name"); err != nil {
		t.Fatalf("Send by name: %v", err)
	}
	spawnTeammate(t, c, h, "alice")
	if _, err := c.Send(testContext(t), "alice", "ambiguous"); !errors.Is(err, team.ErrAmbiguousMember) {
		t.Fatalf("ambiguous name = %v", err)
	}
	if _, err := c.Send(testContext(t), first.ID, "by id"); err != nil {
		t.Fatalf("Send by id: %v", err)
	}
}

func TestChangesAndWaitChange(t *testing.T) {
	h := newFakeHost()
	c := team.New(h, team.Options{})

	// Nothing published yet: the wake channel is still open.
	changes := c.Changes()
	select {
	case <-changes:
		t.Fatal("Changes() fired before any change")
	default:
	}

	teammate := spawnTeammate(t, c, h, "alpha")
	// A subscriber registered after the spawn wakes on the next change.
	changes = c.Changes()
	h.finish(teammate.ID, team.Result{Output: "done"}, nil)
	select {
	case <-changes:
	case <-time.After(2 * time.Second):
		t.Fatal("Changes() did not fire for the finished run")
	}

	// WaitChange blocks until the next change and honors its context.
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if err := c.WaitChange(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitChange with no pending change = %v", err)
	}
}

func TestCoordinatorWithoutHost(t *testing.T) {
	c := team.New(nil, team.Options{})
	ctx := testContext(t)
	if _, err := c.Spawn(ctx, team.SpawnSpec{Name: "n", Prompt: "p"}); !errors.Is(err, team.ErrNoHost) {
		t.Fatalf("Spawn without host = %v", err)
	}
	if _, err := c.Send(ctx, "x", "m"); !errors.Is(err, team.ErrNoHost) {
		t.Fatalf("Send without host = %v", err)
	}
	if _, err := c.Wait(ctx, "x"); !errors.Is(err, team.ErrNoHost) {
		t.Fatalf("Wait without host = %v", err)
	}
	if err := c.Cancel(ctx, "x"); !errors.Is(err, team.ErrNoHost) {
		t.Fatalf("Cancel without host = %v", err)
	}
	if err := c.Close(ctx, "x"); !errors.Is(err, team.ErrNoHost) {
		t.Fatalf("Close without host = %v", err)
	}
	if err := c.Reconcile(ctx); !errors.Is(err, team.ErrNoHost) {
		t.Fatalf("Reconcile without host = %v", err)
	}
	// A zero-value coordinator is inert, not a crash.
	var zero team.Coordinator
	if _, err := zero.Spawn(ctx, team.SpawnSpec{Name: "n", Prompt: "p"}); !errors.Is(err, team.ErrNoHost) {
		t.Fatalf("zero Spawn = %v", err)
	}
	if _, ok := zero.Teammate("x"); ok {
		t.Fatal("zero Teammate reported a member")
	}
	if state := zero.Snapshot(); len(state.Members) != 0 || len(state.Activity) != 0 {
		t.Fatalf("zero Snapshot = %+v", state)
	}
}

func TestSpawnRejectsEmptyHostID(t *testing.T) {
	h := newFakeHost()
	h.emptyID = true
	c := team.New(h, team.Options{})
	if _, err := c.Spawn(testContext(t), team.SpawnSpec{Name: "n", Prompt: "p"}); !errors.Is(err, team.ErrEmptyHostID) {
		t.Fatalf("empty host id = %v", err)
	}
	if members := c.Snapshot().Members; len(members) != 0 {
		t.Fatalf("empty host id registered members: %+v", members)
	}
}

func TestSendCanceledContextMarksInterrupted(t *testing.T) {
	h := newFakeHost()
	c := team.New(h, team.Options{})
	teammate := spawnTeammate(t, c, h, "alpha")
	h.finish(teammate.ID, team.Result{Output: "first"}, nil)
	waitFor(t, "first run", func() bool {
		got, _ := c.Teammate(teammate.ID)
		return got.Status == team.StatusDone
	})

	h.mu.Lock()
	h.sendFn = func(ctx context.Context, _ string, _ string) (team.Result, error) {
		<-ctx.Done()
		return team.Result{}, ctx.Err()
	}
	h.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, err := c.Send(ctx, teammate.ID, "slow"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Send with a canceled context = %v", err)
	}
	after, _ := c.Teammate(teammate.ID)
	if after.Status != team.StatusInterrupted || after.Error == "" || after.Finished.IsZero() {
		t.Fatalf("canceled send state = %+v", after)
	}
	if last := after.Activity[len(after.Activity)-1]; last.Kind != team.ActivityInterrupt {
		t.Fatalf("last activity = %+v", last)
	}
}

func TestRecordActivityAppendsTeamLevelRow(t *testing.T) {
	c := team.New(newFakeHost(), team.Options{MaxActivity: 2})
	c.RecordActivity(team.ActivityTool, "task board: team_task_create", false)
	c.RecordActivity(team.ActivityTool, "   ", false) // empty text is dropped
	rows := c.Activities()
	if len(rows) != 1 {
		t.Fatalf("activity rows = %+v", rows)
	}
	if rows[0].MemberID != "" || rows[0].Kind != team.ActivityTool || rows[0].Error {
		t.Fatalf("team-level row = %+v", rows[0])
	}
	if c.Snapshot().Revision == 0 {
		t.Fatal("RecordActivity did not publish a revision")
	}
}

type failingStore struct{ saveErr, loadErr error }

func (s failingStore) Load(context.Context) (team.TeamState, error) {
	return team.TeamState{}, s.loadErr
}

func (s failingStore) Save(context.Context, team.TeamState) error { return s.saveErr }

func TestStoreFailuresAreReported(t *testing.T) {
	saveBoom := errors.New("disk full")
	reported := make(chan error, 8)
	c := team.New(newFakeHost(), team.Options{
		Store:   failingStore{saveErr: saveBoom},
		OnError: func(err error) { reported <- err },
	})
	spawnTeammate(t, c, newFakeHost(), "alpha")
	select {
	case err := <-reported:
		if !errors.Is(err, saveBoom) {
			t.Fatalf("reported error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a failed Store.Save was not reported")
	}
	// A coordinator with a failing store still works in memory.
	if members := c.Snapshot().Members; len(members) != 1 {
		t.Fatalf("members after a failed save = %+v", members)
	}

	loadBoom := errors.New("corrupt snapshot")
	restored := team.New(newFakeHost(), team.Options{Store: failingStore{loadErr: loadBoom}})
	if err := restored.Restore(testContext(t)); !errors.Is(err, loadBoom) {
		t.Fatalf("Restore with a failing store = %v", err)
	}
}

func TestConcurrentSpawnsAndReads(t *testing.T) {
	h := newFakeHost()
	c := team.New(h, team.Options{})
	ctx := testContext(t)

	const spawns = 24
	var wg sync.WaitGroup
	ids := make(chan string, spawns)
	for index := 0; index < spawns; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			teammate, err := c.Spawn(ctx, team.SpawnSpec{
				Name: fmt.Sprintf("member-%d", index), Prompt: "p",
			})
			if err != nil {
				t.Errorf("concurrent Spawn(%d): %v", index, err)
				return
			}
			ids <- teammate.ID
		}(index)
	}
	wg.Wait()
	close(ids)

	seen := make(map[string]bool, spawns)
	for id := range ids {
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
	if len(seen) != spawns {
		t.Fatalf("spawns = %d, want %d", len(seen), spawns)
	}
	if members := c.Snapshot().Members; len(members) != spawns {
		t.Fatalf("members = %d, want %d", len(members), spawns)
	}

	var readers sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for index := 0; index < 50; index++ {
				state := c.Snapshot()
				if len(state.Members) != spawns {
					t.Errorf("snapshot members = %d", len(state.Members))
					return
				}
				_ = c.Activities()
				for id := range seen {
					if member, ok := c.Teammate(id); !ok || member.ID != id {
						t.Errorf("Teammate(%q) = %+v, %v", id, member, ok)
						return
					}
				}
			}
		}()
	}
	readers.Wait()

	// Finish every initial run before steering the team.
	for id := range seen {
		h.finish(id, team.Result{Output: "initial"}, nil)
	}
	waitFor(t, "all initial runs", func() bool {
		for _, member := range c.Snapshot().Members {
			if member.Status != team.StatusDone {
				return false
			}
		}
		return true
	})

	// Concurrent sends on distinct members all complete.
	var senders sync.WaitGroup
	for id := range seen {
		senders.Add(1)
		go func(id string) {
			defer senders.Done()
			if _, err := c.Send(ctx, id, "hello"); err != nil {
				t.Errorf("Send(%q): %v", id, err)
			}
		}(id)
	}
	senders.Wait()
	state := c.Snapshot()
	for _, member := range state.Members {
		if member.Status != team.StatusDone {
			t.Fatalf("member %q = %+v, want done", member.ID, member.Status)
		}
	}
}
