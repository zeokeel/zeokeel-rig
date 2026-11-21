package gate_test

// The approval queue is the shared "ask a human, wait, answer" mechanic: hosts
// (desktop, TUI, a server loop) render Pending and call Resolve; the waiting tool
// call gets the decision. Strategy stays outside.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/gate"
)

func waitPending(t *testing.T, queue *gate.Queue) gate.QueueRequest {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pending := queue.Pending(); len(pending) == 1 {
			return pending[0]
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no pending request appeared; pending = %+v", queue.Pending())
	return gate.QueueRequest{}
}

func TestQueueAskAndResolve(t *testing.T) {
	queue := gate.NewQueue()
	var answered rig.Decision
	done := make(chan struct{})
	go func() {
		defer close(done)
		decision, err := queue.Ask(context.Background(), gate.QueueRequest{Tool: "bash", Summary: "run", SessionID: "s1", Source: "user"})
		if err != nil {
			t.Errorf("Ask: %v", err)
		}
		answered = decision
	}()
	request := waitPending(t, queue)
	if request.ID == "" || request.Tool != "bash" || request.SessionID != "s1" || request.CreatedAt.IsZero() {
		t.Fatalf("pending request = %+v", request)
	}
	if err := queue.Resolve(request.ID, rig.DecisionAllowAlways); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	<-done
	if answered != rig.DecisionAllowAlways {
		t.Fatalf("decision = %q", answered)
	}
	if pending := queue.Pending(); len(pending) != 0 {
		t.Fatalf("pending after resolve = %+v", pending)
	}
	// Idempotent: answering twice is a no-op, not an error.
	if err := queue.Resolve(request.ID, rig.DecisionDeny); err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
}

func TestQueueResolveErrors(t *testing.T) {
	queue := gate.NewQueue()
	if err := queue.Resolve("missing", rig.DecisionAllow); !errors.Is(err, gate.ErrUnknownRequest) {
		t.Fatalf("unknown id error = %v", err)
	}
	if err := queue.Resolve("x", rig.Decision("maybe")); !errors.Is(err, gate.ErrInvalidDecision) {
		t.Fatalf("invalid decision error = %v", err)
	}
}

func TestQueueContextCancelRemovesPending(t *testing.T) {
	queue := gate.NewQueue()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := queue.Ask(ctx, gate.QueueRequest{Tool: "bash"})
		done <- err
	}()
	request := waitPending(t, queue)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Ask error = %v, want context.Canceled", err)
	}
	if pending := queue.Pending(); len(pending) != 0 {
		t.Fatalf("cancelled request still pending: %+v", pending)
	}
	// A cancelled request is not remembered as answered.
	if err := queue.Resolve(request.ID, rig.DecisionAllow); !errors.Is(err, gate.ErrUnknownRequest) {
		t.Fatalf("resolve after cancel = %v", err)
	}
}

func TestQueueTimeout(t *testing.T) {
	queue := gate.NewQueue()
	_, err := queue.AskTimeout(context.Background(), gate.QueueRequest{Tool: "bash"}, 20*time.Millisecond)
	if !errors.Is(err, gate.ErrTimeout) {
		t.Fatalf("AskTimeout error = %v, want ErrTimeout", err)
	}
	if pending := queue.Pending(); len(pending) != 0 {
		t.Fatalf("timed-out request still pending: %+v", pending)
	}
}

func TestQueueEnqueueHookAndSubscribers(t *testing.T) {
	enqueued := make(chan gate.QueueRequest, 1)
	notifications := make(chan struct{}, 8)
	queue := gate.NewQueue(gate.WithQueuePrefix("appr-"), gate.WithQueueEnqueueHook(func(request gate.QueueRequest) {
		enqueued <- request
	}))
	unsubscribe, err := queue.Subscribe(func() { notifications <- struct{}{} })
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = queue.Ask(context.Background(), gate.QueueRequest{Tool: "bash"})
	}()
	request := <-enqueued
	if request.ID[:5] != "appr-" {
		t.Fatalf("request id = %q, want the appr- prefix", request.ID)
	}
	pending := waitPending(t, queue)
	if err := queue.Resolve(pending.ID, rig.DecisionDeny); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	<-done
	select {
	case <-notifications:
	default:
		t.Fatal("subscriber was never notified")
	}
	unsubscribe()
	// The callback is contained even if it panics.
	panicQueue := gate.NewQueue()
	panicked := false
	if _, err := panicQueue.Subscribe(func() { panicked = true; panic("boom") }); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	go func() { _, _ = panicQueue.Ask(context.Background(), gate.QueueRequest{Tool: "bash"}) }()
	waitPending(t, panicQueue)
	for index := range panicQueue.Pending() {
		_ = index
	}
	if !panicked {
		t.Fatal("subscriber did not run")
	}
}

func TestQueuePendingKeepsEnqueueOrder(t *testing.T) {
	queue := gate.NewQueue()
	for _, tool := range []string{"a", "b", "c"} {
		tool := tool
		go func() { _, _ = queue.Ask(context.Background(), gate.QueueRequest{Tool: tool}) }()
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(queue.Pending()) < 3 {
		time.Sleep(time.Millisecond)
	}
	pending := queue.Pending()
	if len(pending) != 3 {
		t.Fatalf("pending = %+v, want 3", pending)
	}
	for _, request := range pending {
		if err := queue.Resolve(request.ID, rig.DecisionAllow); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
	}
}
