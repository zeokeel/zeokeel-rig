package team

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func mustSpawn(t *testing.T, manager *Manager, name, prompt string) Member {
	t.Helper()
	member, err := manager.Spawn(testContext(t), rig.SubagentSpec{Name: name, Prompt: prompt})
	if err != nil {
		t.Fatalf("Spawn(%q): %v", name, err)
	}
	return member
}

func findMember(t *testing.T, manager *Manager, id string) Member {
	t.Helper()
	for _, member := range manager.Members() {
		if member.ID == id {
			return member
		}
	}
	t.Fatalf("member %q not found in %+v", id, manager.Members())
	return Member{}
}

func TestSpawnAndList(t *testing.T) {
	runner := newFakeRunner()
	manager := NewManager(runner)

	member, err := manager.Spawn(testContext(t), rig.SubagentSpec{
		Name:         "  research  ",
		Prompt:       "  investigate  ",
		Model:        "small-model",
		SystemPrompt: "be terse",
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if member.ID != "member-1" || member.Name != "research" || member.Status != StatusActive {
		t.Fatalf("member = %+v, want member-1/research/active", member)
	}
	if member.Created.IsZero() {
		t.Fatal("member.Created is zero")
	}
	started := runner.started()
	if len(started) != 1 {
		t.Fatalf("runner starts = %d, want 1", len(started))
	}
	wantSpec := rig.SubagentSpec{Name: "research", Prompt: "investigate", Model: "small-model", SystemPrompt: "be terse"}
	if !reflect.DeepEqual(started[0], wantSpec) {
		t.Fatalf("runner spec = %+v, want %+v", started[0], wantSpec)
	}
	if runner.startCanceled {
		t.Fatal("runner.Start received a cancelled context")
	}
	if manager.Len() != 1 {
		t.Fatalf("Len = %d, want 1", manager.Len())
	}
	members := manager.Members()
	if len(members) != 1 || members[0].Name != "research" || members[0].Last != nil {
		t.Fatalf("Members = %+v", members)
	}

	// Validation.
	if _, err := manager.Spawn(testContext(t), rig.SubagentSpec{Name: " ", Prompt: "p"}); err == nil || !strings.Contains(err.Error(), "name is required") {
		t.Fatalf("Spawn(blank name) = %v", err)
	}
	if _, err := manager.Spawn(testContext(t), rig.SubagentSpec{Name: "n", Prompt: " "}); err == nil || !strings.Contains(err.Error(), "prompt is required") {
		t.Fatalf("Spawn(blank prompt) = %v", err)
	}
	if _, err := NewManager(nil).Spawn(testContext(t), rig.SubagentSpec{Name: "n", Prompt: "p"}); !errors.Is(err, ErrNoRunner) {
		t.Fatalf("Spawn(nil runner) = %v, want ErrNoRunner", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := manager.Spawn(ctx, rig.SubagentSpec{Name: "n", Prompt: "p"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Spawn(cancelled ctx) = %v, want context.Canceled", err)
	}
}

func TestWithMaxMembersAndPrefix(t *testing.T) {
	manager := NewManager(newFakeRunner(), WithMaxMembers(2), WithNamePrefix("agent-"))
	first := mustSpawn(t, manager, "a", "p")
	second := mustSpawn(t, manager, "b", "p")
	if first.ID != "agent-1" || second.ID != "agent-2" {
		t.Fatalf("ids = %q/%q, want agent-1/agent-2", first.ID, second.ID)
	}
	if _, err := manager.Spawn(testContext(t), rig.SubagentSpec{Name: "c", Prompt: "p"}); !errors.Is(err, ErrTeamFull) {
		t.Fatalf("third spawn = %v, want ErrTeamFull", err)
	}
	if err := manager.Close(first.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	third := mustSpawn(t, manager, "c", "p")
	if third.ID != "agent-3" {
		t.Fatalf("third id = %q, want agent-3", third.ID)
	}
}

func TestRunnerStartErrorLeavesNoMember(t *testing.T) {
	runner := newFakeRunner()
	runner.startErr = errors.New("provider exploded")
	manager := NewManager(runner)
	if _, err := manager.Spawn(testContext(t), rig.SubagentSpec{Name: "a", Prompt: "p"}); err == nil ||
		!strings.Contains(err.Error(), "provider exploded") {
		t.Fatalf("Spawn(runner error) = %v", err)
	}
	if got := manager.Len(); got != 0 {
		t.Fatalf("Len after failed spawn = %d, want 0", got)
	}
}

func TestResolveByIDAndName(t *testing.T) {
	manager := NewManager(newFakeRunner())
	member := mustSpawn(t, manager, "alpha", "first")

	result, err := manager.Send(testContext(t), "alpha", "by name")
	if err != nil || result.Output != "reply:by name" {
		t.Fatalf("Send(by name) = %+v, %v", result, err)
	}
	result, err = manager.Send(testContext(t), member.ID, "by id")
	if err != nil || result.Output != "reply:by id" {
		t.Fatalf("Send(by id) = %+v, %v", result, err)
	}
	if _, err := manager.Send(testContext(t), "", "m"); err == nil || !strings.Contains(err.Error(), "id or name is required") {
		t.Fatalf("Send(blank ref) = %v", err)
	}
	if _, err := manager.Send(testContext(t), "ghost", "m"); !errors.Is(err, ErrUnknownMember) {
		t.Fatalf("Send(unknown ref) = %v, want ErrUnknownMember", err)
	}
	if _, err := manager.Send(testContext(t), member.ID, "  "); err == nil || !strings.Contains(err.Error(), "message is required") {
		t.Fatalf("Send(blank message) = %v", err)
	}
}

func TestAmbiguousNameResolution(t *testing.T) {
	manager := NewManager(newFakeRunner())
	first := mustSpawn(t, manager, "worker", "one")
	second := mustSpawn(t, manager, "worker", "two")

	_, err := manager.Send(testContext(t), "worker", "hello")
	if !errors.Is(err, ErrAmbiguousMember) {
		t.Fatalf("Send(ambiguous name) = %v, want ErrAmbiguousMember", err)
	}
	for _, id := range []string{first.ID, second.ID} {
		if !strings.Contains(err.Error(), id) {
			t.Fatalf("ambiguity error %q does not list %q", err, id)
		}
	}
	if _, err := manager.Send(testContext(t), first.ID, "hello"); err != nil {
		t.Fatalf("Send(by id): %v", err)
	}
}

func TestSendWaitAndLast(t *testing.T) {
	runner := newFakeRunner()
	manager := NewManager(runner)
	member := mustSpawn(t, manager, "alpha", "first")

	first, err := manager.Wait(testContext(t), member.ID)
	if err != nil || first.Output != "first:first" {
		t.Fatalf("Wait(first run) = %+v, %v", first, err)
	}
	last, ok, err := manager.Last(member.ID)
	if err != nil || !ok || last.Output != "first:first" {
		t.Fatalf("Last after Wait = %+v, %v, %v", last, ok, err)
	}
	reply, err := manager.Send(testContext(t), "alpha", "status?")
	if err != nil || reply.Output != "reply:status?" {
		t.Fatalf("Send = %+v, %v", reply, err)
	}
	last, ok, err = manager.Last("alpha")
	if err != nil || !ok || last.Output != "reply:status?" {
		t.Fatalf("Last after Send = %+v, %v, %v", last, ok, err)
	}

	other := mustSpawn(t, manager, "beta", "first")
	if _, ok, err := manager.Last(other.ID); err != nil || ok {
		t.Fatalf("Last before any run = ok=%v, err=%v; want false, nil", ok, err)
	}
}

func TestWaitTimeout(t *testing.T) {
	runner := newFakeRunner()
	manager := NewManager(runner)
	member := mustSpawn(t, manager, "alpha", "first")
	sub := runner.subagents()[0]
	sub.mu.Lock()
	sub.waitBlock = make(chan struct{})
	sub.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if _, err := manager.Wait(ctx, member.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait(timeout) = %v, want context.DeadlineExceeded", err)
	}

	// The member is still active: a later Wait without the block returns.
	sub.mu.Lock()
	close(sub.waitBlock)
	sub.waitBlock = nil
	sub.mu.Unlock()
	result, err := manager.Wait(testContext(t), member.ID)
	if err != nil || result.Output != "first:first" {
		t.Fatalf("Wait after timeout = %+v, %v", result, err)
	}
}

func TestInterruptKeepsMember(t *testing.T) {
	runner := newFakeRunner()
	manager := NewManager(runner)
	member := mustSpawn(t, manager, "alpha", "first")

	if err := manager.Interrupt(member.ID); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	if got := findMember(t, manager, member.ID).Status; got != StatusInterrupted {
		t.Fatalf("status after Interrupt = %q, want interrupted", got)
	}
	if got := runner.subagents()[0].stopCount(); got != 1 {
		t.Fatalf("Stop calls = %d, want 1", got)
	}
	// Interrupt is idempotent and does not stop twice.
	if err := manager.Interrupt(member.ID); err != nil {
		t.Fatalf("second Interrupt: %v", err)
	}
	if got := runner.subagents()[0].stopCount(); got != 1 {
		t.Fatalf("Stop calls after second Interrupt = %d, want 1", got)
	}
	if _, err := manager.Send(testContext(t), member.ID, "hello"); !errors.Is(err, ErrMemberInactive) {
		t.Fatalf("Send to interrupted member = %v, want ErrMemberInactive", err)
	}
	// Closing removes it.
	if err := manager.Close(member.ID); err != nil {
		t.Fatalf("Close interrupted member: %v", err)
	}
	if manager.Len() != 0 {
		t.Fatalf("Len after Close = %d, want 0", manager.Len())
	}
}

func TestCloseAndUnknownMember(t *testing.T) {
	runner := newFakeRunner()
	manager := NewManager(runner)
	first := mustSpawn(t, manager, "first", "p")
	second := mustSpawn(t, manager, "second", "p")

	if err := manager.Close("first"); err != nil {
		t.Fatalf("Close(by name): %v", err)
	}
	if manager.Len() != 1 || findMember(t, manager, second.ID).Name != "second" {
		t.Fatalf("members after close = %+v", manager.Members())
	}
	if got := runner.subagents()[0].stopCount(); got != 1 {
		t.Fatalf("first subagent Stop calls = %d, want 1", got)
	}
	if err := manager.Close(first.ID); !errors.Is(err, ErrUnknownMember) {
		t.Fatalf("second Close = %v, want ErrUnknownMember", err)
	}
	if err := manager.Close(""); err == nil || !strings.Contains(err.Error(), "id or name is required") {
		t.Fatalf("Close(blank) = %v", err)
	}
}

func TestInterruptCancelsInFlightSend(t *testing.T) {
	runner := newFakeRunner()
	manager := NewManager(runner)
	member := mustSpawn(t, manager, "alpha", "first")
	sub := runner.subagents()[0]
	sub.mu.Lock()
	sub.sendBlock = make(chan struct{})
	sub.mu.Unlock()

	sendErr := make(chan error, 1)
	go func() {
		_, err := manager.Send(context.Background(), member.ID, "long task")
		sendErr <- err
	}()
	// Wait until the send is in flight before interrupting.
	select {
	case <-sub.sendGate:
	case <-time.After(2 * time.Second):
		t.Fatal("Send did not start")
	}
	if err := manager.Interrupt(member.ID); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	select {
	case err := <-sendErr:
		if err == nil || !strings.Contains(err.Error(), "send failed") {
			t.Fatalf("in-flight Send error = %v, want a readable send failure", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight Send did not return after Interrupt")
	}
	if got := findMember(t, manager, member.ID).Status; got != StatusInterrupted {
		t.Fatalf("status = %q, want interrupted", got)
	}
}

func TestCloseAll(t *testing.T) {
	runner := newFakeRunner()
	manager := NewManager(runner)
	for index := 0; index < 3; index++ {
		mustSpawn(t, manager, fmt.Sprintf("m%d", index), "p")
	}
	if err := manager.CloseAll(); err != nil {
		t.Fatalf("CloseAll: %v", err)
	}
	if manager.Len() != 0 || len(manager.Members()) != 0 {
		t.Fatalf("manager not empty after CloseAll: %+v", manager.Members())
	}
	for index, sub := range runner.subagents() {
		if got := sub.stopCount(); got != 1 {
			t.Fatalf("subagent %d Stop calls = %d, want 1", index, got)
		}
	}
	if err := manager.CloseAll(); err != nil {
		t.Fatalf("second CloseAll: %v", err)
	}
	if _, err := manager.Spawn(testContext(t), rig.SubagentSpec{Name: "late", Prompt: "p"}); !errors.Is(err, ErrManagerClosed) {
		t.Fatalf("Spawn after CloseAll = %v, want ErrManagerClosed", err)
	}
}

func TestCloseAllReportsStopErrors(t *testing.T) {
	runner := newFakeRunner()
	manager := NewManager(runner)
	mustSpawn(t, manager, "ok", "p")
	mustSpawn(t, manager, "broken", "p")
	runner.subagents()[1].mu.Lock()
	runner.subagents()[1].stopErr = errors.New("stop failed")
	runner.subagents()[1].mu.Unlock()

	err := manager.CloseAll()
	if err == nil || !strings.Contains(err.Error(), "stop failed") {
		t.Fatalf("CloseAll = %v, want the stop failure", err)
	}
	for _, sub := range runner.subagents() {
		if sub.stopCount() != 1 {
			t.Fatalf("a member was not stopped exactly once: %d", sub.stopCount())
		}
	}
}

func TestConcurrentSpawnRespectsLimit(t *testing.T) {
	manager := NewManager(newFakeRunner(), WithMaxMembers(5))
	var group sync.WaitGroup
	var mu sync.Mutex
	successes := make(map[string]bool)
	full := 0
	for index := 0; index < 20; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			member, err := manager.Spawn(context.Background(), rig.SubagentSpec{
				Name: fmt.Sprintf("m%d", index), Prompt: "p",
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if !errors.Is(err, ErrTeamFull) {
					t.Errorf("Spawn error = %v, want ErrTeamFull", err)
				}
				full++
				return
			}
			successes[member.ID] = true
		}(index)
	}
	group.Wait()
	if len(successes) != 5 {
		t.Fatalf("successful spawns = %d, want 5", len(successes))
	}
	if full != 15 {
		t.Fatalf("full errors = %d, want 15", full)
	}
	if manager.Len() != 5 {
		t.Fatalf("Len = %d, want 5", manager.Len())
	}
}

func TestConcurrentSendSerialized(t *testing.T) {
	runner := newFakeRunner()
	manager := NewManager(runner)
	member := mustSpawn(t, manager, "alpha", "first")
	sub := runner.subagents()[0]
	sub.mu.Lock()
	sub.sendDelay = 5 * time.Millisecond
	sub.mu.Unlock()

	const workers = 5
	var group sync.WaitGroup
	for index := 0; index < workers; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			if _, err := manager.Send(context.Background(), member.ID, fmt.Sprintf("msg-%d", index)); err != nil {
				t.Errorf("Send: %v", err)
			}
		}(index)
	}
	group.Wait()
	if got := sub.maxConcurrentSends(); got != 1 {
		t.Fatalf("concurrent Sends observed = %d, want 1 (serialized)", got)
	}
	if got := sub.sendCount(); got != workers {
		t.Fatalf("Send calls = %d, want %d", got, workers)
	}
}

func TestNilManager(t *testing.T) {
	var manager *Manager
	if manager.Len() != 0 || manager.Members() != nil {
		t.Fatal("nil manager reported members")
	}
	if _, err := manager.Spawn(context.Background(), rig.SubagentSpec{Name: "a", Prompt: "p"}); !errors.Is(err, ErrManagerClosed) {
		t.Fatalf("nil Spawn = %v", err)
	}
	if _, err := manager.Send(context.Background(), "member-1", "m"); !errors.Is(err, ErrManagerClosed) {
		t.Fatalf("nil Send = %v", err)
	}
	if _, _, err := manager.Last("member-1"); !errors.Is(err, ErrManagerClosed) {
		t.Fatalf("nil Last = %v", err)
	}
	if err := manager.CloseAll(); err != nil {
		t.Fatalf("nil CloseAll = %v", err)
	}
	if tools := manager.Tools(); len(tools) != 7 {
		t.Fatalf("nil Tools = %d, want 7", len(tools))
	}
}

func TestSpawnSurvivesToolContext(t *testing.T) {
	runner := newFakeRunner()
	manager := NewManager(runner)
	ctx, cancel := context.WithCancel(context.Background())
	member, err := manager.Spawn(ctx, rig.SubagentSpec{Name: "alpha", Prompt: "p"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	cancel() // the spawn call's context is gone; the child is not.
	result, err := manager.Wait(context.Background(), member.ID)
	if err != nil || result.Output != "first:p" {
		t.Fatalf("Wait after tool ctx cancellation = %+v, %v", result, err)
	}
}

func ExampleManager() {
	manager := NewManager(newFakeRunner())
	member, err := manager.Spawn(context.Background(), rig.SubagentSpec{
		Name:   "research",
		Prompt: "Find the docs",
	})
	if err != nil {
		panic(err)
	}
	result, err := manager.Wait(context.Background(), member.ID)
	if err != nil {
		panic(err)
	}
	fmt.Println(member.ID, member.Name)
	fmt.Println(result.Output)
	_ = manager.CloseAll()
	// Output:
	// member-1 research
	// first:Find the docs
}
