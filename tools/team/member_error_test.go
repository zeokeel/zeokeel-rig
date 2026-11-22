package team

// Tests for the retained last-run error per member: a failed initial run, Send,
// SendAsync or Wait exposes Member.Error through Members(), and the next
// successful run clears it.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func waitMember(t *testing.T, manager *Manager, id string, cond func(Member) bool) Member {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		member := findMember(t, manager, id)
		if cond(member) {
			return member
		}
		time.Sleep(time.Millisecond)
	}
	member := findMember(t, manager, id)
	t.Fatalf("member did not reach expected state: %+v", member)
	return Member{}
}

func TestMemberErrorRetainedForInitialRunAndCleared(t *testing.T) {
	runner := newFakeRunner()
	manager := NewManager(runner)
	member := mustSpawn(t, manager, "worker", "work")
	sub := runner.subagents()[0]

	sub.mu.Lock()
	sub.waitErr = errors.New("initial provider exploded")
	sub.mu.Unlock()
	if _, err := manager.Wait(testContext(t), member.ID); err == nil {
		t.Fatal("Wait on a failed initial run returned nil")
	}
	failed := findMember(t, manager, member.ID)
	if !strings.Contains(failed.Error, "initial provider exploded") {
		t.Fatalf("Member.Error = %q, want the initial run error", failed.Error)
	}
	if failed.Last != nil {
		t.Fatalf("failed run produced a Last result: %+v", failed.Last)
	}

	sub.mu.Lock()
	sub.waitErr = nil
	sub.mu.Unlock()
	if _, err := manager.Wait(testContext(t), member.ID); err != nil {
		t.Fatalf("Wait after recovery: %v", err)
	}
	recovered := findMember(t, manager, member.ID)
	if recovered.Error != "" {
		t.Fatalf("Member.Error = %q after a successful run, want empty", recovered.Error)
	}
	if recovered.Last == nil || !strings.Contains(recovered.Last.Output, "first:work") {
		t.Fatalf("recovered Last = %+v", recovered.Last)
	}
}

func TestMemberErrorRetainedForSendAndCleared(t *testing.T) {
	runner := newFakeRunner()
	manager := NewManager(runner)
	member := mustSpawn(t, manager, "worker", "work")
	sub := runner.subagents()[0]

	sub.mu.Lock()
	sub.sendErr = errors.New("send provider exploded")
	sub.mu.Unlock()
	if _, err := manager.Send(testContext(t), member.ID, "again"); err == nil {
		t.Fatal("Send failure returned nil")
	}
	failed := findMember(t, manager, member.ID)
	if !strings.Contains(failed.Error, "send provider exploded") {
		t.Fatalf("Member.Error = %q, want the send error", failed.Error)
	}

	sub.mu.Lock()
	sub.sendErr = nil
	sub.mu.Unlock()
	result, err := manager.Send(testContext(t), member.ID, "recovered")
	if err != nil {
		t.Fatalf("Send after recovery: %v", err)
	}
	if result.Output != "reply:recovered" {
		t.Fatalf("Send result = %+v", result)
	}
	recovered := findMember(t, manager, member.ID)
	if recovered.Error != "" {
		t.Fatalf("Member.Error = %q after a successful send, want empty", recovered.Error)
	}
}

func TestMemberErrorRetainedForSendAsyncAndCleared(t *testing.T) {
	runner := newFakeRunner()
	manager := NewManager(runner)
	member := mustSpawn(t, manager, "worker", "work")
	sub := runner.subagents()[0]

	sub.mu.Lock()
	sub.sendErr = errors.New("async provider exploded")
	sub.mu.Unlock()
	if err := manager.SendAsync(member.ID, "queued"); err != nil {
		t.Fatalf("SendAsync: %v", err)
	}
	failed := waitMember(t, manager, member.ID, func(current Member) bool {
		return strings.Contains(current.Error, "async provider exploded")
	})
	if failed.Error == "" {
		t.Fatal("async run error was not retained")
	}

	sub.mu.Lock()
	sub.sendErr = nil
	sub.mu.Unlock()
	if err := manager.SendAsync(member.ID, "queued again"); err != nil {
		t.Fatalf("SendAsync after recovery: %v", err)
	}
	recovered := waitMember(t, manager, member.ID, func(current Member) bool {
		return current.Error == "" && current.Last != nil &&
			strings.Contains(current.Last.Output, "reply:queued again")
	})
	if recovered.Last == nil {
		t.Fatal("successful async run did not record a result")
	}
}

func TestMemberErrorIsPerMember(t *testing.T) {
	runner := newFakeRunner()
	manager := NewManager(runner)
	first := mustSpawn(t, manager, "first", "p")
	second := mustSpawn(t, manager, "second", "p")
	runner.subagents()[0].mu.Lock()
	runner.subagents()[0].waitErr = errors.New("first failed")
	runner.subagents()[0].mu.Unlock()
	if _, err := manager.Wait(context.Background(), first.ID); err == nil {
		t.Fatal("Wait on failed member returned nil")
	}
	if got := findMember(t, manager, first.ID); got.Error == "" {
		t.Fatal("first member lost its error")
	}
	if got := findMember(t, manager, second.ID); got.Error != "" {
		t.Fatalf("second member inherited an error: %q", got.Error)
	}
}
